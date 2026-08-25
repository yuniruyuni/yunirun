package main

import (
	"strings"
	"testing"
)

// 戻り先を残す。1 つしか残さないと、新しい版が壊れていたときに手元に
// 戻れる版が無くなる。
func TestKeepsEnoughGenerationsToRollBack(t *testing.T) {
	if KeepImages < 2 {
		t.Fatalf("残す世代が少なすぎる: %d", KeepImages)
	}
}

// 何を消すかが説明に出ること。黙って消えるのが一番困る。
func TestPruneSaysWhatItRemoves(t *testing.T) {
	got := describePrune("ghcr.io/x/post")
	if !strings.Contains(got, "ghcr.io/x/post") || !strings.Contains(got, "残す") {
		t.Fatalf("説明が足りない: %s", got)
	}
}

// 消すのは対象の image に限る。reference で絞らないと、他アプリの image まで
// 巻き込む。
func TestPruneIsScopedToOneImage(t *testing.T) {
	r := &recordingRunner{out: map[string]string{"podman": "a\nb\nc\nd\ne\n"}}
	pruneImages(t.Context(), r, "ghcr.io/x/post")
	var listed bool
	for _, c := range r.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "images") {
			listed = true
			if !strings.Contains(j, "reference=ghcr.io/x/post") {
				t.Fatalf("対象を絞っていない: %s", j)
			}
		}
	}
	if !listed {
		t.Fatal("一覧を取っていない")
	}
}

// 新しい方を残す。--sort created は古い順なので、切るのは前側。
func TestPruneRemovesTheOldestFirst(t *testing.T) {
	// 古い順に a b c d e。KeepImages=3 なら a と b が消える。
	r := &recordingRunner{out: map[string]string{"podman": "a\nb\nc\nd\ne\n"}}
	pruneImages(t.Context(), r, "img")
	var removed []string
	for _, c := range r.calls {
		if len(c) >= 3 && c[1] == "rmi" {
			removed = append(removed, c[2])
		}
	}
	want := []string{"a", "b"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Fatalf("消したもの %v (期待 %v)", removed, want)
	}
}
