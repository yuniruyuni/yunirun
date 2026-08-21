package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/render"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// Request は CI から標準入力で受け取る内容。
//
// トークンを argv ではなく stdin で渡すのは ps に出さないため。マニフェストを
// 同じ経路で運ぶことで、VPS が GitHub を取りに行かずに済む。private
// リポジトリでも認証情報を置かなくてよい。
type Request struct {
	Token    string `json:"token"`
	Manifest string `json:"manifest"`
}

var tagRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func runDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	appFlag := fs.String("app", "", "対象アプリ (既定は実行ユーザから判定)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("使い方: yunirun deploy <tag>")
	}
	tag := fs.Arg(0)
	if !tagRE.MatchString(tag) {
		return fmt.Errorf("タグに使えない文字が含まれています: %q", tag)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	app := *appFlag
	if app == "" {
		if app, err = appFromCurrentUser(cfg); err != nil {
			return err
		}
	}
	repo, ok := cfg.Apps[app]
	if !ok {
		return fmt.Errorf("%s は取り込まれていません", app)
	}

	// 割り当ては converge が置いた情報から読む。台帳そのものは root 専用で、
	// 他アプリの割り当ても含むので deploy には見せない。
	info, err := loadAppInfo(app)
	if err != nil {
		return err
	}

	var req Request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
		return fmt.Errorf("標準入力を読めません: %w", err)
	}

	m, err := manifest.Parse([]byte(req.Manifest))
	if err != nil {
		return err
	}
	// 受け取ったマニフェストを状態として残す。converge はこれを使う。
	if err := saveManifest(cfg, app, req.Manifest); err != nil {
		return err
	}

	r := system.ExecRunner{}
	owner := strings.SplitN(repo, "/", 2)[0]
	// image 名はアプリ側が宣言できる。既定はアプリ名。
	image := imageRef(owner, app, m.App.Image)

	authfile := filepath.Join(runtimeDir, app, "ghcr-auth.json")
	defer os.Remove(authfile)
	fmt.Println("==> ghcr.io にログイン")
	if _, err := r.Run(ctx, []byte(req.Token), "podman", "login", "ghcr.io",
		"--username", owner, "--password-stdin", "--authfile", authfile); err != nil {
		return err
	}

	fmt.Printf("==> %s:%s を取得\n", image, tag)
	if _, err := r.Run(ctx, nil, "podman", "pull", "--authfile", authfile, image+":"+tag); err != nil {
		return err
	}
	if _, err := r.Run(ctx, nil, "podman", "tag", image+":"+tag, "localhost/"+app+":current"); err != nil {
		return err
	}

	if _, has := m.Workloads["migration"]; has {
		// migration は owner パスワード (DDL) を使うので、この非特権ユーザでは
		// 実行しない。root 側の unit に依頼する。deploy ユーザは起動できるが
		// owner パスワードを読めない。
		fmt.Println("==> schema を適用 (root 側で実行)")
		if err := saveTag(cfg, app, tag); err != nil {
			return err
		}
		if _, err := r.Run(ctx, nil, "sudo", "--non-interactive",
			"systemctl", "start", "yunirun-migrate@"+app+".service"); err != nil {
			return fmt.Errorf("migration に失敗しました: %w", err)
		}
	}

	// 片方ずつ入れ替える。落としている間はもう片方が受けるので停止しない。
	for _, color := range render.Colors {
		unit := fmt.Sprintf("%s-%s.service", app, color)
		fmt.Printf("==> %s を再起動\n", color)
		if _, err := r.Run(ctx, nil, "systemctl", "--user", "restart", unit); err != nil {
			return err
		}
		port := info.Blue
		if color == "green" {
			port = info.Green
		}
		if err := waitHealthy(ctx, port, m.App.Health); err != nil {
			// 片側が上がらない時点で止める。もう片方はまだ旧版のまま動いている。
			return fmt.Errorf("%s が healthy になりません: %w", color, err)
		}
	}
	fmt.Printf("==> 完了: %s:%s\n", image, tag)
	return nil
}

func waitHealthy(ctx context.Context, port int, path string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	r := system.ExecRunner{}
	for i := 0; i < 60; i++ {
		if _, err := r.Run(ctx, nil, "curl", "-fsS", "--max-time", "3", url); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("%s が応答しません", url)
}

// imageRef は image の完全な参照を組み立てる。
//
// 宣言が空ならアプリ名、"/" を含まなければ owner を補い、含むならそのまま使う。
// loadAppInfo は converge が置いた情報を読む。
func loadAppInfo(app string) (*AppInfo, error) {
	b, err := os.ReadFile(appInfoPath(app))
	if err != nil {
		return nil, fmt.Errorf("%s はまだ収束していません: %w", app, err)
	}
	var info AppInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func imageRef(owner, app, declared string) string {
	name := declared
	if name == "" {
		name = app
	}
	if strings.Contains(name, "/") {
		return name
	}
	return "ghcr.io/" + owner + "/" + name
}

func appFromCurrentUser(cfg *config.Config) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	const prefix = "yunirun-"
	if !strings.HasPrefix(u.Username, prefix) {
		return "", fmt.Errorf("実行ユーザ %s からアプリを判定できません", u.Username)
	}
	name := strings.TrimPrefix(u.Username, prefix)
	if _, ok := cfg.Apps[name]; !ok {
		return "", fmt.Errorf("%s は取り込まれていません", name)
	}
	return name, nil
}

// saveManifest と saveTag は受け渡し用ディレクトリへ書く。
//
// このディレクトリは converge がアプリのユーザ所有で作る。stateDir は台帳や
// 秘密が入っているので root 専用のままにしておく。
func saveManifest(cfg *config.Config, app, content string) error {
	return os.WriteFile(manifestPath(cfg, app), []byte(content), 0o644)
}

func saveTag(_ *config.Config, app, tag string) error {
	return os.WriteFile(tagPath(app), []byte(tag), 0o644)
}
