package render

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/manifest"
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
// 書き込む領域に :U が無いと Permission denied になる。
//
// 読むだけの場所は対象外。むしろ :U を付けてはいけない。所有者を書き手から
// 奪うことになる (指標の置き場は root が書いて node exporter が読む)。
//
// トレースの口も対象外。あちらはアプリのグループで共有するので、:U で
// Tempo の uid に付け替えると他が繋げなくなる。
func TestStackDataVolumesAreChownedToTheContainerUser(t *testing.T) {
	sock := spec().TraceSocketDir()
	for name, unit := range spec().StackUnits() {
		for _, line := range strings.Split(unit, "\n") {
			if !strings.HasPrefix(line, "Volume=/var/lib/yunirun-obs/") {
				continue
			}
			if strings.HasPrefix(line, "Volume="+sock+":") {
				continue
			}
			if strings.HasSuffix(line, ":ro") {
				continue
			}
			if !strings.HasSuffix(line, ":U") {
				t.Fatalf("%s のデータ領域に :U が無い: %s", name, line)
			}
		}
	}
}

// 読むだけの場所を :U にすると、所有者を書き手から奪ってしまう。
func TestReadOnlyStackVolumesAreNotChowned(t *testing.T) {
	for name, unit := range spec().StackUnits() {
		for _, line := range strings.Split(unit, "\n") {
			if !strings.HasPrefix(line, "Volume=") {
				continue
			}
			if strings.Contains(line, ":ro") && strings.Contains(line, ":U") {
				t.Errorf("%s が読むだけの場所を chown している: %s", name, line)
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

// ダッシュボードは JSON として成立していないと Grafana が黙って読み飛ばす。
func TestDashboardsAreValidJSONWithPanels(t *testing.T) {
	for name, body := range spec().StackDashboards() {
		var d struct {
			UID    string           `json:"uid"`
			Title  string           `json:"title"`
			Panels []map[string]any `json:"panels"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("%s が JSON になっていない: %v", name, err)
		}
		if d.UID == "" || d.Title == "" || len(d.Panels) == 0 {
			t.Fatalf("%s の中身が足りない: uid=%q title=%q panels=%d", name, d.UID, d.Title, len(d.Panels))
		}
	}
}

// USE は資源の計測が無いと成立しない。node exporter を取り込んでいないと、
// 板だけあって中身が空になる。
func TestUseNeedsTheResourceMetricsToBeScraped(t *testing.T) {
	if !strings.Contains(spec().PrometheusConfig(), "job_name: node") {
		t.Fatal("ホストの資源を取り込んでいない")
	}
	if !strings.Contains(spec().StackUnits()["yunirun-node.container"], "--path.rootfs=/host/root") {
		t.Fatal("ホスト側の root を見せていない (コンテナ自身の資源を報告してしまう)")
	}
}

// exprs はダッシュボードに現れる式をすべて集める。
//
// JSON の中では引用符が退避されるので、文字列として探すと取りこぼす。
func exprs(t *testing.T, body string) string {
	t.Helper()
	var d struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			b.WriteString(tg.Expr)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RED の 3 つが揃っていること。どれか欠けると方法として成立しない。
func TestRedCoversRateErrorsDuration(t *testing.T) {
	got := exprs(t, REDDashboard())
	for _, want := range []string{
		"rate(haproxy_backend_http_responses_total[5m])",   // Rate
		`haproxy_backend_http_responses_total{code="5xx"}`, // Errors
		"haproxy_backend_response_time_average_seconds",    // Duration
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("RED に %s が無い:\n%s", want, got)
		}
	}
}

// USE の 3 つが資源ごとに揃っていること。飽和は PSI で見る。
func TestUseCoversUtilizationSaturationErrors(t *testing.T) {
	got := exprs(t, USEDashboard())
	for _, want := range []string{
		`node_cpu_seconds_total{mode="idle"}`,        // CPU 使用率
		"node_pressure_cpu_waiting_seconds_total",    // CPU 飽和
		"node_memory_MemAvailable_bytes",             // メモリ使用率
		"node_pressure_memory_waiting_seconds_total", // メモリ飽和
		"node_disk_io_time_seconds_total",            // ディスク使用率
		"node_pressure_io_waiting_seconds_total",     // ディスク飽和
		"node_network_receive_errs_total",            // ネットワーク誤り
		"node_network_receive_drop_total",            // ネットワーク飽和
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("USE に %s が無い:\n%s", want, got)
		}
	}
}

// HAProxy は入れ直しの対象にしない。USR2 で読み直せるので、繋ぎっぱなしの
// 接続を切ってまで入れ直す理由が無い。
func TestHAProxyIsReloadedNotRestarted(t *testing.T) {
	in := spec().StackInputs("/etc/yunirun")
	if _, ok := in["yunirun-haproxy.container"]; ok {
		t.Fatal("HAProxy を入れ直しの対象にしている")
	}
}

// 設定を渡している unit は、その設定を見張っていないと取りこぼす。
func TestEveryStackUnitWatchesTheConfigItIsGiven(t *testing.T) {
	s := spec()
	in := s.StackInputs("/etc/yunirun")
	for name, unit := range s.StackUnits() {
		watched, ok := in[name]
		if !ok {
			t.Fatalf("%s の見張り対象が定義されていない", name)
		}
		for _, line := range strings.Split(unit, "\n") {
			v, isVol := strings.CutPrefix(line, "Volume=/etc/yunirun/")
			if !isVol {
				continue
			}
			src := "/etc/yunirun/" + strings.SplitN(v, ":", 2)[0]
			// ディレクトリを渡しているものは中身が動き続けるので対象外。
			if !strings.HasSuffix(src, ".yml") && !strings.HasSuffix(src, ".yaml") &&
				!strings.HasSuffix(src, ".alloy") && !strings.HasSuffix(src, ".cfg") {
				continue
			}
			if !slices.Contains(watched, src) {
				t.Fatalf("%s は %s を渡しているのに見張っていない", name, src)
			}
		}
	}
}

// トレースの口には認証が無い。誰でも繋げると偽のスパンを流し込める。
// DB のソケットは誰でも繋げるが、あちらは PostgreSQL がパスワードで認証する。
func TestTraceSocketIsSharedByGroupNotByEveryone(t *testing.T) {
	u := spec().StackUnits()["yunirun-tempo.container"]
	if strings.Contains(u, spec().TraceSocketDir()+":"+"/run/tempo:U") {
		t.Fatal("ソケット領域を Tempo の uid に付け替えている (他が繋げなくなる)")
	}
	if TraceGroup == "" {
		t.Fatal("共有するグループが決まっていない")
	}
}

// ユーザ名前空間の中では補助グループがそのまま見えない。keep-groups が無いと
// グループで守ったソケットに繋げない (実測で確認済み)。
func TestAppsKeepTheirHostGroupsSoTheyCanReachTheSocket(t *testing.T) {
	a := App{Name: "post", TraceSockDir: "/var/lib/yunirun-obs/tempo-sock", Manifest: &manifest.Manifest{}}
	got := ContainerUnit(a, "blue")
	for _, want := range []string{
		"GroupAdd=keep-groups",
		"Environment=OTEL_EXPORTER_OTLP_ENDPOINT=unix:///run/tempo/otlp.sock",
		// 指定しないと gRPC が TLS を試みて WRONG_VERSION_NUMBER で落ちる。
		"Environment=OTEL_EXPORTER_OTLP_INSECURE=true",
		"Environment=OTEL_SERVICE_NAME=post",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s が無い:\n%s", want, got)
		}
	}
}

// 単機構成では接続先が空のままになり、Tempo 自身が「推測した」と警告を出した
// うえで問い合わせに失敗し続ける。明示すると出なくなる (実測で確認済み)。
func TestTempoIsToldWhereItsOwnPartsAre(t *testing.T) {
	got := spec().TempoConfig()
	if !strings.Contains(got, "frontend_address: 127.0.0.1:9095") {
		t.Fatalf("接続先を明示していない:\n%s", got)
	}
}

// 全アドレスで待ち受けると公開 IP でも受けることになる。ホストの
// ネットワークを使うので、既定のままだと外へ出てしまう。
func TestTempoListensOnlyOnLoopback(t *testing.T) {
	got := spec().TempoConfig()
	for _, want := range []string{
		"http_listen_address: 127.0.0.1",
		"grpc_listen_address: 127.0.0.1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s が無い:\n%s", want, got)
		}
	}
}

// unit が渡す先はすべて converge が作っていないと、podman が
// "statfs ...: no such file or directory" で起動できない (実際に踏んだ)。
//
// データ領域の一覧と unit の Volume がずれていないことを固定する。
func TestEveryDataVolumeTheUnitsMountIsListedForCreation(t *testing.T) {
	s := spec()
	// converge が作るものと同じ一覧を見る。複製するとずれても気付けない。
	created := map[string]bool{}
	for _, d := range s.StackDataDirs() {
		created[d] = true
	}

	for name, unit := range s.StackUnits() {
		for _, line := range strings.Split(unit, "\n") {
			v, ok := strings.CutPrefix(line, "Volume="+s.Dir+"/")
			if !ok {
				continue
			}
			src := s.Dir + "/" + strings.SplitN(v, ":", 2)[0]
			if !created[src] {
				t.Fatalf("%s が %s を渡しているが、converge が作る一覧に無い", name, src)
			}
		}
	}
}

// 置き場所をグループで守るので、作る側 (Tempo) も入れないとソケットを
// 作れない。実際に踏んだ: ディレクトリが root:yunirun-trace 2770 で、
// Tempo は uid 10001。所属していないので書けなかった。
func TestTempoIsInTheGroupThatGuardsItsOwnSocket(t *testing.T) {
	s := spec()
	s.TraceGID = 983
	u := s.StackUnits()["yunirun-tempo.container"]
	if !strings.Contains(u, "GroupAdd=983") {
		t.Fatalf("グループに入れていない:\n%s", u)
	}
}

// 見張る対象が増えたことを固定する。規則が減っていることに気づけないと、
// 「見ているつもりで見ていない」状態に戻る。
func TestAlertRulesCoverDiskAndBackup(t *testing.T) {
	want := map[string]bool{
		"yunirun-origin-down":     false,
		"yunirun-replica-down":    false,
		"yunirun-backend-errors":  false,
		"yunirun-metrics-blind":   false,
		"yunirun-disk-low":        false,
		"yunirun-backup-stale":    false,
		"yunirun-textfile-broken": false,
	}
	for _, r := range alertRules() {
		if _, ok := want[r.UID]; !ok {
			t.Errorf("知らない規則が増えている: %s", r.UID)
			continue
		}
		want[r.UID] = true
	}
	for uid, found := range want {
		if !found {
			t.Errorf("%s が無い", uid)
		}
	}
}

// バックアップは「失敗した」ではなく「成功していない」で見る。失敗を見ると、
// そもそも動かなかった場合 (timer が止まった、script が消えた) を取りこぼす。
func TestBackupAlertWatchesAbsenceOfSuccessNotFailure(t *testing.T) {
	r := ruleByUID(t, "yunirun-backup-stale")
	if !strings.Contains(r.Expr, "yunirun_backup_last_success_seconds") {
		t.Errorf("最後に成功した時刻を見ていない: %s", r.Expr)
	}
	if !strings.Contains(r.Expr, "time()") {
		t.Errorf("経過時間で見ていない: %s", r.Expr)
	}
	// 指標そのものが消えるのも「取れていない」の一種。黙らせてはいけない。
	if r.NoData != "Alerting" {
		t.Errorf("指標が消えたときに黙る: noDataState=%s", r.NoData)
	}
}

// 置き場のファイルが 1 つ壊れると、その系列だけが黙って消える。他の系列が
// 健在なら無データにもならないので、見張っているつもりの値が欠けたまま
// 何事も無いように見える。実際に HELP の文言の食い違いで踏んだ。
// コンテナの stderr は journald が一律 err として受ける。既定の info のままだと
// 1 日 6,000 行を超える info が err として積み上がり、本物の異常が埋もれる。
func TestTempoDoesNotLogAtInfo(t *testing.T) {
	cfg := spec().TempoConfig()
	if !strings.Contains(cfg, "log_level: warn") {
		t.Errorf("log_level が絞られていない:\n%s", cfg)
	}
}

func TestBrokenTextfileIsItselfAnAlert(t *testing.T) {
	r := ruleByUID(t, "yunirun-textfile-broken")
	if !strings.Contains(r.Expr, "node_textfile_scrape_error") {
		t.Errorf("取り込みの失敗を見ていない: %s", r.Expr)
	}
	// 1 つでも壊れていれば拾う。平均や合計だと薄まる。
	if !strings.HasPrefix(r.Expr, "max(") {
		t.Errorf("1 つでも壊れていれば拾う形になっていない: %s", r.Expr)
	}
}

// Prometheus は取り込み側のジョブ名で job を上書きする。指標側が job を
// 名乗っても exported_job へ追いやられるので、job でまとめると全部の
// バックアップが 1 系列に潰れる。そうなると 1 つが止まっても他の成功が
// max で拾われて隠れる。実機で job="postgresql" が job="node" に化けた。
func TestBackupAlertDoesNotGroupByReservedJobLabel(t *testing.T) {
	r := ruleByUID(t, "yunirun-backup-stale")
	if strings.Contains(r.Expr, "by (job)") {
		t.Errorf("job でまとめている (取り込み側に上書きされて 1 系列に潰れる): %s", r.Expr)
	}
	if !strings.Contains(r.Expr, "by (backup)") {
		t.Errorf("バックアップごとに分かれていない: %s", r.Expr)
	}
	if strings.Contains(r.Summary, "$labels.job") {
		t.Errorf("どのバックアップかを job で出そうとしている: %s", r.Summary)
	}
}

// 同じ 1 台の装置を複数の場所へ結び付けている場合 (NixOS の /nix/store など)、
// 場所でまとめると同じ空き容量で二重に鳴る。
func TestDiskAlertGroupsByDeviceNotMountpoint(t *testing.T) {
	r := ruleByUID(t, "yunirun-disk-low")
	if !strings.Contains(r.Expr, "by (device)") {
		t.Errorf("装置でまとめていない: %s", r.Expr)
	}
	if strings.Contains(r.Expr, "by (mountpoint)") {
		t.Errorf("場所でまとめている (同じ装置で二重に鳴る): %s", r.Expr)
	}
	// 実体の無いファイルシステムは常に満杯や空に見えるので外す。
	if !strings.Contains(r.Expr, "tmpfs") {
		t.Errorf("仮想のファイルシステムを外していない: %s", r.Expr)
	}
}

// 指標の置き場を読むだけにしないと、書き手 (root) から所有者を奪う。
// 取り込みの指定が無ければ、ファイルを置いても指標にならない。
func TestNodeExporterReadsTheTextfileDirectory(t *testing.T) {
	unit := spec().StackUnits()["yunirun-node.container"]
	if !strings.Contains(unit, "--collector.textfile.directory=/host/textfile") {
		t.Errorf("取り込みの指定が無い:\n%s", unit)
	}
	if !strings.Contains(unit, "Volume="+spec().TextfileDir()+":/host/textfile:ro") {
		t.Errorf("置き場を読むだけで渡していない:\n%s", unit)
	}
}

// unit の Volume と作る場所がずれると podman が statfs で起動できない。
func TestTextfileDirectoryIsCreated(t *testing.T) {
	want := spec().TextfileDir()
	for _, d := range spec().StackDataDirs() {
		if d == want {
			return
		}
	}
	t.Fatalf("%s を作る一覧に入っていない: %v", want, spec().StackDataDirs())
}

func ruleByUID(t *testing.T, uid string) alertRule {
	t.Helper()
	for _, r := range alertRules() {
		if r.UID == uid {
			return r
		}
	}
	t.Fatalf("%s が無い", uid)
	return alertRule{}
}
