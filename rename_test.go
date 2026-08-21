package main

import (
	"os"
	"path/filepath"
	"testing"
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
