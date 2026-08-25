package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// migrateTimeout は schema 適用の上限。
const migrateTimeout = "600"

// runMigrate は schema を適用する。root で動く。
//
// deploy ユーザがこれを直接実行しないのが要点。migration は owner ロール (DDL)
// を使うため、その資格情報を非特権ユーザに渡さない。deploy ユーザは systemd
// 経由でこの処理を起動できるが、owner パスワードを読むことはできない。
//
// 残る境界として、deploy ユーザは実行するタグを選べるので root に任意のタグの
// image を実行させられる。ただし新しい image を push することはできない
// (CI の deploy job のトークンは packages: read で、push は別 job)。
// Cloud Run で builder と deployer を分けていたのと同じ性質。
func runMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("使い方: yunirun migrate <app>")
	}
	app := fs.Arg(0)
	if os.Geteuid() != 0 {
		return fmt.Errorf("migrate は root で実行してください")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	repo, ok := cfg.Apps[app]
	if !ok {
		return fmt.Errorf("%s は取り込まれていません", app)
	}

	m, err := manifest.Load(storedManifestPath(cfg, app))
	if err != nil {
		return err
	}
	w, ok := m.Workloads["migration"]
	if !ok {
		fmt.Printf("%s には migration がありません\n", app)
		return nil
	}

	tag, err := readTag(cfg, app)
	if err != nil {
		return err
	}
	owner := strings.SplitN(repo, "/", 2)[0]
	// 省略時はアプリ本体と同じ image を使う。fighter の cleanup がこの形。
	base := w.Image
	if base == "" {
		base = m.App.Image
	}
	image := imageRef(owner, app, base)

	names := dbNamesFor(app, m)
	r := system.ExecRunner{}

	fmt.Printf("==> %s:%s で schema を適用\n", image, tag)

	// GHCR は非公開なので pull に認証が要る。deploy が置いたトークンを使う。
	// このトークンは job の終了とともに失効するので、長期の資格情報にはならない。
	authfile := filepath.Join(inboxDir(app), "migrate-auth.json")
	defer os.Remove(authfile)
	if token, err := os.ReadFile(tokenPath(app)); err == nil {
		if _, err := r.Run(ctx, token, "podman", "login", "ghcr.io",
			"--username", owner, "--password-stdin", "--authfile", authfile); err != nil {
			return fmt.Errorf("ghcr.io にログインできません: %w", err)
		}
		if _, err := r.Run(ctx, nil, "podman", "pull", "--authfile", authfile, image+":"+tag); err != nil {
			return fmt.Errorf("%s を取得できません: %w", image, err)
		}
	}

	sockDir := filepath.Join(cfg.DatabaseDir(), app, "sock")

	args2 := []string{
		"run", "--rm",
		// owner パスワードは root しか読めないファイルから読む。
		"--env-file", cfg.EnvPath(app, "migration.env"),
		// DB へは TCP ではなく Unix ソケットで繋ぐ。渡すのはこのアプリ専用
		// PostgreSQL のソケットだけなので、他アプリの DB へは到達しようがない。
		"--volume", sockDir + ":/run/postgresql",
		"--env", "PGHOST=/run/postgresql",
		"--env", "PGPORT=5432",
		"--env", "DB_USER=" + names.Owner,
		"--env", "DB_NAME=" + names.Database,
		// migrate.sh の変数名がアプリごとに揃っていないため両方渡す。
		"--env", "DB_APP_NAME=" + names.Database,
	}
	args2 = append(args2, w.Args...)
	args2 = append(args2, image+":"+tag)

	out, err := r.Run(ctx, nil, "timeout", append([]string{migrateTimeout, "podman"}, args2...)...)
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("schema の適用に失敗しました: %w", err)
	}

	// 適用が通っただけでは足りない。schema 側が書く GRANT の宛先と、yunirun が
	// 作るロールの名前が食い違っていても pgschema は成功するので、アプリの
	// ロールが実際にテーブルを使えるところまで見る。
	//
	// 接続には owner の資格情報を使う。この経路は root しか通らない。
	ownerPassword, err := readEnvValue(cfg.EnvPath(app, "migration.env"), "DB_PASSWORD")
	if err != nil {
		return err
	}
	conn := system.Conn{SocketDir: sockDir, Owner: names.Owner, Password: ownerPassword}
	if err := system.VerifyAppGrants(ctx, r, conn, names); err != nil {
		return err
	}

	// migration の image も pull するだけで消していなかった。デプロイのたびに
	// root 側へ 1 つずつ積み上がる。適用が通った後に片付ける。
	PruneAfterDeploy(ctx, r, image)
	return nil
}

func readTag(_ *config.Config, app string) (string, error) {
	b, err := os.ReadFile(tagPath(app))
	if err != nil {
		return "", fmt.Errorf("適用するタグが分かりません: %w", err)
	}
	tag := trimLine(string(b))
	if !tagRE.MatchString(tag) {
		return "", fmt.Errorf("タグに使えない文字が含まれています: %q", tag)
	}
	return tag, nil
}

// readEnvValue は KEY=VALUE 形式のファイルから 1 つの値を読む。
func readEnvValue(path, key string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s を読めません: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s に %s がありません", path, key)
}
