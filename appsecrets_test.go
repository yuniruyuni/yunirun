package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/config"
)

const armored = "-----BEGIN AGE ENCRYPTED FILE-----\nYWJj\n-----END AGE ENCRYPTED FILE-----\n"

// 名前は暗号文のファイル名からそのまま作る。区切り文字を通すと、保存先の外を
// 指す名前が書けてしまう。
func TestSecretNameRejectsAnythingThatCouldEscapeTheStore(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd", "a/b", "lower", "", "WITH SPACE", "WITH-DASH", "1LEADING",
	} {
		if err := checkSecretName(bad); err == nil {
			t.Fatalf("通してはいけない名前を通した: %q", bad)
		}
	}
	for _, ok := range []string{"A", "BETTER_AUTH_SECRET", "DB_PASSWORD2"} {
		if err := checkSecretName(ok); err != nil {
			t.Fatalf("使える名前を拒んだ: %q (%v)", ok, err)
		}
	}
}

// 復号は converge まで行わないので、受け取った時点で中身は確かめられない。
// せめて age の暗号文でないものは入口で弾く。
func TestSecretBodyMustLookLikeAgeCiphertext(t *testing.T) {
	if err := checkSecretBody("X", []byte("plain value")); err == nil {
		t.Fatal("平文を通した")
	}
	if err := checkSecretBody("X", []byte(armored)); err != nil {
		t.Fatalf("armor 形式を拒んだ: %v", err)
	}
}

// deploy が置いたものを converge が引き取る。inbox は tmpfs なので、引き取り先は
// 再起動を跨いで残らなければならない。
func TestAdoptAppSecretsSurvivesTheInboxBeingLost(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	useTempRuntimeDir(t)

	src := inboxSecretsDir("beta")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "TOKEN.age"), []byte(armored), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := adoptAppSecrets(cfg, "beta"); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(appSecretsDir(cfg, "beta"), "TOKEN.age")
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("引き取れていない: %v", err)
	}

	// inbox が消えても引き取り済みのものは残る。
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if err := adoptAppSecrets(cfg, "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("inbox の消失で失われた: %v", err)
	}
}

// アプリ側で秘密を消したら、ホスト側からも消えてほしい。差分で足すだけだと
// 消したはずの値が環境変数に残り続ける。
func TestAdoptAppSecretsReplacesTheWholeSet(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	useTempRuntimeDir(t)

	src := inboxSecretsDir("beta")
	write := func(names ...string) {
		if err := os.RemoveAll(src); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(src, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(src, n+".age"), []byte(armored), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	write("OLD", "KEPT")
	if err := adoptAppSecrets(cfg, "beta"); err != nil {
		t.Fatal(err)
	}
	write("KEPT")
	if err := adoptAppSecrets(cfg, "beta"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(appSecretsDir(cfg, "beta"), "OLD.age")); !os.IsNotExist(err) {
		t.Fatal("消したはずの秘密が残っている")
	}
	if _, err := os.Stat(filepath.Join(appSecretsDir(cfg, "beta"), "KEPT.age")); err != nil {
		t.Fatalf("残すべき秘密が消えた: %v", err)
	}
}

// 壊れたものが一つでもあれば、引き取り自体を止める。一部だけ新しいという
// 状態にすると、どの値が効いているのか分からなくなる。
func TestAdoptAppSecretsKeepsTheOldSetWhenSomethingIsBroken(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	useTempRuntimeDir(t)

	src := inboxSecretsDir("beta")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "GOOD.age"), []byte(armored), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := adoptAppSecrets(cfg, "beta"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "BAD.age"), []byte("not age"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := adoptAppSecrets(cfg, "beta"); err == nil {
		t.Fatal("壊れた暗号文を受け入れた")
	}
	if _, err := os.Stat(filepath.Join(appSecretsDir(cfg, "beta"), "GOOD.age")); err != nil {
		t.Fatalf("既存の秘密が失われた: %v", err)
	}
}
