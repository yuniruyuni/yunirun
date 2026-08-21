package alloc

import "testing"

func names() []string {
	return []string{"web", "costume", "template", "lom"}
}

func TestForIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	a := For(names(), DefaultBase())
	b := For([]string{"lom", "template", "costume", "web"}, DefaultBase())

	for n, av := range a {
		if b[n] != av {
			t.Fatalf("%s: 入力順で割り当てが変わった: %+v != %+v", n, av, b[n])
		}
	}
}

// 番号の重複はそのまま権限の越境になる。以前 uid を既存ユーザと重ねてしまい、
// あるアプリの deploy ユーザが別アプリの DB パスワードを読める状態を作った。
func TestForAssignsNoDuplicates(t *testing.T) {
	got := For(names(), DefaultBase())

	seen := map[int]string{}
	claim := func(kind string, v int, app string) {
		t.Helper()
		key := v
		if prev, ok := seen[key]; ok {
			t.Fatalf("%s=%d が %s と %s で重複した", kind, v, prev, app)
		}
		seen[key] = app + "/" + kind
	}
	for app, a := range got {
		claim("uid", a.UID, app)
		claim("frontend", a.Frontend, app)
		claim("blue", a.Blue, app)
		claim("green", a.Green, app)
	}
}

// NixOS は system user の uid を 400-999 から動的に割り当てる。そこに固定値を
// 置くと、既に使われている番号を踏んでグループが別ユーザと共有される。
func TestDefaultBaseAvoidsNixOSSystemUIDRange(t *testing.T) {
	for _, a := range For(names(), DefaultBase()) {
		if a.UID >= 400 && a.UID <= 999 {
			t.Fatalf("uid=%d は NixOS の system uid 範囲 (400-999) と衝突する", a.UID)
		}
		if a.UID != a.GID {
			t.Fatalf("uid=%d と gid=%d が食い違っている", a.UID, a.GID)
		}
	}
}

func TestUserIsNamespaced(t *testing.T) {
	if got := User("web"); got != "yunirun-web" {
		t.Fatalf("User(web) = %q", got)
	}
}

// subuid 帯が重なると、あるアプリのコンテナ内 uid が別アプリのファイル所有者と
// 一致してしまう。
func TestSubUIDRangesDoNotOverlap(t *testing.T) {
	got := For(names(), DefaultBase())
	type span struct{ lo, hi int }
	var spans []span
	for _, a := range got {
		spans = append(spans, span{a.SubUID, a.SubUID + SubUIDSize - 1})
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].lo <= spans[j].hi && spans[j].lo <= spans[i].hi {
				t.Fatalf("subuid 帯が重なっている: %+v と %+v", spans[i], spans[j])
			}
		}
	}
}

// NixOS は 100000 付近から subuid を配る。そこに重ねると既存ユーザと衝突する。
func TestSubUIDBaseAvoidsNixOSRange(t *testing.T) {
	for _, a := range For(names(), DefaultBase()) {
		if a.SubUID < 1000000 {
			t.Fatalf("subuid=%d は NixOS の割り当て帯に近すぎる", a.SubUID)
		}
	}
}
