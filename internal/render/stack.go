package render

// 計測基盤。yunirun が管理する付帯コンテナとして立てる。
//
// アプリと違ってリポジトリから deploy されるものではないので、HAProxy と
// 同じく converge が unit と設定の両方を生成する。
//
// トレースは入れていない。いまのところ OTLP を吐くアプリが 1 つも無く、
// 置いても空の UI が増えるだけになる。アプリを計装したら Tempo を 1 つ
// 足すだけで繋がる形にしてある (Grafana と収集経路は共通)。
//
// ホストの資源 (CPU・メモリ・ディスク) は既に Mackerel が見ているので、
// node exporter は置かない。ここで足すのは「アプリがどう応答しているか」で、
// それは HAProxy が既に知っている。

import (
	"fmt"
	"strings"
)

// 付帯コンテナが使うポート。アプリの割り当て帯 (basePort 以降) の外に取る。
const (
	GrafanaPort    = 8090
	PrometheusPort = 8091
	LokiPort       = 8092
	AlloyPort      = 8093
)

// StackSpec は計測基盤を組み立てるのに要るもの。
type StackSpec struct {
	// Dir はデータの置き場所。中に各コンポーネントのディレクトリを作る。
	Dir string
	// ConfDir は生成した設定の置き場所。
	ConfDir string

	PrometheusImage string
	LokiImage       string
	AlloyImage      string
	GrafanaImage    string

	// Retention は保持期間。metrics と logs の両方に使う。
	Retention string
}

// PrometheusConfig は取り込み対象を書き出す。
//
// 対象は HAProxy の exporter だけでよい。HAProxy は全アプリの経路を担って
// いるので、そこに全アプリの Rate・Errors・Duration が揃っている。
// アプリ側に計装を足す必要が無いのが利点。
func (s StackSpec) PrometheusConfig() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("global:")
	p("  scrape_interval: 15s")
	p("scrape_configs:")
	p("  - job_name: haproxy")
	p("    static_configs:")
	p("      - targets: ['127.0.0.1:%d']", MetricsPort)
	// 自分自身も見る。取り込みが止まっていることに気付けるようにする。
	p("  - job_name: prometheus")
	p("    static_configs:")
	p("      - targets: ['127.0.0.1:%d']", PrometheusPort)
	return b.String()
}

// LokiConfig は単体構成の Loki 設定を書き出す。
//
// 分散させる理由が無いので、すべて 1 プロセスに寄せてファイルシステムに置く。
func (s StackSpec) LokiConfig() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("auth_enabled: false")
	p("server:")
	p("  http_listen_address: 127.0.0.1")
	p("  http_listen_port: %d", LokiPort)
	p("  grpc_listen_address: 127.0.0.1")
	p("  grpc_listen_port: 9096")
	// 既定は info で、取り込みのたびに 1 行出る。量が多いので落とす。
	p("  log_level: warn")
	p("common:")
	p("  instance_addr: 127.0.0.1")
	p("  path_prefix: /loki")
	p("  storage:")
	p("    filesystem:")
	p("      chunks_directory: /loki/chunks")
	p("      rules_directory: /loki/rules")
	p("  replication_factor: 1")
	p("  ring:")
	p("    kvstore:")
	p("      store: inmemory")
	p("schema_config:")
	p("  configs:")
	p("    - from: 2024-01-01")
	p("      store: tsdb")
	p("      object_store: filesystem")
	p("      schema: v13")
	p("      index:")
	p("        prefix: index_")
	p("        period: 24h")
	p("limits_config:")
	p("  retention_period: %s", s.Retention)
	p("  allow_structured_metadata: true")
	p("compactor:")
	p("  working_directory: /loki/compactor")
	p("  retention_enabled: true")
	p("  delete_request_store: filesystem")
	// 外へ送らない。
	p("analytics:")
	p("  reporting_enabled: false")
	return b.String()
}

// AlloyConfig は journald を読んで Loki へ送る設定を書き出す。
//
// journald を選ぶのは、podman が全コンテナのログをそこへ流しているため。
// アプリ・DB・HAProxy をまとめて 1 か所で拾える。アプリ側に何も足さずに
// 済むのが利点。
func (s StackSpec) AlloyConfig() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p(`loki.relabel "journal" {`)
	p(`  forward_to = []`)
	// どの unit から出たログかが分からないと、アプリごとに絞り込めない。
	//
	// どの規則にも regex = "(.+)" を付ける。元が空のとき規則ごと飛ばさせる
	// ためで、付けないと空の値で上書きしてラベルが消える。
	p(`  rule {`)
	p(`    source_labels = ["__journal__systemd_unit"]`)
	p(`    regex         = "(.+)"`)
	p(`    target_label  = "unit"`)
	p(`  }`)
	// アプリは rootless のユーザ unit で動く。こちらを見ないと、全アプリの
	// ログが user@<uid>.service という 1 つの塊になり、アプリごとに引けない。
	// システム unit より後に置いて上書きさせる。
	p(`  rule {`)
	p(`    source_labels = ["__journal__systemd_user_unit"]`)
	p(`    regex         = "(.+)"`)
	p(`    target_label  = "unit"`)
	p(`  }`)
	// container 名で引けるようにする。rootful の podman はこれを journal に
	// 載せるが、rootless は載せないので、ユーザ unit 名から作る。
	p(`  rule {`)
	p(`    source_labels = ["__journal_container_name"]`)
	p(`    regex         = "(.+)"`)
	p(`    target_label  = "container"`)
	p(`  }`)
	p(`  rule {`)
	p(`    source_labels = ["__journal__systemd_user_unit"]`)
	p(`    regex         = "(.+)\\.service"`)
	p(`    target_label  = "container"`)
	p(`  }`)
	p(`}`)
	p(``)
	p(`loki.source.journal "read" {`)
	p(`  forward_to    = [loki.write.local.receiver]`)
	p(`  relabel_rules = loki.relabel.journal.rules`)
	p(`  labels        = {job = "journal"}`)
	// 起動のたびに全履歴を読み直すと、再起動が重複の原因になる。
	p(`  max_age       = "12h"`)
	p(`}`)
	p(``)
	p(`loki.write "local" {`)
	p(`  endpoint {`)
	p(`    url = "http://127.0.0.1:%d/loki/api/v1/push"`, LokiPort)
	p(`  }`)
	p(`}`)
	return b.String()
}

// GrafanaDatasources は接続先を書き出す。
//
// 手で足させない。UI で足したものは Grafana の DB にしか残らず、作り直すと
// 消える。
func (s StackSpec) GrafanaDatasources() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("apiVersion: 1")
	p("datasources:")
	p("  - name: Prometheus")
	p("    type: prometheus")
	p("    access: proxy")
	p("    url: http://127.0.0.1:%d", PrometheusPort)
	p("    isDefault: true")
	p("  - name: Loki")
	p("    type: loki")
	p("    access: proxy")
	p("    url: http://127.0.0.1:%d", LokiPort)
	return b.String()
}

// stackUnit は付帯コンテナ 1 つ分の Quadlet unit を組み立てる。
//
// どれも Network=host にする。互いに 127.0.0.1 で繋ぎ、HAProxy の exporter も
// 127.0.0.1 にあるため。外へは開かない (各プロセスに 127.0.0.1 を bind させる)。
func stackUnit(name, desc, image string, opts []string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("[Unit]")
	p("Description=yunirun: %s", desc)
	p("")
	p("[Container]")
	p("ContainerName=%s", name)
	p("Image=%s", image)
	p("Network=host")
	for _, o := range opts {
		p("%s", o)
	}
	p("")
	p("[Service]")
	p("Restart=always")
	p("RestartSec=10")
	p("")
	p("[Install]")
	p("WantedBy=multi-user.target")
	return b.String()
}

// StackUnits は付帯コンテナの unit を名前つきで返す。
//
// 中身が書けないと起動しないので、呼ぶ側は設定を先に置くこと。
func (s StackSpec) StackUnits() map[string]string {
	// :U を付けるのは、bind mount の元が root 所有で、各 image が別々の
	// 非 root ユーザで動くため。付けないと書き込みが Permission denied になる。
	return map[string]string{
		"yunirun-prometheus.container": stackUnit(
			"yunirun-prometheus", "Prometheus", s.PrometheusImage, []string{
				fmt.Sprintf("Volume=%s/prometheus.yml:/etc/prometheus/prometheus.yml:ro", s.ConfDir),
				fmt.Sprintf("Volume=%s/prometheus:/prometheus:U", s.Dir),
				fmt.Sprintf("Exec=--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=%s --web.listen-address=127.0.0.1:%d", s.Retention, PrometheusPort),
			}),

		"yunirun-loki.container": stackUnit(
			"yunirun-loki", "Loki", s.LokiImage, []string{
				fmt.Sprintf("Volume=%s/loki.yaml:/etc/loki/local-config.yaml:ro", s.ConfDir),
				fmt.Sprintf("Volume=%s/loki:/loki:U", s.Dir),
				"Exec=-config.file=/etc/loki/local-config.yaml",
			}),

		"yunirun-alloy.container": stackUnit(
			"yunirun-alloy", "Alloy (ログ収集)", s.AlloyImage, []string{
				fmt.Sprintf("Volume=%s/alloy.alloy:/etc/alloy/config.alloy:ro", s.ConfDir),
				fmt.Sprintf("Volume=%s/alloy:/var/lib/alloy:U", s.Dir),
				// journald を読む。machine-id が無いと journal を開けない。
				"Volume=/var/log/journal:/var/log/journal:ro",
				"Volume=/etc/machine-id:/etc/machine-id:ro",
				// journal は root しか読めない。image の既定ユーザでは開けない。
				"User=0:0",
				fmt.Sprintf("Exec=run /etc/alloy/config.alloy --storage.path=/var/lib/alloy --server.http.listen-addr=127.0.0.1:%d", AlloyPort),
			}),

		"yunirun-grafana.container": stackUnit(
			"yunirun-grafana", "Grafana", s.GrafanaImage, []string{
				fmt.Sprintf("Volume=%s/grafana-datasources.yaml:/etc/grafana/provisioning/datasources/yunirun.yaml:ro", s.ConfDir),
				fmt.Sprintf("Volume=%s/grafana:/var/lib/grafana:U", s.Dir),
				// 127.0.0.1 にだけ bind する。外からは ssh のポート転送で見る。
				"Environment=GF_SERVER_HTTP_ADDR=127.0.0.1",
				fmt.Sprintf("Environment=GF_SERVER_HTTP_PORT=%d", GrafanaPort),
				// 到達できる時点でホストに入れているので、そこで改めて
				// 認証させる意味が無い。認証を足すのは外へ開くときに。
				"Environment=GF_AUTH_ANONYMOUS_ENABLED=true",
				"Environment=GF_AUTH_ANONYMOUS_ORG_ROLE=Admin",
				"Environment=GF_AUTH_BASIC_ENABLED=false",
				// 外へ送らない。
				"Environment=GF_ANALYTICS_REPORTING_ENABLED=false",
				"Environment=GF_ANALYTICS_CHECK_FOR_UPDATES=false",
			}),
	}
}

// StackFiles は生成する設定ファイルを名前つきで返す。
func (s StackSpec) StackFiles() map[string]string {
	return map[string]string{
		"prometheus.yml":           s.PrometheusConfig(),
		"loki.yaml":                s.LokiConfig(),
		"alloy.alloy":              s.AlloyConfig(),
		"grafana-datasources.yaml": s.GrafanaDatasources(),
	}
}
