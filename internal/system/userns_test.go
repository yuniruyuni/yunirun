package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// 実際に本番で読んだ写像。fighter (uid 6006, subuid 4393216) の正常時と、
// 再起動直後に /etc/subuid が欠けた状態で作られてしまった名前空間。
const (
	healthyMap = "         0       6006          1\n         1    4393216      65536\n"
	staleMap   = "         0       6006          1\n"
)

func fighter() alloc.Alloc {
	return alloc.Alloc{UID: 6006, GID: 6006, SubUID: 4393216}
}

func TestParseIDMapReadsKernelFormat(t *testing.T) {
	got, err := ParseIDMap(healthyMap)
	if err != nil {
		t.Fatal(err)
	}
	want := []IDMap{{0, 6006, 1}, {1, 4393216, 65536}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%d 行目: %+v != %+v", i, got[i], want[i])
		}
	}
}

func TestParseIDMapRejectsGarbage(t *testing.T) {
	// 読めない出力を「写像が空」と扱うと、壊れていないものを作り直してしまう。
	for _, s := range []string{"0 6006\n", "a b c\n", "Error: cannot connect\n"} {
		if _, err := ParseIDMap(s); err == nil {
			t.Fatalf("%q を受け付けた", s)
		}
	}
}

func TestStaleDetectsNamespaceCreatedWithoutSubIDs(t *testing.T) {
	ok, _ := ParseIDMap(healthyMap)
	bad, _ := ParseIDMap(staleMap)

	if (UserNamespace{UIDMap: ok, GIDMap: ok}).Stale(fighter()) {
		t.Fatal("正常な名前空間を古いと判定した")
	}
	if !(UserNamespace{UIDMap: bad, GIDMap: bad}).Stale(fighter()) {
		t.Fatal("subuid の欠けた名前空間を見逃した (本番で踏んだ形)")
	}
	// uid だけ張られていて gid が欠けている場合も、chown に失敗する。
	if !(UserNamespace{UIDMap: ok, GIDMap: bad}).Stale(fighter()) {
		t.Fatal("gid 側の欠けを見逃した")
	}
}

func TestStaleDetectsWrongRange(t *testing.T) {
	// 別の範囲 (改名前の番号など) が張られていても、割り当てと違えば古い。
	m, _ := ParseIDMap("0 6006 1\n1 4000000 65536\n")
	if !(UserNamespace{UIDMap: m, GIDMap: m}).Stale(fighter()) {
		t.Fatal("違う範囲を正しいと判定した")
	}
}

func TestCoversOutsideJoinsAdjacentLines(t *testing.T) {
	// podman は範囲を複数行に分けて張ることがある。
	m := []IDMap{{0, 6006, 1}, {1, 4393216, 1000}, {1001, 4394216, 64536}}
	if !CoversOutside(m, 4393216, 65536) {
		t.Fatal("隣り合う行をつなげていない")
	}
	if CoversOutside(m[:2], 4393216, 65536) {
		t.Fatal("途中までしか無いのに含むと判定した")
	}
	if !CoversOutside(nil, 10, 0) {
		t.Fatal("幅 0 は常に含まれる")
	}
}

// probeRunner は podman unshare の出力を返し、呼ばれたコマンドを記録する。
type probeRunner struct {
	calls []string
	// maps は unshare の呼び出しごとに返す写像。使い切ったら最後のものを返す。
	maps []string
	n    int
}

func (p *probeRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	p.calls = append(p.calls, cmd)
	if strings.Contains(cmd, "podman unshare") {
		i := p.n / 2 // uid_map と gid_map で 1 組
		if i >= len(p.maps) {
			i = len(p.maps) - 1
		}
		p.n++
		return []byte(p.maps[i]), nil
	}
	return nil, nil
}

func (p *probeRunner) RunEnv(ctx context.Context, stdin []byte, _ []string, name string, args ...string) ([]byte, error) {
	return p.Run(ctx, stdin, name, args...)
}

func (p *probeRunner) ran(sub string) bool {
	for _, c := range p.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestEnsureUserNamespaceLeavesHealthyAlone(t *testing.T) {
	r := &probeRunner{maps: []string{healthyMap}}
	healed, err := EnsureUserNamespace(context.Background(), r, "yunirun-fighter", fighter())
	if err != nil {
		t.Fatal(err)
	}
	// 正しい名前空間を作り直すと、収束のたびにコンテナが落ちる。
	if healed || r.ran("system migrate") || r.ran("systemctl restart") {
		t.Fatalf("正常なのに作り直した: %v", r.calls)
	}
	// アプリのユーザとして実行していること。root の名前空間を見ても意味が無い。
	if !r.ran("--uid 6006") || !r.ran("XDG_RUNTIME_DIR=/run/user/6006") {
		t.Fatalf("アプリのユーザで実行していない: %v", r.calls)
	}
}

// stubRestart は RestartUserInstance を記録だけするものに差し替える。
func stubRestart(t *testing.T) {
	t.Helper()
	old := restartUserInstance
	restartUserInstance = func(ctx context.Context, r Runner, uid int) error {
		_, err := r.Run(ctx, nil, "systemctl", "restart", fmt.Sprintf("user@%d.service", uid))
		return err
	}
	t.Cleanup(func() { restartUserInstance = old })
}

func TestEnsureUserNamespaceMigratesStaleThenRestarts(t *testing.T) {
	stubRestart(t)
	r := &probeRunner{maps: []string{staleMap, healthyMap}}
	healed, err := EnsureUserNamespace(context.Background(), r, "yunirun-fighter", fighter())
	if err != nil {
		t.Fatal(err)
	}
	if !healed {
		t.Fatal("作り直したのに false を返した")
	}

	mig, restart, reprobe := -1, -1, -1
	for i, c := range r.calls {
		switch {
		case strings.Contains(c, "podman system migrate") && mig < 0:
			mig = i
		case strings.Contains(c, "systemctl restart user@6006.service") && restart < 0:
			restart = i
		case strings.Contains(c, "podman unshare") && restart >= 0 && reprobe < 0:
			reprobe = i
		}
	}
	if mig < 0 {
		t.Fatalf("古い名前空間を作り直していない: %v", r.calls)
	}
	// migrate はコンテナを止めるので、その後に unit から起動し直させる必要がある。
	if restart < mig {
		t.Fatalf("migrate の後にインスタンスを入れ直していない: %v", r.calls)
	}
	// 直ったかどうかを確かめずに成功としない。
	if reprobe < restart {
		t.Fatalf("作り直した後に確かめていない: %v", r.calls)
	}
	if !strings.Contains(r.calls[mig], "--uid 6006") {
		t.Fatalf("migrate をアプリのユーザで実行していない: %s", r.calls[mig])
	}
}

func TestEnsureUserNamespaceReportsWhenMigrateDoesNotHelp(t *testing.T) {
	stubRestart(t)
	// /etc/subuid そのものが欠けていると、migrate しても同じ写像に戻る。
	r := &probeRunner{maps: []string{staleMap, staleMap}}
	healed, err := EnsureUserNamespace(context.Background(), r, "yunirun-fighter", fighter())
	if err == nil {
		t.Fatal("作り直しても欠けたままなのに成功とした")
	}
	if !healed {
		t.Fatal("作り直しを試みたことを返していない")
	}
}

func TestEnsureSubIDsReportsChange(t *testing.T) {
	dir := t.TempDir()
	old := SubIDFiles
	SubIDFiles = []string{filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")}
	defer func() { SubIDFiles = old }()

	// NixOS の activation が再生成した直後の形。他人の行だけがある。
	for _, p := range SubIDFiles {
		if err := os.WriteFile(p, []byte("alice:100000:65536\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := EnsureSubIDs("yunirun-fighter", fighter())
	if err != nil || !changed {
		t.Fatalf("消えた行を戻していない: changed=%v err=%v", changed, err)
	}
	changed, err = EnsureSubIDs("yunirun-fighter", fighter())
	if err != nil || changed {
		t.Fatalf("変わっていないのに書き換えた: changed=%v err=%v", changed, err)
	}
	for _, p := range SubIDFiles {
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "alice:100000:65536\n") ||
			!strings.Contains(string(b), "yunirun-fighter:4393216:65536\n") {
			t.Fatalf("%s:\n%s", p, b)
		}
		st, _ := os.Stat(p)
		// newuidmap は誰でも読める必要がある。
		if st.Mode().Perm() != 0o644 {
			t.Fatalf("%s の権限が %v", p, st.Mode().Perm())
		}
	}
}

func TestUserPathPutsSetuidWrappersFirst(t *testing.T) {
	if _, err := os.Stat(setuidWrappers); err != nil {
		// NixOS 以外では変えない。
		if got := userPath("/usr/bin"); got != "/usr/bin" {
			t.Fatalf("%q", got)
		}
		return
	}
	// shadow のパッケージが wrapper より前にあると setuid の無い newuidmap を拾う。
	got := userPath("/nix/store/x-shadow/bin:/run/wrappers/bin")
	if !strings.HasPrefix(got, setuidWrappers+":") {
		t.Fatalf("wrapper が先頭に無い: %q", got)
	}
}
