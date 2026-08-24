package render

import (
	"strings"
	"testing"
)

func spec() StackSpec {
	return StackSpec{
		Dir: "/var/lib/yunirun-obs", ConfDir: "/etc/yunirun",
		PrometheusImage: "p", LokiImage: "l", AlloyImage: "a", GrafanaImage: "g",
		Retention: "30d",
	}
}

// 取り込み対象は HAProxy の exporter。そこに全アプリの応答が集まっているので、
// アプリ側に計装を足さずに Rate・Errors・Duration が揃う。
func TestPrometheusScrapesTheProxyWhereEveryAppsTrafficPasses(t *testing.T) {
	got := spec().PrometheusConfig()
	if !strings.Contains(got, "127.0.0.1:8098") {
		t.Fatalf("HAProxy の exporter を見ていない:\n%s", got)
	}
}

// どれも外へ開かない。到達経路は ssh のポート転送だけにする。
func TestNothingInTheStackListensOutsideLoopback(t *testing.T) {
	s := spec()
	for name, body := range s.StackFiles() {
		for _, line := range strings.Split(body, "\n") {
			l := strings.TrimSpace(line)
			// listen 系の指定に 0.0.0.0 が現れたら外へ開いている。
			if strings.Contains(l, "0.0.0.0") {
				t.Fatalf("%s が外へ開いている: %s", name, l)
			}
		}
	}
	if !strings.Contains(s.StackUnits()["yunirun-grafana.container"],
		"GF_SERVER_HTTP_ADDR=127.0.0.1") {
		t.Fatal("Grafana が 127.0.0.1 に絞られていない")
	}
	if !strings.Contains(s.StackUnits()["yunirun-prometheus.container"],
		"--web.listen-address=127.0.0.1:8091") {
		t.Fatal("Prometheus が 127.0.0.1 に絞られていない")
	}
}

// bind mount の元は root 所有で、各 image は別々の非 root ユーザで動く。
// :U が無いと書き込みが Permission denied になる。
func TestStackDataVolumesAreChownedToTheContainerUser(t *testing.T) {
	for name, unit := range spec().StackUnits() {
		for _, line := range strings.Split(unit, "\n") {
			if !strings.HasPrefix(line, "Volume=/var/lib/yunirun-obs/") {
				continue
			}
			if !strings.HasSuffix(line, ":U") {
				t.Fatalf("%s のデータ領域に :U が無い: %s", name, line)
			}
		}
	}
}

// journal は root しか読めない。image の既定ユーザのままだと開けず、ログが
// 1 行も入らないまま「動いている」ように見える。
func TestAlloyRunsAsRootBecauseTheJournalIsRootOnly(t *testing.T) {
	got := spec().StackUnits()["yunirun-alloy.container"]
	if !strings.Contains(got, "User=0:0") {
		t.Fatalf("root で動かしていない:\n%s", got)
	}
	if !strings.Contains(got, "Volume=/var/log/journal:/var/log/journal:ro") {
		t.Fatalf("journal を渡していない:\n%s", got)
	}
}

// 保持期間は 1 か所の指定で両方に効いてほしい。片方だけ伸ばすと、ログは
// あるのにメトリクスが無いといった噛み合わない状態になる。
func TestRetentionAppliesToBothMetricsAndLogs(t *testing.T) {
	s := spec()
	s.Retention = "7d"
	if !strings.Contains(s.LokiConfig(), "retention_period: 7d") {
		t.Fatal("ログ側に効いていない")
	}
	if !strings.Contains(s.StackUnits()["yunirun-prometheus.container"],
		"--storage.tsdb.retention.time=7d") {
		t.Fatal("メトリクス側に効いていない")
	}
}

// アプリは rootless のユーザ unit で動く。system unit のフィールドしか見ないと、
// 全アプリのログが user@<uid>.service という 1 つの塊になり、アプリごとに
// 引けない。rootless の podman は CONTAINER_NAME も載せない。
func TestAlloyLabelsRootlessAppLogsPerApp(t *testing.T) {
	got := spec().AlloyConfig()
	if !strings.Contains(got, "__journal__systemd_user_unit") {
		t.Fatalf("ユーザ unit を見ていない:\n%s", got)
	}
	// 空で上書きするとラベルごと消える。どの規則にも regex の番人が要る。
	n := strings.Count(got, `source_labels`)
	if g := strings.Count(got, `regex         = "(.+)`); g != n {
		t.Fatalf("regex の番人が足りない: source_labels=%d regex=%d\n%s", n, g, got)
	}
}

// システム unit より後に置かないと、アプリのログが user@<uid>.service のまま残る。
func TestAlloyPrefersTheUserUnitOverTheSystemUnit(t *testing.T) {
	got := spec().AlloyConfig()
	sys := strings.Index(got, "__journal__systemd_unit")
	usr := strings.Index(got, "__journal__systemd_user_unit")
	if sys < 0 || usr < 0 || usr < sys {
		t.Fatalf("ユーザ unit がシステム unit より前にある:\n%s", got)
	}
}

// 全系が落ちたときに発火しないと意味が無い。
//
// haproxy_server_status{state="UP"} == 1 と絞ると、全系が落ちた瞬間に系列
// そのものが消え、閾値の比較対象が無くなる。実機で確認した (絞ると
// template_out が消え、絞らなければ 0 が残った)。
func TestAlertsDoNotFilterAwayTheVerySituationTheyWatchFor(t *testing.T) {
	got := spec().GrafanaAlerting()
	if strings.Contains(got, `state=\"UP\"} == 1`) {
		t.Fatalf("全系ダウンで系列ごと消える書き方をしている:\n%s", got)
	}
	if !strings.Contains(got, "yunirun-origin-down") {
		t.Fatalf("オリジン停止の規則が無い:\n%s", got)
	}
}

// 計測が届かなくなったこと自体を知りたい。NoData のまま黙られると、
// 落ちているのか見えていないのかが分からない。
func TestLosingSightIsItselfAnAlert(t *testing.T) {
	got := spec().GrafanaAlerting()
	i := strings.Index(got, "yunirun-metrics-blind")
	if i < 0 {
		t.Fatalf("計測断の規則が無い:\n%s", got)
	}
	if !strings.Contains(got[i:], "noDataState: Alerting") {
		t.Fatalf("データが無いときに黙ってしまう:\n%s", got[i:])
	}
}

// 規則はデータソースを名前で指す。自動採番だと参照できない。
func TestAlertsCanReferenceTheDatasource(t *testing.T) {
	s := spec()
	if !strings.Contains(s.GrafanaDatasources(), "uid: "+PrometheusUID) {
		t.Fatal("データソースの uid が固定されていない")
	}
	if !strings.Contains(s.GrafanaAlerting(), "datasourceUid: "+PrometheusUID) {
		t.Fatal("規則がデータソースを指していない")
	}
}

// 送り先が無いのに空の url を書くと、Grafana が起動時に取り込みごと失敗する。
func TestNoContactPointFileWithoutADestination(t *testing.T) {
	s := spec()
	if _, ok := s.StackFiles()["grafana-contactpoints.yaml"]; ok {
		t.Fatal("送り先が無いのに送り先の設定を書いている")
	}
	if strings.Contains(s.StackUnits()["yunirun-grafana.container"], "contactpoints") {
		t.Fatal("無いファイルを mount しようとしている")
	}
	s.AlertWebhook = "http://127.0.0.1:5678/webhook/x"
	if _, ok := s.StackFiles()["grafana-contactpoints.yaml"]; !ok {
		t.Fatal("送り先を指定したのに設定が出ていない")
	}
	if !strings.Contains(s.StackUnits()["yunirun-grafana.container"], "contactpoints") {
		t.Fatal("送り先の設定が Grafana に渡っていない")
	}
}

// uid を後から固定したとき、既に別の uid で登録されていると Grafana は
// "Datasource provisioning error: data source not found" で起動そのものに
// 失敗する。実機で踏んだ。消してから入れ直せばこの経路を通らない。
func TestDatasourcesAreReplacedSoPinningTheUidCannotBreakStartup(t *testing.T) {
	got := spec().GrafanaDatasources()
	del := strings.Index(got, "deleteDatasources:")
	add := strings.Index(got, "\ndatasources:")
	if del < 0 {
		t.Fatalf("入れ直していない:\n%s", got)
	}
	if add < del {
		t.Fatalf("消すより先に入れている:\n%s", got)
	}
	for _, n := range []string{"Prometheus", "Loki"} {
		if !strings.Contains(got[del:add], "name: "+n) {
			t.Fatalf("%s を消していない:\n%s", n, got)
		}
	}
}
