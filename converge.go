package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/render"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runtimeDir は受け渡し用の置き場所。tmpfs なので再起動で消える。
//
// 消えて困らないものだけを置く。ここにあるのは deploy との受け渡し (inbox) と
// 割り当ての公開 (app.json) で、どちらも converge が書き直せる。unit が
// 起動時に読む env は消えると起動に失敗するので EnvDir 側に置く。
// テストから差し替えられるよう var にしてある。const のままだと、inbox を
// 作れない環境でテストが skip され、走っていないことに気付けない。
var runtimeDir = "/run/yunirun"

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
	// トレースの口を共有するグループは、アプリより先に作る。
	//
	// 各アプリのユーザをこのグループへ入れるので、アプリの収束より後に作ると
	// 「そんなグループは無い」で全アプリが失敗する (実際に踏んだ)。
	if cfg.Observability.Enable {
		if err := system.EnsureSharedGroup(ctx, r, render.TraceGroup); err != nil {
			return err
		}
	}

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

	// 宣言から消えたアプリを止める。
	//
	// 経路 (HAProxy) は宣言から生成しているので勝手に消えるが、コンテナは
	// user unit として Restart=always で動き続ける。外からは 404 なのに中では
	// 動いてポートを掴んだままになり、しかも再起動を跨ぐと /run 上の env が
	// 作り直されずに起動失敗へ変わる。気付きにくい壊れ方なので、経路と
	// 揃えて稼働も止める。
	//
	// 消すのは稼働だけ。ユーザ・ホーム・DB・金庫・台帳は残す。戻したく
	// なったときに台帳が残っていれば uid もポートも元通りになる。片付けは
	// yunirun remove が明示的に行う。
	stopUndeclared(ctx, r, cfg, ledger)

	if err := os.MkdirAll(filepath.Dir(*haproxyOut), 0o755); err != nil {
		return err
	}
	changed, err := writeIfChanged(*haproxyOut, []byte(render.HAProxy(apps)))
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("==> HAProxy 設定を書き出しました: %s\n", *haproxyOut)
	}

	// HAProxy 自体も unit として置く。設定より先に置く必要はないが、
	// 設定が無いまま起動すると読み込みに失敗するのでこの順にする。
	unit := render.HAProxyContainer + ".container"
	if err := system.ApplySystemUnit(ctx, r, unit, render.HAProxyUnit(cfg.HAProxyImage(), *haproxyOut)); err != nil {
		return err
	}

	if err := convergeObservability(ctx, r, cfg, filepath.Dir(*haproxyOut)); err != nil {
		return err
	}
	// 内容が変わっていなくても毎回読み直させる。
	//
	// 変わったときだけにしていたところ、ディスク上の設定と動いている設定が
	// 既にずれている状態から抜け出せなかった。実際、reload を入れる前に
	// 書かれた設定に fighter の frontend が含まれていたにもかかわらず、
	// 次の収束では「変わっていない」と判定されて反映されなかった。
	//
	// 読み直しても、動いている設定と同じなら実質何も起きない。-Ws の
	// master-worker では worker が入れ替わるだけで、既存の接続は旧 worker が
	// 処理し終えてから終わる。ずれ続けるより毎回揃える方がよい。
	if err := reloadHAProxy(ctx, r, *haproxyOut); err != nil {
		return err
	}

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
	m, err := loadManifest(cfg, name)
	if err != nil {
		return render.App{}, err
	}
	// 暗号文も宣言と同じく deploy が運んでくる。同じ場所で引き取る。
	if err := adoptAppSecrets(cfg, name); err != nil {
		return render.App{}, err
	}

	names := dbNamesFor(name, m)
	// env は永続領域に置く。unit が起動時に EnvironmentFile= で読むので、
	// tmpfs に置くと再起動のたびに converge との競争になる。
	if err := ensureEnvDir(cfg, name); err != nil {
		return render.App{}, err
	}
	runtimeEnv := cfg.EnvPath(name, "runtime.env")
	migrationEnv := cfg.EnvPath(name, "migration.env")

	// アプリ固有の秘密を集める。
	runtimeEnvVars := map[string]string{}

	// infra 側 (agenix) に置く旧方式。アプリは名前だけを宣言する。
	// アプリ側 (secrets/<ENV>.age) へ移行済みのものはここに現れない。
	for envName, secretName := range m.App.Secrets {
		v, err := os.ReadFile(filepath.Join("/run/agenix", secretName))
		if err != nil {
			return render.App{}, fmt.Errorf("秘密 %s を読めません: %w", secretName, err)
		}
		// 末尾改行を落とす。agenix の値に乗っていることがあり、環境変数へ
		// そのまま入ると認証などが静かに失敗する。
		runtimeEnvVars[envName] = strings.TrimRight(string(v), "\r\n")
	}

	// アプリ側 (secrets/<ENV>.age) に置く方式。deploy が暗号文のまま運び、
	// ここで初めて復号する。
	appSecrets, err := loadAppSecrets(ctx, r, cfg, name)
	if err != nil {
		return render.App{}, err
	}
	for envName, v := range appSecrets {
		runtimeEnvVars[envName] = v
	}

	// DB を使わないアプリには作らない。作ると消し忘れた資源が溜まるうえ、
	// 不要な資格情報が生成される。
	sockDir := ""
	if m.App.Database {
		vault := system.Vault{
			Dir:            filepath.Join(cfg.StateDir, "secrets", name),
			HostKeyPath:    cfg.HostKeyPath,
			HostRecipient:  hostRecipient,
			AdminRecipient: cfg.AdminRecipient,
			Runner:         r,
		}
		owner, app, err := resolveDBPasswords(ctx, vault, names, m.App.DatabasePasswords)
		if err != nil {
			return render.App{}, err
		}

		conn, err := ensureDatabaseContainer(ctx, r, cfg, name, a, names, owner)
		if err != nil {
			return render.App{}, err
		}
		sockDir = conn.SocketDir

		if err := system.EnsureDatabase(ctx, r, conn, names, app); err != nil {
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

	// 秘密を展開する。読めるのはこのアプリのユーザだけ。
	if len(runtimeEnvVars) > 0 {
		if err := writeEnvFile(runtimeEnv, runtimeEnvVars, a.UID, a.GID); err != nil {
			return render.App{}, err
		}
	} else if err := os.Remove(runtimeEnv); err != nil && !os.IsNotExist(err) {
		// 秘密が全部消えたら実体も消す。残すと、宣言から外したはずの値が
		// ディスク上に居座り続ける。
		return render.App{}, err
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

	// 計測基盤を立てているときだけトレースの口を渡す。無いのに渡すと、
	// 存在しない場所を mount しようとして unit が起動できなくなる。
	if cfg.Observability.Enable {
		ra.TraceSockDir = cfg.Observability.TraceSocketDir()
		// ソケットはグループで守ってある。所属していないと繋げない。
		if err := system.AddToGroup(ctx, r, user, render.TraceGroup); err != nil {
			return render.App{}, err
		}
	}
	if m.App.Database {
		ra.DBName = names.Database
		ra.DBUser = names.App
		ra.SockDir = sockDir
	}
	if len(runtimeEnvVars) > 0 {
		ra.EnvFile = runtimeEnv
	}

	units := map[string]string{}
	var timers []string
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
			tname := fmt.Sprintf("%s-%s.timer", name, wname)
			units[tname] = render.TimerUnit(ra, wname, spec)
			timers = append(timers, tname)
		}
	}
	if err := system.WriteUnits(home, a.UID, a.GID, units); err != nil {
		return render.App{}, err
	}
	if err := system.ReloadUserUnits(ctx, r, user, a.UID); err != nil {
		return render.App{}, err
	}
	// ファイルを置いて daemon-reload しただけでは timer は動かない。
	if err := system.EnableUserTimers(ctx, r, a.UID, timers); err != nil {
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
		Env:      w.Env,
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

// resolveDBPasswords は使う DB パスワードを決める。
//
// 既存の秘密が指定されていればそれを使い、無ければ生成する。指定する用途は
// 移行期間中に旧システムと DB を共有する場合。パスワードを変えると旧側の
// 稼働中コンテナが即座に認証に失敗する。
func resolveDBPasswords(ctx context.Context, v system.Vault, n system.DBNames,
	existing map[string]string) (owner, app string, err error) {

	if len(existing) == 0 {
		return ensureDBPasswords(ctx, v, n)
	}
	read := func(role string) (string, error) {
		name, ok := existing[role]
		if !ok {
			return "", fmt.Errorf("databasePasswords に %s がありません", role)
		}
		b, err := os.ReadFile(filepath.Join("/run/agenix", name))
		if err != nil {
			return "", fmt.Errorf("秘密 %s を読めません: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if owner, err = read(manifest.RoleOwner); err != nil {
		return "", "", err
	}
	if app, err = read(manifest.RoleApp); err != nil {
		return "", "", err
	}
	return owner, app, nil
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
	// 並びを固定する。map の走査順は毎回変わるので、そのまま書くと同じ内容でも
	// ファイルが変わる。生成物は決定的にしておく。
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, []byte(k+"="+kv[k]+"\n")...)
	}
	// 内容が同じなら触らない。書き直すと更新時刻が動き、設定が変わったと
	// 見なされてコンテナが毎回再起動する。
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, b) {
		return nil
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

// ensureEnvDir は env ファイルの置き場所を作る。
//
// 中の各ファイルが自分で権限を持つので、ディレクトリ自体は辿れればよい。
func ensureEnvDir(cfg *config.Config, app string) error {
	d := filepath.Dir(cfg.EnvPath(app, "x"))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.Chmod(d, 0o755)
}

// inboxDir は deploy がマニフェストとタグを置く場所。
//
// stateDir は root 専用にしておきたい (台帳や秘密が入っている) ので、アプリが
// 書く場所だけを分ける。converge と migrate はここから読む。
func inboxDir(app string) string { return filepath.Join(runtimeDir, app, "inbox") }

// manifestPath は deploy が置く受け渡し用の場所。
//
// deploy はアプリのユーザとして動くので stateDir (root 専用) へは書けない。
// tmpfs 上のこの場所へ置き、converge が root として永続領域へ引き取る。
func manifestPath(_ *config.Config, app string) string {
	return filepath.Join(inboxDir(app), "yunirun.jsonc")
}

// storedManifestPath は引き取った後の置き場所。
//
// tmpfs ではなく永続領域に置く。ここを tmpfs のままにしていたため、再起動で
// 宣言が消え、converge が「ファイルが無い」を既定値として扱って全アプリを
// 既定設定へ書き戻す状態になっていた。DB を使う宣言も env も workload も
// 消えるうえ、converge は成功として報告するので気付けない。
func storedManifestPath(cfg *config.Config, app string) string {
	return filepath.Join(cfg.StateDir, "manifests", app+".jsonc")
}

// loadManifest は宣言を読む。
//
// deploy が置いた新しいものがあればそれを永続領域へ引き取り、無ければ既に
// 引き取ってあるものを読む。どちらも無いときだけ既定値になる (まだ一度も
// deploy されていないアプリ)。
func loadManifest(cfg *config.Config, app string) (*manifest.Manifest, error) {
	stored := storedManifestPath(cfg, app)
	if b, err := os.ReadFile(manifestPath(cfg, app)); err == nil {
		if err := os.MkdirAll(filepath.Dir(stored), 0o700); err != nil {
			return nil, err
		}
		// 中身を確かめてから引き取る。壊れた宣言で既存のものを潰さない。
		if _, err := manifest.Parse(b); err != nil {
			return nil, fmt.Errorf("%s の宣言を読めません: %w", app, err)
		}
		if err := os.WriteFile(stored, b, 0o600); err != nil {
			return nil, err
		}
	}
	return manifest.Load(stored)
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

// writeIfChanged は内容が変わるときだけ書く。
//
// 書き換えを避けるのは mtime を動かさないため。変わっていないのに触ると、
// 変更を追う側 (人の目も含めて) が無用な差分を見ることになる。
func writeIfChanged(path string, want []byte) (bool, error) {
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, want) {
		return false, nil
	}
	return true, os.WriteFile(path, want, 0o644)
}

// reloadHAProxy は書き出した設定を HAProxy に読み直させる。
//
// これが無いと、設定は正しく書かれているのに反映されない。実際、アプリの
// 改名から数時間、HAProxy は旧名の frontend を配り続けていた。ポートは
// 台帳が引き継いでいたので配送自体は成り立ってしまい、新しく足した
// アプリの frontend が listen されないという形で初めて露見した。
//
// 収束のたびに無条件で呼ぶ。設定が変わったときだけにすると、既にずれて
// いる状態から抜け出せない。
//
// try-reload-or-restart を使う。まだ動いていないとき (起動直後で converge が
// 先に走った場合) に reload を送ると失敗するが、この形なら何もしない。
// 起動そのものは wantedBy が受け持つ。
//
// --no-block が要る。HAProxy の unit は After=yunirun-converge.service なので、
// その job は converge が終わるまで走れない。完了を待つと、converge は
// 自分が終わらないと進まない job を待つことになり、起動時に固まる
// (実際 VM テストが数十分返らなくなった)。
//
// 待たない代わりに、reload の成否はここでは分からない。設定の妥当性は
// ExecReload の 1 段目が検査し、失敗すれば HAProxy の unit 側に残る。
func reloadHAProxy(ctx context.Context, r system.Runner, configPath string) error {
	// 動いていなければ何もしない。起動は unit が受け持ち、そのときディスク上の
	// 設定を読むので、ここで送るものは無い。
	out, err := r.Run(ctx, nil, "podman", "ps", "--format", "{{.Names}}")
	if err != nil || !slices.Contains(strings.Fields(string(out)), render.HAProxyContainer) {
		return nil
	}
	// 先に設定を検査する。壊れた設定のまま USR2 を送ると、マスターは新しい
	// ワーカーを起こせないまま古いものを抱えることになり、何が起きたのかが
	// 分かりにくい状態になる。
	if _, err := r.Run(ctx, nil, "podman", "exec", render.HAProxyContainer,
		"haproxy", "-c", "-q", "-f", configPath); err != nil {
		return fmt.Errorf("HAProxy の設定が不正です: %w", err)
	}
	// USR2 はマスターへの再読込指示。ワーカーを新しい設定で起こし直す。
	if _, err := r.Run(ctx, nil, "podman", "kill", "--signal", "USR2",
		render.HAProxyContainer); err != nil {
		return fmt.Errorf("HAProxy を読み直せません: %w", err)
	}
	return nil
}

// stopUndeclared は台帳にあるが宣言に無いアプリを止める。
//
// 判定に台帳を使うのは、これが「yunirun が作ったもの」の一覧だから。
// ホームや Linux ユーザを走査すると、yunirun 以外が作ったものまで拾う。
func stopUndeclared(ctx context.Context, r system.Runner, cfg *config.Config, l *alloc.Ledger) {
	declared := make(map[string]bool, len(cfg.Apps))
	for _, n := range cfg.Names() {
		declared[n] = true
	}
	names := make([]string, 0, len(l.Entries))
	for n := range l.Entries {
		if !declared[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		a := l.Entries[name]
		user := alloc.User(name)
		system.StopUser(ctx, r, user, a.UID)
		// 実行時に公開していたものは消す。秘密が tmpfs 上に残り続ける
		// 理由が無い。
		_ = os.RemoveAll(filepath.Join(runtimeDir, name))
		fmt.Printf("==> %s を止めました (宣言に無い)\n", name)
		fmt.Printf("    ユーザ %s、ホーム %s、台帳の割り当ては残しています。\n",
			user, filepath.Join(cfg.HomeDir(), name))
		fmt.Printf("    片付けるなら yunirun remove %s\n", name)
	}
}

// ensureDatabaseContainer はアプリ専用 PostgreSQL を立てて、繋がるまで待つ。
//
// root 側の Quadlet として置く。アプリのユーザで動かすと、初期化に要る
// owner のパスワードをそのユーザが読めることになり、runtime が owner の
// 資格情報を持てないという分離が崩れる。
//
// データはホームの外に置く。ホームは rename や remove が捨てるので、そこに
// 置くと名前を変えただけでデータが消える。
func ensureDatabaseContainer(ctx context.Context, r system.Runner, cfg *config.Config,
	name string, a alloc.Alloc, n system.DBNames, ownerPassword string) (system.Conn, error) {

	base := filepath.Join(cfg.DatabaseDir(), name)
	dataDir := filepath.Join(base, "data")
	sockDir := filepath.Join(base, "sock")

	// data は root だけが触れればよい。sock はアプリのコンテナが辿るので、
	// 通り抜けだけ許す。中のソケット自体は PostgreSQL が 0777 で作る。
	for dir, mode := range map[string]os.FileMode{
		cfg.DatabaseDir(): 0o755, base: 0o755, dataDir: 0o700, sockDir: 0o755,
	} {
		if err := os.MkdirAll(dir, mode); err != nil {
			return system.Conn{}, err
		}
		if err := os.Chmod(dir, mode); err != nil {
			return system.Conn{}, err
		}
	}

	// 初期化に使う値。root しか読めない場所に置く。
	dbEnv := cfg.EnvPath(name, "db.env")
	if err := writeEnvFile(dbEnv, map[string]string{
		"POSTGRES_USER":     n.Owner,
		"POSTGRES_PASSWORD": ownerPassword,
		"POSTGRES_DB":       n.Database,
	}, 0, 0); err != nil {
		return system.Conn{}, err
	}

	unit := name + "-db.container"
	spec := render.DBSpec{
		Image:   cfg.DatabaseImage(),
		DataDir: dataDir,
		SockDir: sockDir,
		EnvFile: dbEnv,
		Owner:   n.Owner,
		// 資源を絞る。1 アプリ分のデータしか入らないので、共有インスタンス
		// 向けの既定値は大きすぎる。
		Args: []string{
			"-c", "shared_buffers=32MB",
			"-c", "max_connections=20",
			"-c", "autovacuum_max_workers=1",
			// 問い合わせごとの所要時間を残す。共有メモリに載せる必要が
			// あるので起動時に読み込ませる (後から有効にはできない)。
			//
			// これが無いと「アプリが遅い」までしか分からず、SQL なのか
			// アプリのコードなのかを切り分けられない。
			"-c", "shared_preload_libraries=pg_stat_statements",
		},
	}
	ra := render.App{Name: name, User: alloc.User(name), Alloc: a}
	// db.env も見張る。パスワードを入れ替えたのにコンテナが古いまま、
	// という形にならないようにする。
	if err := system.ApplySystemUnit(ctx, r, unit, render.DBUnit(ra, spec), dbEnv); err != nil {
		return system.Conn{}, err
	}

	conn := system.Conn{SocketDir: sockDir, Owner: n.Owner, Password: ownerPassword}
	// 初回は initdb が走るぶん時間がかかる。待たずに続けると、収束のたびに
	// 「たまたま間に合ったか」で結果が変わる。
	if err := system.WaitReady(ctx, r, conn, dbReadyTries); err != nil {
		return system.Conn{}, err
	}
	return conn, nil
}

// dbReadyTries は DB の応答を待つ回数。テストから縮められるよう変数にしてある。
var dbReadyTries = 60

// convergeObservability は計測基盤を立てる。
//
// 設定を先に置いてから unit を起動する。逆にすると、中身が無いまま起動して
// 失敗し、Restart=always で回り続ける。
func convergeObservability(ctx context.Context, r system.Runner, cfg *config.Config, confDir string) error {
	if !cfg.Observability.Enable {
		return nil
	}
	spec := cfg.Observability.Spec(confDir)

	// 世界読み取り可能にする。中に秘密は無く、各コンテナが別々の非 root
	// ユーザで読むため。
	//
	// 内容が同じなら触らない。書き直すと更新時刻が動き、設定が変わったと
	// 見なされてコンテナが毎回再起動する。
	for name, body := range spec.StackFiles() {
		if _, err := writeIfChanged(filepath.Join(confDir, name), []byte(body)); err != nil {
			return err
		}
	}
	// setgid を付ける。Tempo が作るソケットにこのグループを継がせるため。
	sockDir := spec.TraceSocketDir()
	if err := os.MkdirAll(sockDir, 0o2770); err != nil {
		return err
	}
	if err := system.Chgrp(sockDir, render.TraceGroup); err != nil {
		return err
	}
	if err := os.Chmod(sockDir, os.ModeSetgid|0o770); err != nil {
		return err
	}

	// ダッシュボードは別のディレクトリへ。Grafana は「ダッシュボードだけが
	// 入った場所」を見に行くので、設定ファイルと混ぜると読み込みに失敗する。
	dashDir := filepath.Join(confDir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		return err
	}
	for name, body := range spec.StackDashboards() {
		if _, err := writeIfChanged(filepath.Join(dashDir, name), []byte(body)); err != nil {
			return err
		}
	}

	// データの置き場所。中身は :U で各コンテナのユーザへ渡る。
	// 一覧は render 側に置いてある。unit の Volume とずれると起動できない。
	for _, d := range spec.StackDataDirs() {
		if d == sockDir {
			// ソケットの置き場所だけは権限が違う。上で作ってある。
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// 名前順で回す。順序が毎回変わると、失敗したときのログが読みにくい。
	units := spec.StackUnits()
	names := make([]string, 0, len(units))
	for n := range units {
		names = append(names, n)
	}
	sort.Strings(names)
	// 設定ファイルも見張る。unit だけを見ると、中身が変わっただけの場合を
	// 取りこぼす。実際、Prometheus の取り込み対象を足したときに unit は
	// 変わらず、古い設定のまま動き続けた。
	watch := spec.StackInputs(confDir)
	for _, name := range names {
		if err := system.ApplySystemUnit(ctx, r, name, units[name], watch[name]...); err != nil {
			return err
		}
	}

	// ソケットは Tempo が作る。umask の都合で srwxr-xr-x になるので、
	// グループで書けるように直す。生成を待たないと空振りする。
	if err := system.ChmodWhenExists(spec.HostTraceSocket(), 0o660, 30); err != nil {
		return fmt.Errorf("トレースの口を用意できません: %w", err)
	}
	return nil
}
