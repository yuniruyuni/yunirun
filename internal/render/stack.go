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

	// AlertWebhook はアラートの送り先。空なら送り先を作らない
	// (規則は入るので Grafana の画面では発火が見える)。
	AlertWebhook string
}

// データソースの uid は固定する。アラート規則がここを名前で指すので、
// Grafana に自動採番させると規則側から参照できない。
const (
	PrometheusUID = "yunirun-prometheus"
	LokiUID       = "yunirun-loki"
)

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
	p("    uid: %s", PrometheusUID)
	p("    type: prometheus")
	p("    access: proxy")
	p("    url: http://127.0.0.1:%d", PrometheusPort)
	p("    isDefault: true")
	p("  - name: Loki")
	p("    uid: %s", LokiUID)
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
	// 送り先が無いときは mount しない。無いファイルを指すと Quadlet が
	// RequiresMountsFor でその unit ごと起動できなくなる。
	grafana := []string{
		fmt.Sprintf("Volume=%s/grafana-datasources.yaml:/etc/grafana/provisioning/datasources/yunirun.yaml:ro", s.ConfDir),
		fmt.Sprintf("Volume=%s/grafana-alerting.yaml:/etc/grafana/provisioning/alerting/rules.yaml:ro", s.ConfDir),
	}
	if s.AlertWebhook != "" {
		grafana = append(grafana,
			fmt.Sprintf("Volume=%s/grafana-contactpoints.yaml:/etc/grafana/provisioning/alerting/contactpoints.yaml:ro", s.ConfDir))
	}
	grafana = append(grafana,
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
	)

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
			"yunirun-grafana", "Grafana", s.GrafanaImage, grafana),
	}
}

// StackFiles は生成する設定ファイルを名前つきで返す。
func (s StackSpec) StackFiles() map[string]string {
	out := map[string]string{
		"prometheus.yml":           s.PrometheusConfig(),
		"loki.yaml":                s.LokiConfig(),
		"alloy.alloy":              s.AlloyConfig(),
		"grafana-datasources.yaml": s.GrafanaDatasources(),
		"grafana-alerting.yaml":    s.GrafanaAlerting(),
	}
	// 送り先が無いときは書かない。空の url を入れると Grafana が起動時に
	// 設定の取り込みごと失敗する。
	if s.AlertWebhook != "" {
		out["grafana-contactpoints.yaml"] = s.GrafanaContactPoints()
	}
	return out
}

// alertRule はアラート 1 件分。
type alertRule struct {
	UID, Title, Expr, Op, Threshold, For, Severity, Summary, NoData string
}

// alertRules は見張る内容。
//
// どれも「値をそのまま足す」形で書く。haproxy_server_status{state="UP"} == 1 と
// 絞ると、全系が落ちたときに系列そのものが消えて閾値の比較対象が無くなり、
// 最も必要な瞬間に発火しない。実機で確認した (絞ると template_out が消え、
// 絞らなければ 0 が残った)。
func alertRules() []alertRule {
	return []alertRule{
		{
			UID: "yunirun-origin-down", Title: "オリジンが落ちている",
			Expr: `sum by (proxy) (haproxy_server_status{state="UP"})`,
			Op:   "lt", Threshold: "1", For: "2m", Severity: "critical",
			Summary: "{{ $labels.proxy }} は生きている系が 1 つも無い",
			NoData:  "NoData",
		},
		{
			UID: "yunirun-replica-down", Title: "片系だけで動いている",
			Expr: `sum by (proxy) (haproxy_server_status{state="UP"})`,
			// デプロイ中は片系が入れ替わるので、そこでは鳴らさない。
			Op: "lt", Threshold: "2", For: "15m", Severity: "warning",
			Summary: "{{ $labels.proxy }} は片系だけで動いている",
			NoData:  "NoData",
		},
		{
			UID: "yunirun-backend-errors", Title: "5xx を返している",
			Expr: `sum by (proxy) (rate(haproxy_backend_http_responses_total{code="5xx"}[5m]))`,
			Op:   "gt", Threshold: "0", For: "5m", Severity: "warning",
			Summary: "{{ $labels.proxy }} が 5xx を返している",
			NoData:  "NoData",
		},
		{
			UID: "yunirun-metrics-blind", Title: "計測が届いていない",
			Expr: `up{job="haproxy"}`,
			Op:   "lt", Threshold: "1", For: "5m", Severity: "critical",
			// 見えないこと自体が問題なので、データが無い場合も鳴らす。
			Summary: "HAProxy の計測が取れていない (落ちているかどうかも分からない)",
			NoData:  "Alerting",
		},
	}
}

// GrafanaAlerting はアラート規則を書き出す。
func (s StackSpec) GrafanaAlerting() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("apiVersion: 1")
	p("groups:")
	p("  - orgId: 1")
	p("    name: yunirun")
	p("    folder: yunirun")
	p("    interval: 1m")
	p("    rules:")
	for _, r := range alertRules() {
		p("      - uid: %s", r.UID)
		p("        title: %q", r.Title)
		// B は閾値の判定。A の結果をこれで見る。
		p("        condition: B")
		p("        for: %s", r.For)
		p("        noDataState: %s", r.NoData)
		p("        execErrState: Alerting")
		p("        labels:")
		p("          severity: %s", r.Severity)
		p("        annotations:")
		p("          summary: %q", r.Summary)
		p("        data:")
		p("          - refId: A")
		p("            relativeTimeRange:")
		p("              from: 300")
		p("              to: 0")
		p("            datasourceUid: %s", PrometheusUID)
		p("            model:")
		p("              refId: A")
		p("              instant: true")
		p("              range: false")
		p("              editorMode: code")
		p("              expr: %q", r.Expr)
		p("          - refId: B")
		p("            datasourceUid: __expr__")
		p("            model:")
		p("              refId: B")
		p("              type: threshold")
		p("              expression: A")
		p("              conditions:")
		p("                - evaluator:")
		p("                    type: %s", r.Op)
		p("                    params: [%s]", r.Threshold)
	}
	return b.String()
}

// GrafanaContactPoints は送り先と振り分けを書き出す。
//
// 送り先を 1 つの webhook に寄せるのは、その先 (Discord なのかメールなのか) を
// yunirun が知らずに済むようにするため。変えるときにこちらを触らなくてよい。
func (s StackSpec) GrafanaContactPoints() string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("apiVersion: 1")
	p("contactPoints:")
	p("  - orgId: 1")
	p("    name: yunirun")
	p("    receivers:")
	p("      - uid: yunirun-webhook")
	p("        type: webhook")
	p("        settings:")
	p("          url: %q", s.AlertWebhook)
	p("          httpMethod: POST")
	p("policies:")
	p("  - orgId: 1")
	p("    receiver: yunirun")
	p("    group_by: [alertname, proxy]")
	p("    group_wait: 30s")
	p("    group_interval: 5m")
	// 直っていないものを鳴らし続けない。半日ごとに思い出させる程度にする。
	p("    repeat_interval: 12h")
	return b.String()
}
