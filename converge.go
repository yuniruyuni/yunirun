package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/render"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runtimeDir は秘密を展開する場所。tmpfs なので再起動で消える。
const runtimeDir = "/run/yunirun"

func runConverge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("converge", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	haproxyOut := fs.String("haproxy-out", "/etc/yunirun/haproxy.cfg", "生成する HAProxy 設定")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("converge は root で実行してください")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	r := system.ExecRunner{}
	allocs := cfg.Allocs()

	hostRecipient, err := hostAgeRecipient(ctx, r, cfg)
	if err != nil {
		return err
	}

	var apps []render.App
	for _, name := range cfg.Names() {
		a := allocs[name]
		app, err := convergeApp(ctx, r, cfg, name, a, hostRecipient)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		apps = append(apps, app)
		fmt.Printf("==> %s (uid=%d frontend=%d)\n", name, a.UID, a.Frontend)
	}

	if err := os.MkdirAll(filepath.Dir(*haproxyOut), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*haproxyOut, []byte(render.HAProxy(apps)), 0o644); err != nil {
		return err
	}
	fmt.Printf("==> HAProxy 設定を書き出しました: %s\n", *haproxyOut)
	return nil
}

func convergeApp(ctx context.Context, r system.Runner, cfg *config.Config,
	name string, a alloc.Alloc, hostRecipient string) (render.App, error) {

	user := alloc.User(name)
	home := filepath.Join(cfg.StateDir, "home", name)

	if err := system.EnsureUser(ctx, r, user, a, home); err != nil {
		return render.App{}, err
	}
	if err := system.EnsureSubIDs(user, a); err != nil {
		return render.App{}, err
	}
	if err := system.EnsureLinger(ctx, r, user); err != nil {
		return render.App{}, err
	}

	// マニフェストは deploy 時に運ばれて状態として残る。まだ一度も deploy
	// されていなければ規約の既定値で動く。
	m, err := manifest.Load(manifestPath(cfg, name))
	if err != nil {
		return render.App{}, err
	}

	names := system.NamesFor(name)
	vault := system.Vault{
		Dir:            filepath.Join(cfg.StateDir, "secrets", name),
		HostKeyPath:    cfg.HostKeyPath,
		HostRecipient:  hostRecipient,
		AdminRecipient: cfg.AdminRecipient,
		Runner:         r,
	}
	owner, app, err := ensureDBPasswords(ctx, vault, names)
	if err != nil {
		return render.App{}, err
	}
	if err := system.EnsureDatabase(ctx, r, names, owner, app); err != nil {
		return render.App{}, err
	}

	// 秘密を tmpfs へ展開する。runtime 用には app パスワードだけを入れ、
	// owner パスワードは root しか読めない別ファイルに置く。これで runtime は
	// owner の資格情報を構造的に持てない。
	runtimeEnv := filepath.Join(runtimeDir, name, "runtime.env")
	if err := writeEnvFile(runtimeEnv, map[string]string{"DB_PASSWORD": app}, a.UID, a.GID); err != nil {
		return render.App{}, err
	}
	migrationEnv := filepath.Join(runtimeDir, name, "migration.env")
	if err := writeEnvFile(migrationEnv, map[string]string{"DB_PASSWORD": owner}, 0, 0); err != nil {
		return render.App{}, err
	}

	ra := render.App{
		Name:     name,
		User:     user,
		Alloc:    a,
		Manifest: m,
		LocalTag: "localhost/" + name + ":current",
		DBName:   names.Database,
		DBUser:   names.App,
		EnvFile:  runtimeEnv,
	}

	units := map[string]string{}
	for _, color := range render.Colors {
		units[fmt.Sprintf("%s-%s.container", name, color)] = render.ContainerUnit(ra, color)
	}
	// migration は root 側で実行するので、ここでは unit を作らない。
	// 作るとアプリのユーザから owner の資格情報へ手が届いてしまう。
	for wname, w := range m.Workloads {
		if wname == "migration" {
			continue
		}
		spec := workloadSpec(cfg, name, wname, w, names, migrationEnv, runtimeEnv, repoOwner(cfg, name))
		units[fmt.Sprintf("%s-%s.container", name, wname)] = render.WorkloadUnit(ra, wname, spec)
		if w.Schedule != "" {
			units[fmt.Sprintf("%s-%s.timer", name, wname)] = render.TimerUnit(ra, wname, spec)
		}
	}
	if err := system.WriteUnits(home, a.UID, a.GID, units); err != nil {
		return render.App{}, err
	}
	if err := system.ReloadUserUnits(ctx, r, user, a.UID); err != nil {
		return render.App{}, err
	}
	return ra, nil
}

// workloadSpec は 1 ワークロードの生成情報を組み立てる。
//
// role が owner のものはここへ来ない (migration のみで、root 側が扱う)。
// 万一来ても app の env ファイルしか渡さないので、owner パスワードは漏れない。
func workloadSpec(cfg *config.Config, app, name string, w manifest.Workload,
	n system.DBNames, migrationEnv, runtimeEnv, owner string) render.WorkloadSpec {

	image := w.Image
	if image == "" {
		image = app
	}
	if !strings.Contains(image, "/") {
		image = "ghcr.io/" + owner + "/" + image
	}
	spec := render.WorkloadSpec{
		Image:    image + ":current",
		Args:     w.Args,
		EnvFile:  runtimeEnv,
		DBUser:   n.App,
		Schedule: w.Schedule,
	}
	if w.Role == manifest.RoleOwner {
		spec.EnvFile = migrationEnv
		spec.DBUser = n.Owner
	}
	return spec
}

func repoOwner(cfg *config.Config, app string) string {
	return strings.SplitN(cfg.Apps[app], "/", 2)[0]
}

func ensureDBPasswords(ctx context.Context, v system.Vault, n system.DBNames) (owner, app string, err error) {
	for _, role := range []string{"owner", "app"} {
		pw, e := system.NewPassword()
		if e != nil {
			return "", "", e
		}
		// Put は既にあれば何もしない。作り直すと DB 側と食い違う。
		if e := v.Put(ctx, n.SecretName(role), pw); e != nil {
			return "", "", e
		}
	}
	if owner, err = v.Get(ctx, n.SecretName("owner")); err != nil {
		return "", "", err
	}
	if app, err = v.Get(ctx, n.SecretName("app")); err != nil {
		return "", "", err
	}
	return owner, app, nil
}

// writeEnvFile は秘密を含む環境変数ファイルを tmpfs 上に書く。
func writeEnvFile(path string, kv map[string]string, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b []byte
	for k, v := range kv {
		b = append(b, []byte(k+"="+v+"\n")...)
	}
	// 先に 0600 で作ってから所有者を移す。順序を逆にすると、一瞬でも
	// 意図しない相手が読める窓ができる。
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o400)
}

func manifestPath(cfg *config.Config, app string) string {
	return filepath.Join(cfg.StateDir, "manifests", app+".jsonc")
}

// hostAgeRecipient は ssh のホスト鍵から導いた age 公開鍵を返す。
func hostAgeRecipient(ctx context.Context, r system.Runner, cfg *config.Config) (string, error) {
	out, err := r.Run(ctx, nil, "age-keygen", "-y", cfg.HostKeyPath)
	if err != nil {
		return "", fmt.Errorf("ホスト鍵から公開鍵を導けません: %w", err)
	}
	return trimLine(string(out)), nil
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
