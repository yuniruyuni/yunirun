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
