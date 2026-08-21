package main

import (
	"context"
	"encoding/json"
	"errors"
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

	// 割り当ては台帳から取る。名前順のインデックスから導出すると、アルファベット順で
	// 前に入る名前を足したときに既存アプリの uid とポートがずれる。稼働中の
	// コンテナは旧ポートのままで HAProxy は新ポートを見るため停止し、ファイルの
	// 所有者 uid も実ユーザとずれてデータが読めなくなる。
	ledger, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return err
	}
	allocs, err := ledger.EnsureStrict(cfg.Names(), cfg.Base())
	if err != nil {
		return err
	}
	// 実体を作る前に保存する。途中で落ちても、次の収束が同じ番号を使う。
	if err := ledger.Save(cfg.LedgerPath()); err != nil {
		return err
	}

	hostRecipient, err2 := hostAgeRecipient(ctx, r, cfg)
	if err2 != nil {
		return err2
	}

	// 1 つのアプリの失敗で全体を止めない。
	//
	// マニフェストはアプリ側のリポジトリから来るので、他人の事故がこちらの
	// 収束を巻き込む形になってはいけない。失敗したアプリはスキップし、残りを
	// 収束させてから最後にまとめて報告する。
	//
	// スキップしたアプリを HAProxy から外さないのも意図的。外すと現に動いている
	// コンテナへの経路が切れて停止する。宣言が読めないことと、既に動いている
	// ものを止めてよいことは別。
	var apps []render.App
	var failed []error
	for _, name := range cfg.Names() {
		a := allocs[name]
		app, err := convergeApp(ctx, r, cfg, name, a, hostRecipient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "!!! %s: %v\n", name, err)
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			// 収束はできなかったが、既存の経路は保つ。
			if prev, ok := previousApp(cfg, name, a); ok {
				apps = append(apps, prev)
			}
			continue
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

	if len(failed) > 0 {
		return fmt.Errorf("%d 個のアプリを収束できませんでした: %w", len(failed), errors.Join(failed...))
	}
	return nil
}

// previousApp は収束に失敗したアプリについて、HAProxy の経路だけを保つための
// 最小限の情報を返す。
//
// マニフェストが読めない状態なので、ポートとヘルスパスは既定値を使う。既に
// 動いているコンテナが別のポートを使っていれば経路は復旧しないが、少なくとも
// 他のアプリの設定を巻き込んで消すことはない。
func previousApp(cfg *config.Config, name string, a alloc.Alloc) (render.App, bool) {
	m, err := manifest.Parse([]byte("{}"))
	if err != nil {
		return render.App{}, false
	}
	return render.App{
		Name:     name,
		User:     alloc.User(name),
		Alloc:    a,
		Manifest: m,
	}, true
}

func convergeApp(ctx context.Context, r system.Runner, cfg *config.Config,
	name string, a alloc.Alloc, hostRecipient string) (render.App, error) {

	user := alloc.User(name)
	// ホームは stateDir の外に置く。
	//
	// stateDir には台帳と秘密が入るので 0700 root にしておきたいが、そうすると
	// その配下は何であれアプリのユーザから辿れない (パスの途中に x が無いと、
	// 末端の権限に関係なく届かない。エラーは "Not a directory" という誤解を
	// 招く形で出る)。
	home := filepath.Join(cfg.HomeDir(), name)

	if err := system.EnsureUser(ctx, r, user, a, home); err != nil {
		return render.App{}, err
	}
	if err := system.EnsureSubIDs(user, a); err != nil {
		return render.App{}, err
	}
	if err := system.EnsureLinger(ctx, r, user); err != nil {
		return render.App{}, err
	}

	// マニフェストは deploy 時に運ばれて残る。まだ一度も deploy されていなければ
	// 規約の既定値で動く。
	if err := ensureInbox(name, a); err != nil {
		return render.App{}, err
	}
	m, err := manifest.Load(manifestPath(cfg, name))
	if err != nil {
		return render.App{}, err
	}

	names := system.NamesFor(name)
	runtimeEnv := filepath.Join(runtimeDir, name, "runtime.env")
	migrationEnv := filepath.Join(runtimeDir, name, "migration.env")

	// アプリ固有の秘密を集める。値は /run/agenix から読む。
	//
	// 実体を infra 側 (agenix) に置くのは、アプリのリポジトリに秘密を置かない
	// という方針のため。アプリ側は名前だけを宣言する。
	runtimeEnvVars := map[string]string{}
	for envName, secretName := range m.App.Secrets {
		v, err := os.ReadFile(filepath.Join("/run/agenix", secretName))
		if err != nil {
			return render.App{}, fmt.Errorf("秘密 %s を読めません: %w", secretName, err)
		}
		// 末尾改行を落とす。agenix の値に乗っていることがあり、環境変数へ
		// そのまま入ると認証などが静かに失敗する。
		runtimeEnvVars[envName] = strings.TrimRight(string(v), "\r\n")
	}

	// DB を使わないアプリには作らない。作ると消し忘れた資源が溜まるうえ、
	// 不要な資格情報が生成される。
	if m.App.Database {
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

		// runtime 用には app パスワードだけを入れ、owner パスワードは root しか
		// 読めない別ファイルに置く。これで runtime は owner の資格情報を
		// 構造的に持てない。
		runtimeEnvVars["DB_PASSWORD"] = app
		if err := writeEnvFile(migrationEnv, map[string]string{"DB_PASSWORD": owner}, 0, 0); err != nil {
			return render.App{}, err
		}
	}

	// 秘密を tmpfs へ展開する。読めるのはこのアプリのユーザだけ。
	if len(runtimeEnvVars) > 0 {
		if err := writeEnvFile(runtimeEnv, runtimeEnvVars, a.UID, a.GID); err != nil {
			return render.App{}, err
		}
	}

	// deploy が読むための情報を tmpfs へ置く。
	//
	// deploy はアプリのユーザで動くので、root 専用の stateDir 配下 (台帳など) を
	// 読めない。そもそも deploy に必要なのは自分のポートだけで、他アプリの割り当てを
	// 含む台帳全体を見せる理由が無い。
	if err := writeAppInfo(name, a, m); err != nil {
		return render.App{}, err
	}
	if err := ensureInbox(name, a); err != nil {
		return render.App{}, err
	}

	ra := render.App{
		Name:     name,
		User:     user,
		Alloc:    a,
		Manifest: m,
		LocalTag: "localhost/" + name + ":current",
	}
	if m.App.Database {
		ra.DBName = names.Database
		ra.DBUser = names.App
	}
	if len(runtimeEnvVars) > 0 {
		ra.EnvFile = runtimeEnv
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

// AppInfo は deploy が必要とする情報。converge が書き、deploy が読む。
type AppInfo struct {
	Blue   int    `json:"blue"`
	Green  int    `json:"green"`
	Health string `json:"health"`
}

func appInfoPath(app string) string {
	return filepath.Join(runtimeDir, app, "app.json")
}

func writeAppInfo(app string, a alloc.Alloc, m *manifest.Manifest) error {
	if err := os.MkdirAll(filepath.Dir(appInfoPath(app)), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(AppInfo{Blue: a.Blue, Green: a.Green, Health: m.App.Health})
	if err != nil {
		return err
	}
	// 秘密は含まないので誰が読んでもよい。
	return os.WriteFile(appInfoPath(app), b, 0o644)
}

// inboxDir は deploy がマニフェストとタグを置く場所。
//
// stateDir は root 専用にしておきたい (台帳や秘密が入っている) ので、アプリが
// 書く場所だけを分ける。converge と migrate はここから読む。
func inboxDir(app string) string { return filepath.Join(runtimeDir, app, "inbox") }

func manifestPath(_ *config.Config, app string) string {
	return filepath.Join(inboxDir(app), "yunirun.jsonc")
}

func tagPath(app string) string { return filepath.Join(inboxDir(app), "tag") }

// ensureInbox は受け渡し用ディレクトリをアプリのユーザ所有で作る。
func ensureInbox(app string, a alloc.Alloc) error {
	d := inboxDir(app)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.Chown(d, a.UID, a.GID)
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
