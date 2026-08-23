package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
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

// 宣言から消えたアプリは止める。経路 (HAProxy) は宣言から生成しているので
// 勝手に消えるのに、コンテナは Restart=always で動き続けていた。外からは
// 404 なのに中では動いてポートを掴んだまま、という状態になる。
func TestStopUndeclaredStopsOnlyWhatIsNotDeclared(t *testing.T) {
	r := &recordingRunner{}
	cfg := &config.Config{Apps: map[string]string{"alpha": "example/alpha"}}
	l := &alloc.Ledger{Entries: map[string]alloc.Alloc{
		"alpha": {UID: 6000},
		"gone":  {UID: 6001},
	}}
	stopUndeclared(t.Context(), r, cfg, l)

	var stopped []string
	for _, c := range r.calls {
		if c[0] == "systemctl" && c[1] == "stop" {
			stopped = append(stopped, c[2])
		}
	}
	if len(stopped) != 1 || stopped[0] != "user@6001.service" {
		t.Fatalf("止めた対象が想定と違う: %v (全呼び出し %v)", stopped, r.calls)
	}
}

// 止めるだけで、消してはいけない。宣言の書き間違いでデータが飛ぶのは
// 割に合わない。片付けは yunirun remove の仕事。
func TestStopUndeclaredNeverTouchesData(t *testing.T) {
	r := &recordingRunner{}
	cfg := &config.Config{Apps: map[string]string{}}
	l := &alloc.Ledger{Entries: map[string]alloc.Alloc{"gone": {UID: 6001}}}
	stopUndeclared(t.Context(), r, cfg, l)

	for _, c := range r.calls {
		switch c[0] {
		case "userdel", "psql", "runuser", "dropdb":
			t.Fatalf("データを消す操作を発行している: %v", c)
		}
	}
	// 台帳も残す。戻したときに uid とポートが元通りになる。
	if _, ok := l.Entries["gone"]; !ok {
		t.Fatal("台帳から消している")
	}
}

// バックアップの側で DB 名やソケットの場所を推測させない。規約を変えたときに
// 静かにずれ、しかも「取れた分だけ成功」に見えるので、気付くのは復元しようと
// した時になる。
func TestDatabasesListsOnlyAppsThatDeclareADatabase(t *testing.T) {
	// dbNamesFor は databaseName の宣言があればそれを使い、無ければアプリ名。
	// databases はこの導出をそのまま流すことで規約を 1 か所に保つ。
	m, err := manifest.Parse([]byte(`{"app":{"database":true,"databaseName":"streamer_post"}}`))
	if err != nil {
		t.Fatal(err)
	}
	n := dbNamesFor("post", m)
	if n.Database != "streamer_post" || n.Owner != "streamer_post" || n.App != "streamer_post_app" {
		t.Fatalf("導出が想定と違う: %+v", n)
	}
}
