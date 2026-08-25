package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/render"
)

// 書き換えを避けるのは mtime を動かさないため。反映そのものは内容の変化に
// かかわらず毎回行う (ずれた状態から抜け出せなくなるため)。
func TestWriteIfChangedReportsWhetherItWrote(t *testing.T) {
	p := filepath.Join(t.TempDir(), "haproxy.cfg")

	changed, err := writeIfChanged(p, []byte("a"))
	if err != nil || !changed {
		t.Fatalf("新規作成を変更なしと報告している: changed=%v err=%v", changed, err)
	}
	changed, err = writeIfChanged(p, []byte("a"))
	if err != nil || changed {
		t.Fatalf("同じ内容を変更ありと報告している: changed=%v err=%v", changed, err)
	}
	changed, err = writeIfChanged(p, []byte("b"))
	if err != nil || !changed {
		t.Fatalf("違う内容を変更なしと報告している: changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "b" {
		t.Fatalf("中身が書き変わっていない: %q", got)
	}
}

// 起動時、converge は HAProxy より先に走る。まだ動いていないものへ信号を
// 送っても意味が無く、失敗を報告すると収束そのものが失敗扱いになる。
func TestReloadHAProxySendsNothingWhenItIsNotRunning(t *testing.T) {
	r := &recordingRunner{out: map[string]string{"podman": "some-other-container\n"}}
	if err := reloadHAProxy(t.Context(), r, "/etc/yunirun/haproxy.cfg"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if slices.Contains(c, "kill") {
			t.Fatalf("動いていないのに信号を送った: %v", r.calls)
		}
	}
}

// 壊れた設定のまま USR2 を送ると、マスターは新しいワーカーを起こせないまま
// 古いものを抱える。先に検査してから送る。
func TestReloadHAProxyChecksTheConfigBeforeSignalling(t *testing.T) {
	r := &recordingRunner{out: map[string]string{"podman": render.HAProxyContainer + "\n"}}
	if err := reloadHAProxy(t.Context(), r, "/etc/yunirun/haproxy.cfg"); err != nil {
		t.Fatal(err)
	}
	var check, kill = -1, -1
	for i, c := range r.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "haproxy -c -q") {
			check = i
		}
		if strings.Contains(j, "kill --signal USR2") {
			kill = i
		}
	}
	if check < 0 || kill < 0 {
		t.Fatalf("検査と信号の両方が要る: %v", r.calls)
	}
	if check > kill {
		t.Fatalf("検査より先に信号を送っている: %v", r.calls)
	}
}

// 宣言から消えたアプリは止める。経路 (HAProxy) は宣言から生成しているので
// 勝手に消えるのに、コンテナは Restart=always で動き続けていた。外からは
// 404 なのに中では動いてポートを掴んだまま、という状態になる。
func TestStopUndeclaredStopsOnlyWhatIsNotDeclared(t *testing.T) {
	r := &recordingRunner{}
	cfg := &config.Config{Apps: map[string]string{"alpha": "example/alpha"}}
	l := &alloc.Ledger{Entries: map[string]alloc.Alloc{
		"alpha": {UID: 6000},
		"gone":  {UID: 6001},
	}}
	stopUndeclared(t.Context(), r, cfg, l)

	var stopped []string
	for _, c := range r.calls {
		if c[0] == "systemctl" && c[1] == "stop" {
			stopped = append(stopped, c[2])
		}
	}
	if len(stopped) != 1 || stopped[0] != "user@6001.service" {
		t.Fatalf("止めた対象が想定と違う: %v (全呼び出し %v)", stopped, r.calls)
	}
}

// 止めるだけで、消してはいけない。宣言の書き間違いでデータが飛ぶのは
// 割に合わない。片付けは yunirun remove の仕事。
func TestStopUndeclaredNeverTouchesData(t *testing.T) {
	r := &recordingRunner{}
	cfg := &config.Config{Apps: map[string]string{}}
	l := &alloc.Ledger{Entries: map[string]alloc.Alloc{"gone": {UID: 6001}}}
	stopUndeclared(t.Context(), r, cfg, l)

	for _, c := range r.calls {
		switch c[0] {
		case "userdel", "psql", "runuser", "dropdb":
			t.Fatalf("データを消す操作を発行している: %v", c)
		}
	}
	// 台帳も残す。戻したときに uid とポートが元通りになる。
	if _, ok := l.Entries["gone"]; !ok {
		t.Fatal("台帳から消している")
	}
}

// バックアップの側で DB 名やソケットの場所を推測させない。規約を変えたときに
// 静かにずれ、しかも「取れた分だけ成功」に見えるので、気付くのは復元しようと
// した時になる。
func TestDatabasesListsOnlyAppsThatDeclareADatabase(t *testing.T) {
	// dbNamesFor は databaseName の宣言があればそれを使い、無ければアプリ名。
	// databases はこの導出をそのまま流すことで規約を 1 か所に保つ。
	m, err := manifest.Parse([]byte(`{"app":{"database":true,"databaseName":"streamer_post"}}`))
	if err != nil {
		t.Fatal(err)
	}
	n := dbNamesFor("post", m)
	if n.Database != "streamer_post" || n.Owner != "streamer_post" || n.App != "streamer_post_app" {
		t.Fatalf("導出が想定と違う: %+v", n)
	}
}

// 宣言は tmpfs に置いてはいけない。再起動で消え、converge が「ファイルが
// 無い」を既定値として扱って全アプリを既定設定へ書き戻す。DB を使う宣言も
// env も workload も消えるうえ、converge は成功として報告する。
func TestStoredManifestIsNotOnTmpfs(t *testing.T) {
	cfg := &config.Config{StateDir: "/var/lib/yunirun"}
	got := storedManifestPath(cfg, "post")
	if strings.HasPrefix(got, "/run/") {
		t.Fatalf("tmpfs 上に置いている: %s", got)
	}
	if !strings.HasPrefix(got, cfg.StateDir) {
		t.Fatalf("永続領域の外に置いている: %s", got)
	}
}

// deploy が置いた宣言を converge が引き取ることで、再起動を跨いでも残る。
func TestLoadManifestAdoptsWhatDeployLeftAndSurvivesTheInboxBeingLost(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}

	// deploy が inbox へ置いた状態を作る。
	useTempRuntimeDir(t)
	inbox := inboxDir("post")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	const decl = `{"app":{"database":true,"databaseName":"streamer_post"}}`
	if err := os.WriteFile(manifestPath(cfg, "post"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadManifest(cfg, "post")
	if err != nil {
		t.Fatal(err)
	}
	if !m.App.Database || m.App.DatabaseName != "streamer_post" {
		t.Fatalf("引き取れていない: %+v", m.App)
	}

	// 再起動で inbox が消えた状況。
	os.RemoveAll(inbox)
	m2, err := loadManifest(cfg, "post")
	if err != nil {
		t.Fatal(err)
	}
	if !m2.App.Database || m2.App.DatabaseName != "streamer_post" {
		t.Fatalf("再起動後に宣言が失われている: %+v", m2.App)
	}
}

// 壊れた宣言で既に引き取ってあるものを潰さない。
func TestLoadManifestKeepsTheStoredOneWhenTheNewOneIsBroken(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	good := `{"app":{"database":true}}`
	if err := os.WriteFile(storedManifestPath(cfg, "post"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	useTempRuntimeDir(t)
	inbox := inboxDir("post")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath(cfg, "post"), []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadManifest(cfg, "post"); err == nil {
		t.Fatal("壊れた宣言を受け入れている")
	}
	b, _ := os.ReadFile(storedManifestPath(cfg, "post"))
	if string(b) != good {
		t.Fatalf("既存の宣言を潰した: %s", b)
	}
}

// unit が EnvironmentFile= で参照する env は、起動時に必ず存在していなければ
// ならない。tmpfs に置くと再起動のたびに converge との競争になり、負けると
// unit は起動に失敗する。しかもアプリ側はユーザ unit なのでシステム unit を
// After= できず、順序では直せない。
func TestUnitEnvFilesAreNotOnTmpfs(t *testing.T) {
	cfg := &config.Config{StateDir: "/var/lib/yunirun"}
	for _, name := range []string{"runtime.env", "db.env", "migration.env"} {
		got := cfg.EnvPath("post", name)
		if strings.HasPrefix(got, "/run/") {
			t.Fatalf("%s を tmpfs 上に置いている: %s", name, got)
		}
	}
}

// stateDir は root 専用 (0700) なので、その中に置くとアプリのユーザ unit が
// 自分の runtime.env まで辿れない。
func TestEnvDirIsReachableFromOutsideTheRootOnlyStateDir(t *testing.T) {
	cfg := &config.Config{StateDir: "/var/lib/yunirun"}
	if got := cfg.EnvPath("post", "runtime.env"); strings.HasPrefix(got, cfg.StateDir+"/") {
		t.Fatalf("root 専用の stateDir の中に置いている: %s", got)
	}
}

// useTempRuntimeDir は inbox の位置をテスト用に差し替える。
//
// 既定は /run/yunirun で、CI の実行ユーザは書けない。以前はそこで skip して
// いたため、inbox を跨ぐ引き取りのテストが CI で一度も走っていなかった。
func useTempRuntimeDir(t *testing.T) {
	t.Helper()
	old := runtimeDir
	runtimeDir = t.TempDir()
	t.Cleanup(func() { runtimeDir = old })
}

// 補助グループはプロセスの起動時に決まる。動き続けているユーザの systemd
// インスタンスは新しい所属を持たず、配下のコンテナも持てない。
//
// 実際に踏んだ: グループを足したのに keep-groups が効かず、コンテナの中では
// 65534(nobody) に見えていた。保つべき所属が最初から無かった。
// マネージャは 3 日前から動いており Groups は 6004 だけだった。
func TestAddingAGroupRestartsTheUserInstance(t *testing.T) {
	// 入れ直しの呼び出しが AddToGroup の結果に紐づいていること。
	// 無条件に入れ直すと収束のたびに全コンテナが落ちる。
	src, err := os.ReadFile("converge.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "system.AddToGroup(ctx, r, user, render.TraceGroup)")
	if i < 0 {
		t.Fatal("グループへの追加が無い")
	}
	rest := s[i:]
	if j := strings.Index(rest, "RestartUserInstance"); j < 0 || j > 400 {
		t.Fatal("グループを足した直後にユーザのインスタンスを入れ直していない")
	}
	if !strings.Contains(rest[:400], "if added {") {
		t.Fatal("足したときだけ入れ直す形になっていない (毎回落ちる)")
	}
}

// Unix ソケットのファイルは停止時に消えない。残ったまま起動すると bind に
// 失敗し、受信部が起動しないまま Tempo だけが動く。実際に踏んだ。
func TestStaleTraceSocketIsRemovedOnlyWhenTempoIsNotRunning(t *testing.T) {
	src, err := os.ReadFile("converge.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "os.Remove(spec.HostTraceSocket())")
	if i < 0 {
		t.Fatal("残ったソケットを消していない")
	}
	// 動いているものから取り上げない。
	if !strings.Contains(s[max(0, i-200):i], "!containerRunning(") {
		t.Fatal("動いていても消す形になっている")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
