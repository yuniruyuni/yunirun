package system

import (
	"context"
	"os"
	"strings"
	"testing"
)

type call struct {
	stdin string
	name  string
	args  []string
}

type fakeRunner struct {
	calls []call
	out   []byte
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{stdin: string(stdin), name: name, args: args})
	return f.out, nil
}

func (f *fakeRunner) argvContains(s string) bool {
	for _, c := range f.calls {
		for _, a := range c.args {
			if strings.Contains(a, s) {
				return true
			}
		}
		if strings.Contains(c.name, s) {
			return true
		}
	}
	return false
}

// 生成したパスワードに引用符やバックスラッシュが入ると、SQL や shell へ渡す際の
// エスケープ漏れが致命的な形で現れる。文字種を絞って問題ごと避けている。
func TestNewPasswordUsesOnlySafeCharacters(t *testing.T) {
	for i := 0; i < 200; i++ {
		p, err := NewPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != PasswordLength {
			t.Fatalf("長さが %d", len(p))
		}
		if strings.ContainsAny(p, `'"\ ;$`+"`\n") {
			t.Fatalf("危険な文字を含む: %q", p)
		}
	}
}

func TestNewPasswordDiffersEachTime(t *testing.T) {
	a, _ := NewPassword()
	b, _ := NewPassword()
	if a == b {
		t.Fatal("同じ値が返った")
	}
}

// per-table GRANT を撒くと pgschema に毎回 REVOKE され、次の収束で復活する
// 振動になる。その間アプリは permission denied になる。
func TestProvisionSQLOmitsPerTableGrants(t *testing.T) {
	sql := ProvisionSQL(NamesFor("fighter"), "pw1", "pw2") + GrantSQL(NamesFor("fighter"))
	for _, bad := range []string{"ON ALL TABLES", "ALTER DEFAULT PRIVILEGES", "ON ALL SEQUENCES"} {
		if strings.Contains(sql, bad) {
			t.Fatalf("%q を発行している:\n%s", bad, sql)
		}
	}
}

func TestProvisionSQLMakesOwnerOwnTheDatabase(t *testing.T) {
	sql := ProvisionSQL(NamesFor("fighter"), "pw1", "pw2")
	// apply の権限は GRANT ではなく所有権から来る。
	if !strings.Contains(sql, `ALTER DATABASE "fighter" OWNER TO "fighter";`) {
		t.Fatalf("所有権を設定していない:\n%s", sql)
	}
}

func TestGrantSQLGivesAppOnlyConnectAndUsage(t *testing.T) {
	sql := GrantSQL(NamesFor("fighter"))
	if !strings.Contains(sql, "GRANT CONNECT") || !strings.Contains(sql, "GRANT USAGE ON SCHEMA public") {
		t.Fatalf("最小の権限が無い:\n%s", sql)
	}
}

func TestEnsureDatabaseNeverPutsPasswordInArgv(t *testing.T) {
	r := &fakeRunner{}
	const owner, app = "OWNERSECRET", "APPSECRET"
	if err := EnsureDatabase(context.Background(), r, NamesFor("fighter"), owner, app); err != nil {
		t.Fatal(err)
	}
	// argv は ps から見える。パスワードは stdin 経由でしか渡してはいけない。
	if r.argvContains(owner) || r.argvContains(app) {
		t.Fatalf("パスワードが argv に載っている: %+v", r.calls)
	}
	if !strings.Contains(r.calls[0].stdin, owner) {
		t.Fatal("stdin から渡されていない")
	}
}

// pg_hba が local に md5 を要求する構成では root は peer 認証を使えない。
// postgres だけが peer で入れる。
func TestEnsureDatabaseRunsPsqlAsPostgres(t *testing.T) {
	r := &fakeRunner{}
	if err := EnsureDatabase(context.Background(), r, NamesFor("fighter"), "a", "b"); err != nil {
		t.Fatal(err)
	}
	c := r.calls[0]
	if c.name != "runuser" || c.args[1] != "postgres" {
		t.Fatalf("postgres として実行していない: %s %v", c.name, c.args)
	}
}

func TestVaultPutSendsValueOnStdinNotArgv(t *testing.T) {
	r := &fakeRunner{out: []byte("ciphertext")}
	v := Vault{Dir: t.TempDir(), HostRecipient: "age1host", AdminRecipient: "age1admin", Runner: r}
	const secret = "PLAINTEXTVALUE"
	if err := v.Put(context.Background(), "fighter-db-owner", secret); err != nil {
		t.Fatal(err)
	}
	if r.argvContains(secret) {
		t.Fatalf("値が argv に載っている: %+v", r.calls)
	}
	if r.calls[0].stdin != secret {
		t.Fatal("stdin から渡されていない")
	}
}

// ホストを失っても復旧できるよう、管理者鍵にも必ず暗号化する。
func TestVaultPutAlsoEncryptsToAdminRecipient(t *testing.T) {
	r := &fakeRunner{out: []byte("ciphertext")}
	v := Vault{Dir: t.TempDir(), HostRecipient: "age1host", AdminRecipient: "age1admin", Runner: r}
	if err := v.Put(context.Background(), "x", "v"); err != nil {
		t.Fatal(err)
	}
	if !r.argvContains("age1admin") {
		t.Fatalf("管理者鍵が受信者に入っていない: %+v", r.calls[0].args)
	}
}

// 作り直すと DB 側のパスワードと食い違う。converge から繰り返し呼ばれる。
func TestVaultPutIsIdempotent(t *testing.T) {
	r := &fakeRunner{out: []byte("ciphertext")}
	v := Vault{Dir: t.TempDir(), HostRecipient: "age1host", Runner: r}
	for i := 0; i < 3; i++ {
		if err := v.Put(context.Background(), "x", "v"); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("2 回目以降も暗号化している: %d 回", len(r.calls))
	}
}

func TestNamesForDerivesEverythingFromAppName(t *testing.T) {
	n := NamesFor("fighter")
	if n.Database != "fighter" || n.Owner != "fighter" || n.App != "fighter_app" {
		t.Fatalf("%+v", n)
	}
	if n.SecretName("owner") != "fighter-db-owner" || n.SecretName("app") != "fighter-db-app" {
		t.Fatalf("secret 名が想定と違う")
	}
}

func TestEnsureSubIDsReplacesStaleLineForSameUser(t *testing.T) {
	dir := t.TempDir()
	// 実ファイルを差し替えられないので、appendLineIfMissing を直接検証する。
	p := dir + "/subuid"
	if err := appendLineIfMissing(p, "app:100:65536"); err != nil {
		t.Fatal(err)
	}
	if err := appendLineIfMissing(p, "other:200:65536"); err != nil {
		t.Fatal(err)
	}
	// 番号が変わった同じユーザを足すと、古い行は残ってはいけない。
	// 残ると podman がどちらを使うか不定になる。
	if err := appendLineIfMissing(p, "app:300:65536"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if strings.Contains(s, "app:100:65536") {
		t.Fatalf("古い行が残っている:\n%s", s)
	}
	if !strings.Contains(s, "app:300:65536") || !strings.Contains(s, "other:200:65536") {
		t.Fatalf("必要な行が無い:\n%s", s)
	}
}

func TestAppendLineIfMissingIsIdempotent(t *testing.T) {
	p := t.TempDir() + "/subuid"
	for i := 0; i < 3; i++ {
		if err := appendLineIfMissing(p, "app:100:65536"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "app:100:65536") != 1 {
		t.Fatalf("重複している:\n%s", b)
	}
}
