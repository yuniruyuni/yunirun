package alloc

import (
	"fmt"
	"math/rand"
	"testing"
)

// 割り当ての性質は「ある入力で成り立つ」ではなく「あらゆる操作列で成り立つ」
// ことが要る。個別の例を並べるテストでは、たまたま試さなかった順序で破れる。
//
// ここでは操作列をランダムに生成して不変条件を検査する。網羅ではないが、
// 手で書いた例より遥かに広い空間を通る。

// invariants は台帳がいつでも満たすべき条件。
func invariants(t *testing.T, l *Ledger, b Base, live map[string]Alloc) {
	t.Helper()

	// (1) 番号は重複しない。重複はそのまま権限の越境になる。
	seen := map[int]string{}
	for name, a := range l.Entries {
		for kind, v := range map[string]int{
			"uid": a.UID, "frontend": a.Frontend, "blue": a.Blue, "green": a.Green,
		} {
			key := hash(kind, v)
			if prev, ok := seen[key]; ok && prev != name {
				t.Fatalf("%s=%d が %s と %s で重複", kind, v, prev, name)
			}
			seen[key] = name
		}
	}

	// (2) subuid の帯が重ならない。重なるとコンテナ内 uid が別アプリの
	//     ファイル所有者と一致する。
	for n1, a1 := range l.Entries {
		for n2, a2 := range l.Entries {
			if n1 >= n2 {
				continue
			}
			if a1.SubUID < a2.SubUID+SubUIDSize && a2.SubUID < a1.SubUID+SubUIDSize {
				t.Fatalf("subuid 帯が重なる: %s(%d) と %s(%d)", n1, a1.SubUID, n2, a2.SubUID)
			}
		}
	}

	// (3) 一度配ったアプリの割り当ては、そのアプリが台帳に居る限り変わらない。
	//     変わると稼働中のコンテナが旧ポートに取り残される。
	for name, want := range live {
		if got, ok := l.Entries[name]; ok && got != want {
			t.Fatalf("%s の割り当てが動いた: %+v -> %+v", name, want, got)
		}
	}

	// (4) NixOS が動的に配る帯を踏まない。
	for name, a := range l.Entries {
		if a.UID >= 400 && a.UID <= 999 {
			t.Fatalf("%s の uid=%d が NixOS の system uid 帯", name, a.UID)
		}
	}
}

func hash(kind string, v int) int {
	// 種類ごとに空間を分ける。uid と port は別の空間なので同値でも衝突ではない。
	switch kind {
	case "uid":
		return v
	default:
		return 1_000_000 + v
	}
}

// TestLedgerHoldsInvariantsUnderRandomOperations は、追加と削除をランダムに
// 繰り返しても不変条件が保たれることを確かめる。
func TestLedgerHoldsInvariantsUnderRandomOperations(t *testing.T) {
	b := DefaultBase()

	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		l := &Ledger{Entries: map[string]Alloc{}}
		// live は「今も宣言されているアプリ」の割り当て。ここが動いてはいけない。
		live := map[string]Alloc{}
		var declared []string

		for step := 0; step < 40; step++ {
			if len(declared) > 0 && rng.Intn(3) == 0 {
				// 削除。番号は再利用されてよいが、残っているアプリは動かない。
				i := rng.Intn(len(declared))
				gone := declared[i]
				declared = append(declared[:i], declared[i+1:]...)
				l.Remove(gone)
				delete(live, gone)
			} else {
				// 追加。名前は意図的にばらけさせ、辞書順の前後に入るようにする。
				name := fmt.Sprintf("app%c%d", 'a'+rune(rng.Intn(26)), rng.Intn(100))
				if !contains(declared, name) {
					declared = append(declared, name)
				}
			}

			got := l.Ensure(declared, b)
			for _, n := range declared {
				live[n] = got[n]
			}
			invariants(t, l, b, live)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestLedgerIsIdempotent は、同じ宣言に対して何度収束させても結果が変わらない
// ことを確かめる。converge は繰り返し呼ばれるので、これが破れると毎回何かが
// 書き換わって無用な再起動を招く。
func TestLedgerIsIdempotent(t *testing.T) {
	b := DefaultBase()
	rng := rand.New(rand.NewSource(42))

	for trial := 0; trial < 100; trial++ {
		var names []string
		for i := 0; i < rng.Intn(10)+1; i++ {
			names = append(names, fmt.Sprintf("a%d", rng.Intn(50)))
		}
		l := &Ledger{Entries: map[string]Alloc{}}
		first := l.Ensure(names, b)
		for i := 0; i < 5; i++ {
			again := l.Ensure(names, b)
			for n, v := range first {
				if again[n] != v {
					t.Fatalf("%s が %d 回目で変わった: %+v -> %+v", n, i+2, v, again[n])
				}
			}
		}
	}
}

// TestAllocationIsInjectiveByExhaustion は、index から番号への写像が単射である
// ことを全数検査で示す。
//
// ランダム試験と違い、これは「反証が見つからなかった」ではなく「反例が存在
// しない」ことの証明になる。定義域が有限 (index は 0..N-1) で、N を運用上の
// 上限まで取れるため。
//
// 単射性が破れると別々のアプリが同じ uid やポートを持つ。実際にそれが起きて、
// あるアプリの deploy ユーザが別アプリの DB パスワードを読める状態になった。
func TestAllocationIsInjectiveByExhaustion(t *testing.T) {
	b := DefaultBase()

	// 検査範囲は割り当ての上限そのもの。上限を超える index は allocate が
	// 拒否するので、この範囲を尽くせば「反例が存在しない」ことの証明になる。
	const maxApps = MaxApps

	uids := make(map[int]int, maxApps)
	ports := make(map[int]int, maxApps)
	subs := make(map[int]int, maxApps)

	for i := 0; i < maxApps; i++ {
		a := at(i, b)

		if prev, ok := uids[a.UID]; ok {
			t.Fatalf("uid=%d が index %d と %d で衝突", a.UID, prev, i)
		}
		uids[a.UID] = i

		for kind, p := range map[string]int{"frontend": a.Frontend, "blue": a.Blue, "green": a.Green} {
			if prev, ok := ports[p]; ok {
				t.Fatalf("%s port=%d が index %d と %d で衝突", kind, p, prev, i)
			}
			ports[p] = i
		}

		// subuid は帯なので、開始値だけでなく範囲の重なりを見る。
		for s := range subs {
			if a.SubUID < s+SubUIDSize && s < a.SubUID+SubUIDSize {
				t.Fatalf("subuid 帯が index %d と %d で重なる", subs[s], i)
			}
		}
		subs[a.SubUID] = i

		// NixOS の system uid 帯を踏まない。
		if a.UID >= 400 && a.UID <= 999 {
			t.Fatalf("index %d の uid=%d が NixOS の帯", i, a.UID)
		}
	}
}

// TestPortsNeverCollideWithReservedRanges は、割り当てるポートが予約域や
// 既知のサービスと衝突しないことを全数で示す。
func TestPortsNeverCollideWithReservedRanges(t *testing.T) {
	b := DefaultBase()

	for i := 0; i < MaxApps; i++ {
		a := at(i, b)
		for kind, p := range map[string]int{"frontend": a.Frontend, "blue": a.Blue, "green": a.Green} {
			// 1024 未満は特権ポート。rootless podman では bind できない。
			if p < 1024 {
				t.Fatalf("index %d の %s port=%d が特権ポート", i, kind, p)
			}
			// 短命ポートと重なると、たまたま使われていて bind に失敗する。
			if p >= 32768 {
				t.Fatalf("index %d の %s port=%d が短命ポート帯", i, kind, p)
			}
			// PostgreSQL。
			if p == 5432 {
				t.Fatalf("index %d の %s port が PostgreSQL と衝突", i, kind)
			}
		}
	}
}

// TestAllocationStopsAtTheBound は、上限を超えたら配らないことを確かめる。
//
// 配ってしまうと短命ポート帯 (32768 以降) に入り、たまたま使われている番号と
// 衝突して bind に失敗する。エラーにならず静かに壊れるので、境界で止める。
func TestAllocationStopsAtTheBound(t *testing.T) {
	b := DefaultBase()
	l := &Ledger{Entries: map[string]Alloc{}}

	var names []string
	for i := 0; i < MaxApps+5; i++ {
		names = append(names, fmt.Sprintf("app%04d", i))
	}

	_, err := l.EnsureStrict(names, b)
	if err == nil {
		t.Fatal("上限を超えても配ってしまった")
	}
	if len(l.Entries) != MaxApps {
		t.Fatalf("配った数が上限と違う: %d", len(l.Entries))
	}
}

// TestBoundKeepsPortsOutOfEphemeralRange は、上限の設定自体が正しいことを示す。
func TestBoundKeepsPortsOutOfEphemeralRange(t *testing.T) {
	b := DefaultBase()
	last := at(MaxApps-1, b)
	if last.Green >= 32768 {
		t.Fatalf("上限 %d でも短命ポート帯に入る: %d", MaxApps, last.Green)
	}
	// 上限のすぐ外では破れることも示す。境界が意味を持っている証拠になる。
	beyond := at(2467, b)
	if beyond.Frontend < 32768 {
		t.Fatalf("index 2467 で短命ポート帯に入るはずが %d", beyond.Frontend)
	}
}
