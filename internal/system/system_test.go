package system

import (
	"context"
	"os"
	"slices"
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

// RunEnv は env を記録に含めない。argv に秘密が載っていないことを見るのが
// この擬似 Runner の役目で、環境変数で渡すのは正しい経路であるため。
func (f *fakeRunner) RunEnv(ctx context.Context, stdin []byte, _ []string, name string, args ...string) ([]byte, error) {
	return f.Run(ctx, stdin, name, args...)
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
	sql := ProvisionSQL(NamesFor("fighter"), "pw2") + GrantSQL(NamesFor("fighter"))
	for _, bad := range []string{"ON ALL TABLES", "ALTER DEFAULT PRIVILEGES", "ON ALL SEQUENCES"} {
		if strings.Contains(sql, bad) {
			t.Fatalf("%q を発行している:\n%s", bad, sql)
		}
	}
}

// DB と owner ロールはコンテナの初期化が作る。ここで作り直すと、初期化と
// 収束のどちらが正なのかが曖昧になる。
func TestProvisionSQLLeavesTheDatabaseAndOwnerToTheContainer(t *testing.T) {
	sql := ProvisionSQL(NamesFor("fighter"), "pw2")
	for _, bad := range []string{"CREATE DATABASE", "ALTER DATABASE", `CREATE ROLE "fighter" `} {
		if strings.Contains(sql, bad) {
			t.Fatalf("%q を発行している:\n%s", bad, sql)
		}
	}
	// app ロールは作る。所有権は与えない。
	if !strings.Contains(sql, `CREATE ROLE "fighter_app" LOGIN`) {
		t.Fatalf("app ロールを作っていない:\n%s", sql)
	}
	if !strings.Contains(sql, `NOSUPERUSER`) {
		t.Fatalf("app ロールの権限を絞っていない:\n%s", sql)
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
	conn := Conn{SocketDir: "/var/lib/yunirun-db/fighter/sock", Owner: "fighter", Password: owner}
	if err := EnsureDatabase(context.Background(), r, conn, NamesFor("fighter"), app); err != nil {
		t.Fatal(err)
	}
	// argv は ps から見える。パスワードは stdin 経由でしか渡してはいけない。
	if r.argvContains(owner) || r.argvContains(app) {
		t.Fatalf("パスワードが argv に載っている: %+v", r.calls)
	}
	// app のパスワードは SQL に載るので stdin から渡る。
	if !strings.Contains(r.calls[0].stdin, app) {
		t.Fatal("app のパスワードが stdin から渡されていない")
	}
	// owner のパスワードは接続に使うだけで SQL には現れない。環境変数で渡す。
	if strings.Contains(r.calls[0].stdin, owner) {
		t.Fatal("owner のパスワードが SQL に混ざっている")
	}
}

// 繋ぐのはこのアプリ専用 PostgreSQL のソケットだけ。共有インスタンスへ
// 向けると、他アプリの DB が同じ経路の先に居ることになる。
func TestEnsureDatabaseConnectsToTheAppsOwnSocket(t *testing.T) {
	r := &fakeRunner{}
	conn := Conn{SocketDir: "/var/lib/yunirun-db/fighter/sock", Owner: "fighter", Password: "pw"}
	if err := EnsureDatabase(context.Background(), r, conn, NamesFor("fighter"), "app"); err != nil {
		t.Fatal(err)
	}
	c := r.calls[0]
	if c.name != "psql" {
		t.Fatalf("psql を直接呼んでいない: %s %v", c.name, c.args)
	}
	if !slices.Contains(c.args, conn.SocketDir) {
		t.Fatalf("自分のソケットを指していない: %v", c.args)
	}
	// 共有インスタンスの経路が残っていないこと。
	if slices.Contains(c.args, "/run/postgresql") {
		t.Fatalf("共有のソケットを指している: %v", c.args)
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

// 番号が他のグループに取られている状態で続行すると、ユーザが他人のグループに
// 属し、そのグループに付けた読み取り権限が意図しない相手へ渡る。
func TestEnsureGroupRefusesWhenGIDTakenByAnother(t *testing.T) {
	r := &fakeRunner{}
	// gid 0 は root が持っている。
	err := ensureGroup(context.Background(), r, "yunirun-nonexistent-xyz", 0)
	if err == nil {
		t.Fatal("使用中の gid で続行してしまった")
	}
	if len(r.calls) != 0 {
		t.Fatal("groupadd を試みている")
	}
}

// 既に同じ名前と番号で存在するのは収束済み。作り直さない。
func TestEnsureGroupIsIdempotentForExistingGroup(t *testing.T) {
	r := &fakeRunner{}
	if err := ensureGroup(context.Background(), r, "root", 0); err != nil {
		t.Fatalf("収束済みなのに失敗した: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("groupadd を試みている")
	}
}
