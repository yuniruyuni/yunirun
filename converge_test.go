package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 書き換えを避けるのは mtime を動かさないため。反映そのものは内容の変化に
// かかわらず毎回行う (ずれた状態から抜け出せなくなるため)。
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

// HAProxy の unit は After=yunirun-converge.service なので、その job は
// converge が終わるまで走れない。完了を待つと自分が終わらないと進まない
// job を待つことになり、起動時に固まる。--no-block が要る。
//
// try-reload-or-restart なのは、converge が先に走って HAProxy がまだ
// 動いていない場合に reload だと失敗するため。この形なら何もしない。
func TestReloadHAProxyDoesNotWaitForItsOwnJob(t *testing.T) {
	r := &recordingRunner{}
	if err := reloadHAProxy(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("呼び出しが 1 件ではない: %v", r.calls)
	}
	got := strings.Join(r.calls[0], " ")
	want := "systemctl --no-block try-reload-or-restart yunirun-haproxy.service"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
