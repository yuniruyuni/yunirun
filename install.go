package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/render"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runInstall は NixOS 以外のホストへ yunirun を据え付ける。
//
// NixOS では同じことをモジュールが宣言としてやる。ここが受け持つのは、
// そういう仕組みを持たないホストで、モジュールが置くのと同じものを置くこと。
//
//   - unit (converge / migrate@ / usage)
//   - ディレクトリを作る tmpfiles の規則
//   - deploy ユーザに許す sudo の規則
//
// アプリそのものを作るのは converge の仕事で、ここではやらない。
func runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	from := fs.String("from", "", "この設定ファイルを所定の位置へ複写してから据え付ける")
	dryRun := fs.Bool("dry-run", false, "置くものを表示するだけで書き込まない")
	// 書き出し先をずらせるようにしてある。イメージを組む際の staging と、
	// 置く側の経路をテストから確かめるのに使う。
	rootFlag := fs.String("root", "", "この位置を頂点として置く (systemd への反映は行わない)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// "/" は staging ではない。ここを素通しにすると NixOS の拒否を
	// --root / で迂回できてしまう。
	root := strings.TrimSuffix(*rootFlag, "/")
	staging := root != ""

	if err := refuseOnNixOS(*dryRun || staging); err != nil {
		return err
	}
	if *from != "" && !*dryRun {
		if err := copyConfig(*from, filepath.Join(root, *cfgPath)); err != nil {
			return err
		}
	}
	cfg, err := config.Load(filepath.Join(root, *cfgPath))
	if err != nil {
		return fmt.Errorf("%s を読めません (--from で置くこともできます): %w", *cfgPath, err)
	}

	in, err := installInput(cfg, *dryRun || staging)
	if err != nil {
		return err
	}
	files := render.InstallUnits(in)

	if *dryRun {
		for _, f := range files {
			fmt.Printf("=== %s (%04o)\n%s\n", f.Path, f.Mode, f.Content)
		}
		return nil
	}
	if os.Geteuid() != 0 && !staging {
		return fmt.Errorf("root で実行してください")
	}
	for _, f := range files {
		if err := placeFile(root, f); err != nil {
			return err
		}
	}
	if staging {
		fmt.Printf("%s の下に置きました。systemd への反映は行っていません。\n", root)
		return nil
	}

	r := system.ExecRunner{}
	// ディレクトリを今すぐ作る。規則を置いただけでは次の起動まで効かない。
	if _, err := r.Run(ctx, nil, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/yunirun.conf"); err != nil {
		return fmt.Errorf("ディレクトリを用意できません: %w", err)
	}
	if _, err := r.Run(ctx, nil, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("unit を読み直せません: %w", err)
	}
	units := []string{"yunirun-converge.service"}
	if in.Usage {
		units = append(units, "yunirun-usage.service")
	}
	if _, err := r.Run(ctx, nil, "systemctl", append([]string{"enable", "--now"}, units...)...); err != nil {
		return fmt.Errorf("有効にできません: %w", err)
	}

	fmt.Printf("据え付けました。ExecStart は %s を指しています。\n", in.Exe)
	fmt.Println("移動したら install をやり直してください。")
	if len(in.Apps) > 0 {
		fmt.Println("deploy を許す認可 (opkssh など) は別に用意する必要があります。")
	}
	return nil
}

// installInput は実行時に探るものを集める。
func installInput(cfg *config.Config, staging bool) (render.InstallInput, error) {
	exe, err := selfPath()
	// 消えうる場所に居ても、中身を見せるだけ・staging へ置くだけなら困らない。
	if err != nil && !staging {
		return render.InstallInput{}, err
	}
	// sudo は文字列で照合するので、呼び出し側が PATH で解決するのと同じ
	// ものを書く必要がある。
	sctl, err := exec.LookPath("systemctl")
	if err != nil {
		return render.InstallInput{}, fmt.Errorf("systemctl が見つかりません: %w", err)
	}
	return render.InstallInput{
		Exe:       exe,
		Systemctl: sctl,
		Apps:      cfg.Names(),
		Usage:     cfg.Observability.Enable,
		Dirs:      installDirs(cfg),
	}, nil
}

// installDirs は用意するディレクトリとモードを返す。
func installDirs(cfg *config.Config) map[string]string {
	d := map[string]string{
		// 台帳と秘密が入るので root 専用。
		cfg.StateDir: "0700",
		// 中の各項目が自分で権限を持つので、ここは辿れればよい。
		cfg.HomesDir: "0755",
		cfg.DBDir:    "0755",
		cfg.EnvDir:   "0755",
		// tmpfs。再起動で消えるのでここに規則が要る。
		"/run/yunirun": "0755",
		// root 側の Quadlet の置き場。
		system.SystemUnitDir: "0755",
	}
	if cfg.Observability.Enable {
		d[cfg.Observability.Dir] = "0755"
	}
	return d
}

// ephemeralPrefixes は再起動や後片付けで消えうる場所。
var ephemeralPrefixes = []string{"/tmp/", "/run/", "/var/tmp/", "/dev/"}

// selfPath は yunirun 自身の絶対パスを返す。
//
// unit はここを指す。消えうる場所に居るまま据え付けると、再起動後に
// ExecStart が見つからず起動しない unit ができる。
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	for _, bad := range ephemeralPrefixes {
		if strings.HasPrefix(exe, bad) {
			return exe, fmt.Errorf("%s は消えうる場所です。/usr/local/bin などへ置いてから実行してください", exe)
		}
	}
	return exe, nil
}

// refuseOnNixOS は NixOS では断る。
//
// NixOS ではモジュールが同じものを宣言として持っている。両方が書くと、
// どちらが本当かが分からなくなる。
func refuseOnNixOS(dryRun bool) error {
	if dryRun {
		return nil
	}
	if _, err := os.Stat("/etc/NIXOS"); err == nil {
		return fmt.Errorf("NixOS では services.yunirun を使ってください (install は宣言と競合します)")
	}
	return nil
}

// placeFile は内容が違うときだけ置く。sudoers は置く前に検査する。
func placeFile(root string, f render.InstallFile) error {
	path := filepath.Join(root, f.Path)
	if old, err := os.ReadFile(path); err == nil && string(old) == f.Content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 同じディレクトリに書いてから移す。書き換えの途中の状態を見せない。
	tmp := path + ".yunirun-new"
	if err := os.WriteFile(tmp, []byte(f.Content), os.FileMode(f.Mode)); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if strings.Contains(path, "/sudoers") {
		// 壊れた sudoers を置くと sudo 全体が使えなくなり、直す手段まで
		// 失う。置く前に検査する。
		if err := checkSudoers(tmp); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, os.FileMode(f.Mode)); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lookVisudo は visudo を探す。
//
// PATH だけでは足りない。visudo は sbin に置かれることが多く、root 以外の
// PATH には入っていないことがある。テストが黙って飛ぶのを避けたい。
func lookVisudo() (string, error) {
	if p, err := exec.LookPath("visudo"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/sbin/visudo", "/sbin/visudo", "/usr/local/sbin/visudo"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("visudo が見つからないので sudoers を検査できません")
}

// checkSudoers は visudo に構文を確かめさせる。
func checkSudoers(path string) error {
	visudo, err := lookVisudo()
	if err != nil {
		// 検査できないなら置かない。壊れたものを置く方が高くつく。
		return err
	}
	out, err := exec.Command(visudo, "-c", "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudoers の内容が不正です: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// copyConfig は設定ファイルを所定の位置へ複写する。
//
// 設定を書く仕組み (NixOS のモジュール) が無いホストのための入口。
func copyConfig(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	// 読めるかどうかをここで見る。壊れた設定を置いてから converge が
	// 落ちるより、置く前に断る方が分かりやすい。
	if _, err := config.Parse(b); err != nil {
		return fmt.Errorf("%s を設定として読めません: %w", from, err)
	}
	// 内容が同じなら書かない。他の置きものと揃える。
	if old, err := os.ReadFile(to); err == nil && string(old) == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}
