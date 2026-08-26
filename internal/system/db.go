package system

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DBNames は 1 アプリに対応する DB とロールの名前。
//
// すべてアプリ名から導出する。マニフェストに書かせないのは、アプリ側の宣言で
// 他アプリの DB 名を指せてしまうと境界にならないため。
type DBNames struct {
	Database string // オーナーロールと同名。PostgreSQL の所有権はここから来る
	Owner    string // DDL。migration だけが使う
	App      string // DML のみ。runtime と cleanup が使う
}

// NamesFor はアプリ名から DB 名一式を返す。
func NamesFor(app string) DBNames {
	return DBNames{Database: app, Owner: app, App: app + "_app"}
}

// SecretName は Vault 上での秘密の名前を返す。
func (n DBNames) SecretName(role string) string {
	if role == "owner" {
		return n.Database + "-db-owner"
	}
	return n.Database + "-db-app"
}

// ProvisionSQL は DB とロールを収束させる SQL を組み立てる。
//
// ここで発行するのは「どの table があるかに依存しない」ものだけに限る。
// per-table GRANT や ALTER DEFAULT PRIVILEGES は出さない。出すと pgschema が
// 宣言されていない権限として毎回 REVOKE し、次の収束で復活する振動になり、
// その間アプリが permission denied になる。table 単位の権限はアプリ側の
// schema ファイルで宣言する。
func ProvisionSQL(n DBNames, appPassword string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	// DB と owner ロールはコンテナの初期化が作る (POSTGRES_DB / POSTGRES_USER)。
	// ここで作るのは app ロールだけ。
	//
	// CREATE ROLE に IF NOT EXISTS が無いので DO block で冪等化する。
	p(`DO $$ BEGIN`)
	p(`  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE "%s" LOGIN; END IF;`, n.App, n.App)
	p(`END $$;`)

	// 毎回流す。金庫が正で DB がそれに追従する。金庫を作り直した場合でも
	// 食い違いが残らない。
	p(`ALTER ROLE "%s" WITH PASSWORD '%s';`, n.App, appPassword)

	// app には所有権を与えない。DDL は owner だけが行う。
	p(`ALTER ROLE "%s" NOSUPERUSER NOCREATEDB NOCREATEROLE;`, n.App)
	return b.String()
}

// GrantSQL は接続に必要な最小の権限を与える SQL を返す。対象 DB に接続して実行する。
func GrantSQL(n DBNames) string {
	return fmt.Sprintf(
		"GRANT CONNECT ON DATABASE \"%s\" TO \"%s\";\nGRANT USAGE ON SCHEMA public TO \"%s\";\n",
		n.Database, n.App, n.App)
}

// Conn はアプリ専用の PostgreSQL への接続先。
//
// アプリごとに 1 インスタンス立てるので、共有インスタンスのような「postgres
// ユーザで peer 認証」は使わない。ソケットのディレクトリと owner の資格情報で
// 繋ぐ。owner はそのインスタンスの中では superuser で、他のアプリの DB は
// そもそも別プロセス・別データディレクトリなので触れない。
type Conn struct {
	SocketDir string
	Owner     string
	Password  string
}

// EnsureDatabase は DB の中のロールと権限を収束させる。
//
// DB そのものと owner ロールはコンテナの初期化が作る (POSTGRES_DB と
// POSTGRES_USER)。ここで作るのは app ロールと、その最小限の権限だけ。
func EnsureDatabase(ctx context.Context, r Runner, c Conn, n DBNames, appPassword string) error {
	// SQL は stdin で渡す。argv に載せるとパスワードが ps から見える。
	if _, err := runPsql(ctx, r, c, n.Database, ProvisionSQL(n, appPassword)); err != nil {
		return fmt.Errorf("%s のロール作成に失敗しました: %w", n.Database, err)
	}
	if _, err := runPsql(ctx, r, c, n.Database, GrantSQL(n)); err != nil {
		return fmt.Errorf("%s の権限付与に失敗しました: %w", n.Database, err)
	}
	// 問い合わせごとの所要時間を残す。これが無いと「アプリが遅い」までしか
	// 分からず、SQL なのかアプリのコードなのかを切り分けられない。
	//
	// アプリの DB には作らない。あちらは pgschema が宣言どおりに保つので、
	// 宣言に無いものを消そうとする。拡張が持つビューは直接消せないため、
	// 適用そのものが失敗する (実際に踏んだ)。
	//
	// 収集は共有メモリ側が行い、ビューはどの DB から見ても全体が見える。
	// 管理用の postgres データベースに 1 つあれば足りる。
	if _, err := runPsql(ctx, r, c, "postgres",
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements;"); err != nil {
		return fmt.Errorf("pg_stat_statements を用意できません: %w", err)
	}
	// 以前アプリの DB に作っていたぶんを片付ける。残すと pgschema が
	// 消そうとして失敗し続ける。
	if _, err := runPsql(ctx, r, c, n.Database,
		"DROP EXTENSION IF EXISTS pg_stat_statements;"); err != nil {
		return fmt.Errorf("%s の pg_stat_statements を片付けられません: %w", n.Database, err)
	}
	return nil
}

func runPsql(ctx context.Context, r Runner, c Conn, db, sql string) ([]byte, error) {
	// パスワードは環境変数で渡す。argv に載せると ps から見える。
	// SQL も stdin で渡す。こちらにもパスワードが混ざることがある。
	return r.RunEnv(ctx, []byte(sql), []string{"PGPASSWORD=" + c.Password},
		"psql", "-v", "ON_ERROR_STOP=1",
		"-h", c.SocketDir, "-U", c.Owner, "-d", db)
}

// WaitReady は目的の DB が使えるようになるまで待つ。
//
// コンテナの起動は非同期で、初回は initdb が走るぶん時間がかかる。待たずに
// 続けると、収束のたびに「たまたま間に合ったか」で結果が変わる。
//
// postgres ではなく db を見るのが要点。公式イメージは初期化の途中で一時的な
// サーバを立て、そこで POSTGRES_DB を作る。この間 postgres には繋がるので、
// postgres で判定すると「応答したのに目的の DB が無い」という窓を通り抜けて
// しまう。実際 e2e が
//
//	beta のロール作成に失敗しました: ... database "beta" does not exist
//
// で間欠的に落ちていた。
// readyInterval は再試行の間隔。テストから縮められるよう var にしてある。
var readyInterval = 2 * time.Second

func WaitReady(ctx context.Context, r Runner, c Conn, db string, tries int) error {
	var last error
	for i := 0; i < tries; i++ {
		if _, err := runPsql(ctx, r, c, db, "SELECT 1;"); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
	return fmt.Errorf("%s の DB %s が使えるようになりません: %w", c.SocketDir, db, last)
}

// VerifyAppGrants は、アプリのロールが実際にテーブルを使えることを確かめる。
//
// schema は宣言側 (各リポジトリの .sql) が GRANT を書くが、そこに書かれた
// ロール名と yunirun が作るロール名が食い違っていても、pgschema は自分が
// 作ったロールへ淡々と GRANT するだけで成功してしまう。結果、アプリが接続する
// ロールは何の権限も持たないまま起動する。
//
// これは静かに壊れる。健康確認の経路が DB を触らなければ 200 を返し続け、
// 実際に露見するのは DB を使う画面を誰かが開いたときになる。
//
// テーブルが 1 つも無いなら何も言わない (最初の migration より前)。
// テーブルがあるのにアプリのロールがどれ 1 つ触れないなら、それは設定の
// 食い違いであって正常な状態ではない。個々のテーブルまでは見ない。
// 意図して runtime に見せないテーブルはありうる。
func VerifyAppGrants(ctx context.Context, r Runner, c Conn, n DBNames) error {
	const q = `SELECT count(*), count(*) FILTER (
	  WHERE has_table_privilege($ROLE$%s$ROLE$, c.oid, 'SELECT')
	     OR has_table_privilege($ROLE$%s$ROLE$, c.oid, 'INSERT'))
	FROM pg_class c
	WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r';`

	out, err := runPsql(ctx, r, c, n.Database,
		"-- verify\n"+fmt.Sprintf("\\pset tuples_only on\n\\pset format unaligned\n"+q, n.App, n.App))
	if err != nil {
		return fmt.Errorf("%s の権限を確認できません: %w", n.Database, err)
	}
	total, granted, err := parseGrantCounts(string(out))
	if err != nil {
		return err
	}
	if total > 0 && granted == 0 {
		return fmt.Errorf(
			"%s に %d 個のテーブルがありますが、ロール %s はどれにも権限を持っていません。"+
				"schema 側の GRANT 先のロール名が %s と一致しているか確認してください",
			n.Database, total, n.App, n.App)
	}
	return nil
}

// parseGrantCounts は psql の unaligned 出力から 2 つの数を取り出す。
func parseGrantCounts(out string) (total, granted int, err error) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 2 {
			continue
		}
		t, e1 := strconv.Atoi(strings.TrimSpace(f[0]))
		g, e2 := strconv.Atoi(strings.TrimSpace(f[1]))
		if e1 != nil || e2 != nil {
			continue
		}
		return t, g, nil
	}
	return 0, 0, fmt.Errorf("権限の確認結果を読み取れません: %q", out)
}

// DropDatabase は DB とロールを落とす。
//
// 順序が要る。ロールは所有物があると落とせないので、DB を先に落とし、
// 残った権限を DROP OWNED で外してからロールを消す。
//
// これを呼ぶのは yunirun remove --drop-database だけ。収束の経路からは
// 決して呼ばない。宣言を書き間違えただけでデータが消えるのは割に合わない。
func DropDatabase(ctx context.Context, r Runner, c Conn, n DBNames) error {
	// 接続が残っていると DROP DATABASE が拒否される。先に切る。
	sql := fmt.Sprintf(`
DO $$ BEGIN
  PERFORM pg_terminate_backend(pid) FROM pg_stat_activity
   WHERE datname = '%s' AND pid <> pg_backend_pid();
END $$;
DROP DATABASE IF EXISTS "%s";
`, n.Database, n.Database)
	if _, err := runPsql(ctx, r, c, "postgres", sql); err != nil {
		return fmt.Errorf("%s を落とせません: %w", n.Database, err)
	}
	for _, role := range []string{n.App, n.Owner} {
		drop := fmt.Sprintf(`
DO $$ BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
    EXECUTE format('DROP OWNED BY %%I', '%s');
    EXECUTE format('DROP ROLE %%I', '%s');
  END IF;
END $$;
`, role, role, role)
		if _, err := runPsql(ctx, r, c, "postgres", drop); err != nil {
			return fmt.Errorf("ロール %s を落とせません: %w", role, err)
		}
	}
	return nil
}
