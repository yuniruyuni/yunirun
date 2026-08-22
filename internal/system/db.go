package system

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
func ProvisionSQL(n DBNames, ownerPassword, appPassword string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	// CREATE ROLE / DATABASE に IF NOT EXISTS が無いので DO block で冪等化する。
	p(`DO $$ BEGIN`)
	p(`  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE "%s" LOGIN; END IF;`, n.Owner, n.Owner)
	p(`  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE "%s" LOGIN; END IF;`, n.App, n.App)
	p(`END $$;`)

	p(`ALTER ROLE "%s" WITH PASSWORD '%s';`, n.Owner, ownerPassword)
	p(`ALTER ROLE "%s" WITH PASSWORD '%s';`, n.App, appPassword)

	p(`SELECT 'CREATE DATABASE "%s" OWNER "%s"'`, n.Database, n.Owner)
	p(`  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%s') \gexec`, n.Database)

	// 所有権が apply の権限の源になる。所有者は自分が作ったオブジェクトを
	// 所有するので明示的な GRANT は要らず、pgschema が権限を宣言的に管理しても
	// 自分自身を締め出すことがない。
	p(`ALTER DATABASE "%s" OWNER TO "%s";`, n.Database, n.Owner)
	return b.String()
}

// GrantSQL は接続に必要な最小の権限を与える SQL を返す。対象 DB に接続して実行する。
func GrantSQL(n DBNames) string {
	return fmt.Sprintf(
		"GRANT CONNECT ON DATABASE \"%s\" TO \"%s\";\nGRANT USAGE ON SCHEMA public TO \"%s\";\n",
		n.Database, n.App, n.App)
}

// EnsureDatabase は DB とロールを収束させる。
//
// psql は postgres ユーザとして実行する。pg_hba が local に md5 を要求する
// 構成 (コンテナから Unix ソケット経由で繋ぐために必要) では、root は peer
// 認証を使えずパスワードを求められるため。postgres だけが peer で入れる。
func EnsureDatabase(ctx context.Context, r Runner, n DBNames, ownerPassword, appPassword string) error {
	// SQL は stdin で渡す。argv に載せるとパスワードが ps から見える。
	if _, err := runPsql(ctx, r, "postgres", ProvisionSQL(n, ownerPassword, appPassword)); err != nil {
		return fmt.Errorf("%s の DB 作成に失敗しました: %w", n.Database, err)
	}
	if _, err := runPsql(ctx, r, n.Database, GrantSQL(n)); err != nil {
		return fmt.Errorf("%s の権限付与に失敗しました: %w", n.Database, err)
	}
	return nil
}

func runPsql(ctx context.Context, r Runner, db, sql string) ([]byte, error) {
	return r.Run(ctx, []byte(sql),
		"runuser", "-u", "postgres", "--",
		"psql", "-v", "ON_ERROR_STOP=1", "-d", db)
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
func VerifyAppGrants(ctx context.Context, r Runner, n DBNames) error {
	const q = `SELECT count(*), count(*) FILTER (
	  WHERE has_table_privilege($ROLE$%s$ROLE$, c.oid, 'SELECT')
	     OR has_table_privilege($ROLE$%s$ROLE$, c.oid, 'INSERT'))
	FROM pg_class c
	WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r';`

	out, err := runPsql(ctx, r, n.Database,
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
