package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
