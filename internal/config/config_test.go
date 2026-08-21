package config

import (
	"os"
	"testing"
)

func parse(t *testing.T, s string) (*Config, error) {
	t.Helper()
	p := t.TempDir() + "/config.json"
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// アプリ名は Linux ユーザ名・DB 名・unit 名の元になるので、ここが緩いと
// それらすべてに任意の文字列が流れ込む。
func TestLoadRejectsUnsafeAppName(t *testing.T) {
	for _, bad := range []string{"../x", "a-b", "A", "1x", "a b"} {
		if _, err := parse(t, `{"domain":"e.com","stateDir":"/s","apps":{"`+bad+`":"o/r"}}`); err == nil {
			t.Fatalf("アプリ名 %q を受け入れてしまった", bad)
		}
	}
}

func TestLoadRejectsUnsafeRepo(t *testing.T) {
	for _, bad := range []string{"noslash", "a/b/c", "o/r;rm"} {
		if _, err := parse(t, `{"domain":"e.com","stateDir":"/s","apps":{"x":"`+bad+`"}}`); err == nil {
			t.Fatalf("リポジトリ %q を受け入れてしまった", bad)
		}
	}
}

func TestLoadAcceptsUnderscoreAppName(t *testing.T) {
	// 既存の stream_tag_inventory / streamer_post は下線を含む。
	if _, err := parse(t, `{"domain":"e.com","stateDir":"/s","apps":{"stream_tag_inventory":"o/r"}}`); err != nil {
		t.Fatalf("下線を含む名前が拒否された: %v", err)
	}
}

func TestHostnameAndAllocs(t *testing.T) {
	c, err := parse(t, `{"domain":"example.com","stateDir":"/s","apps":{"web":"o/web","costume":"o/costume"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Hostname("web"); got != "web.example.com" {
		t.Fatalf("Hostname = %q", got)
	}
	a, err := c.Allocs()
	if err != nil {
		t.Fatal(err)
	}
	if a["costume"].Index == a["web"].Index {
		t.Fatalf("番号が重複している: %+v", a)
	}
}

// 既存の仕組みと並行して動かす間、ポート帯が重なると片方が bind に失敗する。
func TestAllocsHonorsCustomBases(t *testing.T) {
	c, err := parse(t, `{"domain":"e.com","stateDir":"/s","basePort":8200,"baseUID":7000,"apps":{"web":"o/w"}}`)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.Allocs()
	if err != nil {
		t.Fatal(err)
	}
	a := all["web"]
	if a.Frontend != 8200 || a.UID != 7000 {
		t.Fatalf("基準値が効いていない: %+v", a)
	}
}
