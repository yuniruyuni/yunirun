package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/config"
)

const testConfigJSON = `{"domain":"example.test","stateDir":"/var/lib/yunirun",
"hostKeyPath":"/k","secretsKeyPath":"/s","homesDir":"/var/lib/yunirun-apps",
"dbDir":"/var/lib/yunirun-db","envDir":"/var/lib/yunirun-env",
"observability":{"enable":true,"dir":"/var/lib/yunirun-obs"},
"apps":{"blog":"someone/blog"}}`

// needTool は道具が無い環境では飛ばす。ただし CI では飛ばさない。
//
// 飛んだことは go test の既定の出力に現れないので、道具が入っていない CI では
// 検査したつもりで何も検査していない状態になる。以前これで、意図した検査が
// ずっと飛んでいたことに気づけなかった。
func needTool(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/" + name, "/sbin/" + name} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s が無い。CI では入っている前提", name)
	}
	t.Skipf("%s が無い", name)
	return ""
}

func stageInstall(t *testing.T) string {
	t.Helper()
	needTool(t, "visudo")
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	if err := os.WriteFile(src, []byte(testConfigJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	err := runInstall(context.Background(), []string{"--root", root, "--from", src})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInstallPlacesEverythingTheModuleDoes(t *testing.T) {
	root := stageInstall(t)
	for _, p := range []string{
		"/etc/yunirun/config.json",
		"/etc/systemd/system/yunirun-converge.service",
		"/etc/systemd/system/yunirun-migrate@.service",
		"/etc/systemd/system/yunirun-usage.service",
		"/etc/tmpfiles.d/yunirun.conf",
		"/etc/sudoers.d/yunirun",
	} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s が置かれていない", p)
		}
	}
}

// 壊れた sudoers を置くと sudo 全体が使えなくなり、直す手段まで失う。
// visudo に通るものだけを置く。
func TestInstallWritesSudoersThatVisudoAccepts(t *testing.T) {
	root := stageInstall(t)
	visudo := needTool(t, "visudo")
	p := filepath.Join(root, "/etc/sudoers.d/yunirun")
	out, err := exec.Command(visudo, "-c", "-f", p).CombinedOutput()
	if err != nil {
		t.Fatalf("visudo が拒否した: %s", out)
	}
}

func TestInstallLeavesSudoersOwnerReadOnly(t *testing.T) {
	p := filepath.Join(stageInstall(t), "/etc/sudoers.d/yunirun")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o440 {
		t.Errorf("モードが %04o", fi.Mode().Perm())
	}
}

// converge から繰り返し呼ばれても書き換えないこと。書き換えると systemd が
// 変更を検知して無用な再起動を招く。
func TestInstallDoesNotRewriteUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	if err := os.WriteFile(src, []byte(testConfigJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	needTool(t, "visudo")
	root := filepath.Join(dir, "root")
	args := []string{"--root", root, "--from", src}
	if err := runInstall(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "/etc/systemd/system/yunirun-converge.service")
	fi, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unit, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	before := fi.ModTime()
	if err := runInstall(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before) {
		t.Error("内容が同じなのに書き換えている")
	}
}

// 壊れた設定を置いてから converge が落ちるより、置く前に断る方が分かりやすい。
func TestInstallRejectsUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	// domain が空。validate が弾く。
	if err := os.WriteFile(src, []byte(`{"stateDir":"/var/lib/yunirun"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	err := runInstall(context.Background(), []string{"--root", root, "--from", src})
	if err == nil {
		t.Fatal("壊れた設定が通った")
	}
	if _, statErr := os.Stat(filepath.Join(root, "/etc/yunirun/config.json")); statErr == nil {
		t.Error("断ったのに置いている")
	}
}

// unit の ExecStart は自分自身を指す。消えうる場所のまま据え付けると、
// 再起動後に起動しない unit ができる。
func TestSelfPathRejectsEphemeralLocations(t *testing.T) {
	for _, p := range []string{"/tmp/yunirun", "/run/x/yunirun", "/var/tmp/yunirun"} {
		if !ephemeral(p) {
			t.Errorf("%s を消えうる場所と見なしていない", p)
		}
	}
	if ephemeral("/usr/local/bin/yunirun") {
		t.Error("/usr/local/bin を消えうる場所と見なしている")
	}
}

func ephemeral(p string) bool {
	for _, bad := range ephemeralPrefixes {
		if strings.HasPrefix(p, bad) {
			return true
		}
	}
	return false
}

// NixOS では断るという判断を --root / で迂回できてはいけない。
func TestRootSlashIsNotStaging(t *testing.T) {
	for _, flag := range []string{"", "/", "//", "///"} {
		if _, staging := stagingRoot(flag); staging {
			t.Errorf("--root %q を staging と見なしている", flag)
		}
	}
	for _, flag := range []string{"/srv/stage", "/srv/stage/"} {
		root, staging := stagingRoot(flag)
		if !staging || root != "/srv/stage" {
			t.Errorf("--root %q → %q staging=%v", flag, root, staging)
		}
	}
}

// systemd が受け取れる形になっているか、systemd 自身に確かめさせる。
//
// 書式の誤りは起動して初めて分かることが多く、しかもそのときには
// 「なぜか動かない unit」として現れる。
func TestInstallWritesUnitsSystemdAccepts(t *testing.T) {
	root := stageInstall(t)
	analyze := needTool(t, "systemd-analyze")
	dir := filepath.Join(root, "/etc/systemd/system")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		out, err := exec.Command(analyze, "verify", "--no-pager", filepath.Join(dir, e.Name())).CombinedOutput()
		// ExecStart の実体や依存する target の有無まで見られると、環境に
		// よって落ちる。書式の誤りだけを問題にする。
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "Failed to parse") ||
				strings.Contains(line, "Unknown key") ||
				strings.Contains(line, "Invalid ") {
				t.Errorf("%s: %s", e.Name(), line)
			}
		}
		_ = err
	}
}

// 鍵を失うと保存した秘密を復号できない。既にあるものを潰してはいけない。
func TestGenerateKeyNeverOverwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(p, []byte("AGE-SECRET-KEY-EXISTING\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := generateKey(p); err == nil {
		t.Fatal("既存の鍵を上書きした")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "AGE-SECRET-KEY-EXISTING\n" {
		t.Fatalf("中身が変わっている: %q", b)
	}
}

// 秘密鍵なので所有者だけが読めること。
func TestGenerateKeyIsOwnerReadOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "key.txt")
	if err := generateKey(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o400 {
		t.Errorf("モードが %04o", fi.Mode().Perm())
	}
}

// 据え付けてから converge が落ちて初めて分かるより、先に言う方が早い。
func TestPrereqsReportsMissingKeys(t *testing.T) {
	cfg := &config.Config{
		Domain: "x.test", StateDir: "/var/lib/yunirun",
		HostKeyPath: filepath.Join(t.TempDir(), "nope.txt"),
	}
	err := checkPrereqs(cfg, false)
	if err == nil {
		t.Fatal("鍵が無いのに通った")
	}
	if !strings.Contains(err.Error(), "hostKeyPath") {
		t.Errorf("何が足りないか言っていない: %v", err)
	}
}

// 無いと user@<uid>.service が XDG_RUNTIME_DIR is not set で落ちる。
// converge からは「systemd が起動しません」としか見えず、真因が埋もれる。
// Debian で実際にここで詰まった。
func TestPAMSystemdIsAPrerequisite(t *testing.T) {
	// この環境に在るなら、探し方が正しいことの確認になる。
	found := hasPAMSystemd()
	real := false
	for _, g := range []string{
		"/usr/lib/*/security/pam_systemd.so",
		"/usr/lib64/security/pam_systemd.so",
		"/usr/lib/security/pam_systemd.so",
		"/lib/*/security/pam_systemd.so",
	} {
		if m, _ := filepath.Glob(g); len(m) > 0 {
			real = true
		}
	}
	if real != found {
		t.Fatalf("探し方が実態と合っていない: 在る=%v 見つけた=%v", real, found)
	}
	if os.Getenv("CI") != "" && !found {
		t.Skip("CI の環境に pam_systemd.so が無い")
	}
}

// 前提の一覧に挙がっていないと、Debian で踏んだのと同じ形で詰まる。
func TestPrereqsMentionsPAMSystemd(t *testing.T) {
	if hasPAMSystemd() {
		t.Skip("この環境には在るので、欠けたときの文言を確かめられない")
	}
	cfg := &config.Config{Domain: "x.test", StateDir: "/var/lib/yunirun"}
	err := checkPrereqs(cfg, false)
	if err == nil || !strings.Contains(err.Error(), "pam_systemd") {
		t.Errorf("pam_systemd に触れていない: %v", err)
	}
}
