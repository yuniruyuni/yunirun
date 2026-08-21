package system

import (
	"context"
	"fmt"
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
func EnsureDatabase(ctx context.Context, r Runner, n DBNames, ownerPassword, appPassword string) error {
	// SQL は stdin で渡す。argv に載せるとパスワードが ps から見える。
	if _, err := r.Run(ctx, []byte(ProvisionSQL(n, ownerPassword, appPassword)),
		"psql", "-v", "ON_ERROR_STOP=1", "-d", "postgres"); err != nil {
		return fmt.Errorf("%s の DB 作成に失敗しました: %w", n.Database, err)
	}
	if _, err := r.Run(ctx, []byte(GrantSQL(n)),
		"psql", "-v", "ON_ERROR_STOP=1", "-d", n.Database); err != nil {
		return fmt.Errorf("%s の権限付与に失敗しました: %w", n.Database, err)
	}
	return nil
}
