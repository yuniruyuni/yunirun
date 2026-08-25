package system

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriteUnitsCreatesEveryIntermediateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"a.container": "x"}); err != nil {
		t.Fatal(err)
	}
	// 中間のどれか 1 つでも別の所有者で残ると、systemd-tmpfiles も podman も
	// unsafe path transition として拒否する。
	for _, r := range []string{".config", ".config/containers", ".config/containers/systemd"} {
		if _, err := os.Stat(filepath.Join(home, r)); err != nil {
			t.Fatalf("%s が作られていない: %v", r, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(UnitDir(home), "a.container"))
	if err != nil || string(b) != "x" {
		t.Fatalf("unit が書けていない: %v %q", err, b)
	}
}

func TestWriteUnitsRemovesUnitsNoLongerDeclared(t *testing.T) {
	home := t.TempDir()
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"old.container": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"new.container": "y"}); err != nil {
		t.Fatal(err)
	}
	// 残すと、構成を変えたときに古いコンテナが動き続ける。
	if _, err := os.Stat(filepath.Join(UnitDir(home), "old.container")); !os.IsNotExist(err) {
		t.Fatal("宣言から消えた unit が残っている")
	}
}

func TestWriteUnitsLeavesUnchangedFilesAlone(t *testing.T) {
	home := t.TempDir()
	units := map[string]string{"a.container": "x"}
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), units); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(UnitDir(home), "a.container")
	st1, _ := os.Stat(p)

	if err := WriteUnits(home, os.Getuid(), os.Getgid(), units); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	// 書き換えると systemd が変更を検知して無用な再起動を招く。
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("内容が同じなのに書き換えている")
	}
}

// linger を有効にしても user@<uid>.service の起動は即座ではない。ユーザを
// 作った直後に daemon-reload を呼ぶと bus がまだ無く、
//
//	Failed to connect to user scope bus via local transport
//
// で失敗する。待機が入っていることを型として保証できないので、せめて
// 呼び出し順を固定する。
func TestReloadUserUnitsWaitsForTheUserInstanceFirst(t *testing.T) {
	r := &recordingRunner{}
	// 存在しない uid なので待機は失敗するが、順序は観測できる。
	defer func(n int) { userInstanceWaitTries = n }(userInstanceWaitTries)
	userInstanceWaitTries = 1
	_ = ReloadUserUnits(context.Background(), r, "nobody", 999999)

	if len(r.commands) == 0 {
		t.Fatal("何も実行していない")
	}
	// 最初に user@<uid> を起動しようとするはず。daemon-reload が先に来ると
	// bus が無い状態で叩くことになる。
	first := r.commands[0]
	if !strings.Contains(first, "user@999999.service") {
		t.Fatalf("ユーザインスタンスの起動を先に試していない: %q", first)
	}
	for _, c := range r.commands {
		if strings.Contains(c, "daemon-reload") {
			t.Fatal("待機が終わる前に daemon-reload を呼んでいる")
		}
	}
}

// .timer を Quadlet のディレクトリへ置いても効かない。Quadlet は自分が知る
// 種類しか処理せず、それ以外は無視するので、systemd からは見えないファイルが
// 1 つ増えるだけになる。実際 fighter の cleanup がこれで動かなかった。
func TestWriteUnitsPutsTimersWhereSystemdLooks(t *testing.T) {
	home := t.TempDir()
	err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{
		"fighter-blue.container":    "[Container]\n",
		"fighter-cleanup.container": "[Container]\n",
		"fighter-cleanup.timer":     "[Timer]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(UnitDir(home), "fighter-cleanup.timer")); err == nil {
		t.Fatal("timer が Quadlet のディレクトリに置かれている。systemd からは見えない")
	}
	for _, p := range []string{
		filepath.Join(SystemdUserDir(home), "fighter-cleanup.timer"),
		filepath.Join(UnitDir(home), "fighter-blue.container"),
		filepath.Join(UnitDir(home), "fighter-cleanup.container"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s が無い: %v", p, err)
		}
	}
}

// enable が作る timers.target.wants を消してはいけない。消すと timer が
// 無効に戻り、次の収束まで動かない。
func TestWriteUnitsKeepsSystemdOwnedDirectories(t *testing.T) {
	home := t.TempDir()
	wants := filepath.Join(SystemdUserDir(home), "timers.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{
		"a-cleanup.timer": "[Timer]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wants); err != nil {
		t.Fatalf("timers.target.wants を消している: %v", err)
	}
}

// 内容の変化だけを見ていると、書き換え済みで未反映という状態から抜け出せない。
// 実際、Grafana の unit を書き換えた次の収束で「内容は同じ」と判定され、
// 1 時間前の定義のまま動き続けた。規則が 1 つも入っていないのに converge は
// 成功と報告していた。判定は unit ファイルとコンテナの時刻で行う。
func TestApplySystemUnitCatchesAlreadyWrittenButUnappliedDrift(t *testing.T) {
	dir := t.TempDir()
	old := SystemUnitDir
	SystemUnitDir = dir
	t.Cleanup(func() { SystemUnitDir = old })

	// 既に書いてあり、内容も一致している。だがコンテナはそれより前に起動した。
	if err := os.WriteFile(filepath.Join(dir, "x.container"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &okRunner{out: "1"} // コンテナの起動は 1970 年
	if err := ApplySystemUnit(t.Context(), r, "x.container", "one"); err != nil {
		t.Fatal(err)
	}
	if !containsCall(r.commands, "systemctl restart x.service") {
		t.Fatalf("未反映のずれを見逃した: %v", r.commands)
	}
}

// コンテナの方が新しければ触らない。毎回再起動すると無用な断を招く。
func TestApplySystemUnitLeavesAnUpToDateContainerAlone(t *testing.T) {
	dir := t.TempDir()
	old := SystemUnitDir
	SystemUnitDir = dir
	t.Cleanup(func() { SystemUnitDir = old })

	if err := os.WriteFile(filepath.Join(dir, "x.container"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &okRunner{out: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}
	if err := ApplySystemUnit(t.Context(), r, "x.container", "one"); err != nil {
		t.Fatal(err)
	}
	if containsCall(r.commands, "systemctl restart x.service") {
		t.Fatalf("新しいコンテナを再起動した: %v", r.commands)
	}
}

// Quadlet の既定名は systemd-<unit 名>。ContainerName= があればそちらを見る。
// ここを間違えると inspect が失敗し、ずれを検知できないまま黙る。
func TestContainerNameFollowsQuadletsRule(t *testing.T) {
	if got := containerName("post-db.container", "[Container]\nImage=x\n"); got != "systemd-post-db" {
		t.Fatalf("既定名が違う: %s", got)
	}
	if got := containerName("yunirun-grafana.container", "[Container]\nContainerName=yunirun-grafana\n"); got != "yunirun-grafana" {
		t.Fatalf("ContainerName を見ていない: %s", got)
	}
}

func containsCall(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

// okRunner は記録しつつ成功を返す。recordingRunner は常に失敗を返すので、
// 途中で止まらず最後まで進む経路の検証には使えない。
type okRunner struct {
	commands []string
	// out は podman inspect が返す値。ずれの判定に使う。
	out string
}

func (r *okRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	return []byte(r.out), nil
}

func (r *okRunner) RunEnv(ctx context.Context, stdin []byte, _ []string, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, stdin, name, args...)
}

// unit の内容が同じでも、渡している設定が変われば動いているものは古い。
// 実際、Prometheus の取り込み対象を足したときに unit は変わらず、node exporter を
// 見に行かないまま「収束した」と報告していた。
func TestApplySystemUnitCatchesAChangedConfigEvenWhenTheUnitIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	old := SystemUnitDir
	SystemUnitDir = dir
	t.Cleanup(func() { SystemUnitDir = old })

	// unit は既にあり内容も同じ。設定ファイルだけがコンテナより新しい。
	if err := os.WriteFile(filepath.Join(dir, "x.container"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "x.yml")
	if err := os.WriteFile(cfg, []byte("scrape: node"), 0o644); err != nil {
		t.Fatal(err)
	}
	// unit だけを見る呼び方では気付かない。
	r := &okRunner{out: strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)}
	if err := ApplySystemUnit(t.Context(), r, "x.container", "one"); err != nil {
		t.Fatal(err)
	}
	// 設定も渡せば気付く。
	r2 := &okRunner{out: strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)}
	if err := ApplySystemUnit(t.Context(), r2, "x.container", "one", cfg); err != nil {
		t.Fatal(err)
	}
	if !containsCall(r2.commands, "systemctl restart x.service") {
		t.Fatalf("設定の変化を取りこぼした: %v", r2.commands)
	}
}
