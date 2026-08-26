package render

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func testInstall() InstallInput {
	return InstallInput{
		Exe:       "/usr/local/bin/yunirun",
		Systemctl: "/usr/bin/systemctl",
		Apps:      []string{"shop", "blog"},
		Usage:     true,
		Dirs:      map[string]string{"/var/lib/yunirun": "0700", "/run/yunirun": "0755"},
	}
}

func fileNamed(t *testing.T, files []InstallFile, path string) InstallFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s が無い", path)
	return InstallFile{}
}

// sudo はコマンドを文字列で照合する。呼び出し側が PATH で解決した結果と
// 一致していなければ、実体が同じでも拒否される。
func TestSudoersUsesTheResolvedSystemctlPath(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/sudoers.d/yunirun")
	for _, line := range strings.Split(f.Content, "\n") {
		if !strings.Contains(line, "systemctl") {
			continue
		}
		if !strings.Contains(line, "NOPASSWD: /usr/bin/systemctl ") {
			t.Errorf("解決したパスで許可していない: %s", line)
		}
	}
}

// converge は RemainAfterExit=true で active (exited) のまま留まるため、
// start では何も起きない。restart でなければ宣言の変更が反映されない。
func TestSudoersAllowsRestartNotStartForConverge(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/sudoers.d/yunirun")
	if !strings.Contains(f.Content, "restart yunirun-converge.service") {
		t.Error("converge を restart で許していない")
	}
	if strings.Contains(f.Content, "start yunirun-converge.service") &&
		!strings.Contains(f.Content, "restart yunirun-converge.service") {
		t.Error("converge を start で許している")
	}
}

// 他のアプリの migration を起動できてはいけない。
func TestSudoersLimitsMigrationToOwnApp(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/sudoers.d/yunirun")
	for _, line := range strings.Split(f.Content, "\n") {
		if !strings.Contains(line, "yunirun-migrate@") {
			continue
		}
		user := strings.Fields(line)[0]
		app := strings.TrimPrefix(user, "yunirun-")
		if !strings.Contains(line, "yunirun-migrate@"+app+".service") {
			t.Errorf("%s が他のアプリの migration を起動できる: %s", user, line)
		}
	}
}

// 壊れた sudoers は sudo 全体を使えなくする。行末に余計なものが無いこと。
func TestSudoersHasNoBlankOrPartialLines(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/sudoers.d/yunirun")
	for i, line := range strings.Split(strings.TrimRight(f.Content, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("%d 行目が空", i+1)
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "ALL=(root) NOPASSWD:") {
			t.Errorf("%d 行目が許可の形をしていない: %s", i+1, line)
		}
	}
}

// tmpfs である /run/yunirun は再起動で消える。mkdir では次の起動で無くなる。
func TestTmpfilesCoversRunDirectory(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/tmpfiles.d/yunirun.conf")
	if !strings.Contains(f.Content, "d /run/yunirun 0755 root root -") {
		t.Errorf("/run/yunirun の規則が無い:\n%s", f.Content)
	}
}

// 台帳と秘密が入るので root 専用でなければならない。
func TestStateDirIsRootOnly(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/tmpfiles.d/yunirun.conf")
	if !strings.Contains(f.Content, "d /var/lib/yunirun 0700 root root -") {
		t.Errorf("stateDir が root 専用でない:\n%s", f.Content)
	}
}

func TestSudoersIsOwnerReadOnly(t *testing.T) {
	f := fileNamed(t, InstallUnits(testInstall()), "/etc/sudoers.d/yunirun")
	if f.Mode != 0o440 {
		t.Errorf("sudoers のモードが %04o", f.Mode)
	}
}

// install と NixOS モジュールは同じものを置く 2 つの実装なので、片方に
// unit を足してもう片方を忘れると、ホストの種類によって挙動が変わる。
func TestInstallAndNixModuleDefineTheSameUnits(t *testing.T) {
	b, err := os.ReadFile("../../nix/module.nix")
	if err != nil {
		t.Skipf("モジュールを読めない: %v", err)
	}
	re := regexp.MustCompile(`systemd\.services\.?"?(yunirun-[a-z@]*)"?`)
	nix := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		nix[m[1]] = true
	}
	got := map[string]bool{}
	for _, f := range InstallUnits(testInstall()) {
		if !strings.HasPrefix(f.Path, "/etc/systemd/system/") {
			continue
		}
		got[strings.TrimSuffix(strings.TrimPrefix(f.Path, "/etc/systemd/system/"), ".service")] = true
	}
	if len(nix) == 0 {
		t.Fatal("モジュールから unit を読み取れなかった")
	}
	if !equalSets(nix, got) {
		t.Errorf("食い違っている\n  module.nix: %v\n  install:    %v", keys(nix), keys(got))
	}
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
