package alloc

import (
	"path/filepath"
	"testing"
)

// これがこの台帳を作った理由そのもの。名前順のインデックスから導出していた頃、
// アルファベット順で前に入る名前を足すと既存アプリの uid とポートが全てずれた。
// 稼働中のコンテナは旧ポートのままで HAProxy は新ポートを見る (= 停止) うえ、
// ファイルの所有者 uid も実ユーザとずれてデータが読めなくなる。
func TestEnsureKeepsExistingAllocationWhenEarlierNameIsAdded(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()

	before := l.Ensure([]string{"template"}, b)["template"]

	// アルファベット順で前に来る名前を足す。
	after := l.Ensure([]string{"aaa", "template"}, b)

	if after["template"] != before {
		t.Fatalf("既存アプリの割り当てが動いた: %+v → %+v", before, after["template"])
	}
	if after["aaa"].UID == before.UID {
		t.Fatalf("新規アプリが既存の番号を奪った: %+v", after["aaa"])
	}
}

func TestEnsureIsStableAcrossManyAdditions(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()

	first := l.Ensure([]string{"web"}, b)["web"]
	for _, n := range []string{"aaa", "bbb", "ccc", "aab"} {
		got := l.Ensure([]string{n, "web"}, b)["web"]
		if got != first {
			t.Fatalf("%s の追加で web の割り当てが動いた: %+v", n, got)
		}
	}
}

func TestEnsureNeverGivesDuplicateNumbers(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()
	got := l.Ensure([]string{"d", "a", "c", "b", "e"}, b)

	seen := map[int]string{}
	for name, a := range got {
		if prev, ok := seen[a.UID]; ok {
			t.Fatalf("uid=%d が %s と %s で重複した", a.UID, prev, name)
		}
		seen[a.UID] = name
	}
}

// 消えたアプリの番号は再利用できる。ただし再利用の前に古いユーザとデータが
// 消えている必要がある。
func TestRemoveFreesTheNumberForReuse(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()

	a := l.Ensure([]string{"gone"}, b)["gone"]
	l.Remove("gone")
	reused := l.Ensure([]string{"fresh"}, b)["fresh"]

	if reused.UID != a.UID {
		t.Fatalf("番号が再利用されていない: %d → %d", a.UID, reused.UID)
	}
}

func TestLedgerSurvivesRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allocations.json")
	l := &Ledger{Entries: map[string]Alloc{}}
	want := l.Ensure([]string{"web", "fighter"}, DefaultBase())
	if err := l.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	// 読み直した台帳が同じ番号を返さないと、再起動のたびに割り当てが変わる。
	after := got.Ensure([]string{"web", "fighter"}, DefaultBase())
	for n := range want {
		if after[n] != want[n] {
			t.Fatalf("%s の割り当てが保存後に変わった: %+v → %+v", n, want[n], after[n])
		}
	}
}

func TestLoadLedgerTreatsMissingFileAsEmpty(t *testing.T) {
	l, err := LoadLedger(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("ファイルが無いのはエラーではない: %v", err)
	}
	if len(l.Entries) != 0 {
		t.Fatal("空でない")
	}
}

// 名前を変えただけで新しい番号が振られると、稼働中のコンテナが旧ポートに
// 取り残されて停止する。
func TestRenameKeepsTheAllocation(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()
	before := l.Ensure([]string{"app2"}, b)["app2"]

	if !l.Rename("app2", "app") {
		t.Fatal("改名できなかった")
	}
	after := l.Ensure([]string{"app"}, b)["app"]
	if after != before {
		t.Fatalf("改名で割り当てが動いた: %+v -> %+v", before, after)
	}
	if _, stale := l.Entries["app2"]; stale {
		t.Fatal("旧名が残っている")
	}
}

// 上書きすると別のアプリの割り当てを奪う。
func TestRenameRefusesWhenTargetIsTaken(t *testing.T) {
	l := &Ledger{Entries: map[string]Alloc{}}
	b := DefaultBase()
	l.Ensure([]string{"a", "b"}, b)

	if l.Rename("a", "b") {
		t.Fatal("使用中の名前へ改名してしまった")
	}
	if len(l.Entries) != 2 {
		t.Fatalf("エントリ数が変わった: %d", len(l.Entries))
	}
}
