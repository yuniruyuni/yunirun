package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/config"
)

// /etc/subuid は全ユーザで共有するファイルなので、消しすぎると他アプリの
// rootless podman が動かなくなる。改名対象の行だけが消えることを固定する。
func TestDropSubIDLinesRemovesOnlyTheNamedUser(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subuid")
	body := "yunirun-template2:4000000:65536\n" +
		"yunirun-costume2:4065536:65536\n" +
		"yunirun-tags2:4327680:65536\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dropSubIDLines(p, "yunirun-template2"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "yunirun-costume2:4065536:65536\n" +
		"yunirun-tags2:4327680:65536\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 名前は接頭辞ではなくコロンまでの完全一致で見る。template を消すつもりで
// template2 まで巻き添えにすると、そのアプリの podman が動かなくなる。
func TestDropSubIDLinesDoesNotMatchOnPrefix(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subuid")
	body := "yunirun-template:4000000:65536\n" +
		"yunirun-template2:4065536:65536\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dropSubIDLines(p, "yunirun-template"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "yunirun-template2:4065536:65536\n" {
		t.Fatalf("接頭辞で巻き添えにしている: %q", got)
	}
}

// 対象が無いときにファイルを空にしてしまわないこと。
func TestDropSubIDLinesKeepsFileWhenNothingMatches(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subuid")
	body := "yunirun-costume2:4065536:65536\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dropSubIDLines(p, "yunirun-nope"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Fatalf("got %q, want %q", got, body)
	}
}

// ファイルが無いのは異常ではない (subgid が無い構成もある)。
func TestDropSubIDLinesIgnoresMissingFile(t *testing.T) {
	if err := dropSubIDLines(filepath.Join(t.TempDir(), "nope"), "x"); err != nil {
		t.Fatalf("存在しないファイルで失敗している: %v", err)
	}
}

// 改名で宣言を置き去りにすると、新しい名前には宣言が無い状態になり、converge が
// 既定値で収束する。DB も env も workload も消えたまま「成功」と報告するので、
// 次の deploy まで気付けない。
func TestRenameCarriesTheStoredManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}

	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"app":{"database":true}}`)
	if err := os.WriteFile(storedManifestPath(cfg, "old"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := moveStoredManifest(cfg, "old", "new"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(storedManifestPath(cfg, "new"))
	if err != nil {
		t.Fatalf("新しい名前に宣言が無い: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("宣言が変わっている: %s", got)
	}
	if _, err := os.Stat(storedManifestPath(cfg, "old")); !os.IsNotExist(err) {
		t.Fatal("古い名前の宣言が残っている")
	}
}

// まだ一度も deploy されていないアプリの改名は失敗しない。
func TestRenameWithoutAStoredManifestIsFine(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if err := moveStoredManifest(cfg, "old", "new"); err != nil {
		t.Fatal(err)
	}
}
