package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 設定を書いただけでは HAProxy は読み直さない。書いた内容と動いている
// 内容が食い違ったまま気付けなくなる。実際、アプリの改名から数時間
// HAProxy は旧名の frontend を配り続けていた。
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

// reload は start ではなく try-reload-or-restart を使う。converge が
// HAProxy より先に走った場合、reload は失敗するがこの形なら何もしない。
func TestReloadHAProxyUsesTryReloadOrRestart(t *testing.T) {
	r := &recordingRunner{}
	if err := reloadHAProxy(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("呼び出しが 1 件ではない: %v", r.calls)
	}
	c := r.calls[0]
	if c[0] != "systemctl" || c[1] != "try-reload-or-restart" ||
		c[2] != "yunirun-haproxy.service" {
		t.Fatalf("想定と違う呼び出し: %v", c)
	}
}
