package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runRename はアプリの名前を変える。
//
// 名前はアプリの至る所に現れる。Linux ユーザ、ホーム、unit 名、HAProxy の
// backend、opkssh の認可先。converge は宣言に無いものを片付けないので、
// 設定の名前を書き換えるだけでは旧名の資源がそのまま残り、しかも旧ユーザが
// uid とポートを握ったままなので新しいユーザを作れない。
//
// 割り当て (uid, ポート, subuid) は引き継ぐ。新しく振り直すと、稼働中の
// コンテナが旧ポートに取り残され、Cloudflare の ingress も指す先を失う。
//
// ホームは移さずに捨てる。podman は graphroot の絶対パスを自身の DB に
// 記録しているため、ホームごと移すと以降 podman が
// "database graph root does not match" で動かなくなる。どのみち unit 名が
// 変わる以上コンテナは作り直しになるので、image を取り直す方が確実。
// つまり改名の後は各アプリのデプロイが要る。
//
// DB のパスワードを入れた金庫だけは移す。捨てても converge が作り直し
// ALTER ROLE で DB 側を揃えるので壊れはしないが、無用な入れ替えを避ける。
func runRename(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rename", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	dryRun := fs.Bool("dry-run", false, "何をするかだけ表示する")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("使い方: yunirun rename [--dry-run] <旧名> <新名>")
	}
	from, to := fs.Arg(0), fs.Arg(1)

	if os.Geteuid() != 0 {
		return fmt.Errorf("rename は root で実行してください")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	l, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return err
	}
	a, ok := l.Entries[from]
	if !ok {
		return fmt.Errorf("台帳に %s がありません", from)
	}
	if _, taken := l.Entries[to]; taken {
		return fmt.Errorf("%s は既に使われています", to)
	}

	fromUser, toUser := alloc.User(from), alloc.User(to)
	fromHome := filepath.Join(cfg.HomeDir(), from)
	fromVault := filepath.Join(cfg.StateDir, "secrets", from)
	toVault := filepath.Join(cfg.StateDir, "secrets", to)

	if *dryRun {
		fmt.Printf("%s -> %s (uid=%d frontend=%d を引き継ぐ)\n", from, to, a.UID, a.Frontend)
		fmt.Printf("  user@%d.service を止める\n", a.UID)
		fmt.Printf("  %s を消す (podman の image は取り直しになる)\n", fromHome)
		fmt.Printf("  %s を %s へ改名する\n", fromUser, toUser)
		fmt.Printf("  /etc/subuid, /etc/subgid から %s の行を消す\n", fromUser)
		if _, err := os.Stat(fromVault); err == nil {
			fmt.Printf("  %s を %s へ移す\n", fromVault, toVault)
		}
		fmt.Printf("  台帳の %s を %s にする\n", from, to)
		return nil
	}

	r := system.ExecRunner{}

	// 1. ユーザの systemd インスタンスを止める。配下のコンテナごと止まる。
	//    linger が残っていると usermod がユーザを使用中と見て拒否する。
	system.StopUser(ctx, r, fromUser, a.UID)

	// 2. ホームを捨てる。converge が新しい名前で作り直す。
	if err := os.RemoveAll(fromHome); err != nil {
		return fmt.Errorf("%s を消せません: %w", fromHome, err)
	}
	// 実行時に公開している側も消す。converge が書き直す。
	_ = os.RemoveAll(filepath.Join("/run/yunirun", from))

	// 3. ユーザとグループを改名する。uid と gid はそのまま残る。
	if _, err := r.Run(ctx, nil, "usermod", "-l", toUser, "-d",
		filepath.Join(cfg.HomeDir(), to), fromUser); err != nil {
		return fmt.Errorf("%s を改名できません: %w", fromUser, err)
	}
	if _, err := r.Run(ctx, nil, "groupmod", "-n", toUser, fromUser); err != nil {
		return fmt.Errorf("グループ %s を改名できません: %w", fromUser, err)
	}

	// 4. subuid/subgid の旧名の行を落とす。converge が新しい名前で入れ直す。
	//    残すと存在しないユーザの行が同じ範囲を主張し続ける。
	for _, p := range []string{"/etc/subuid", "/etc/subgid"} {
		if err := dropSubIDLines(p, fromUser); err != nil {
			return err
		}
	}

	// 5. 金庫を移す。DB を使わないアプリには無い。
	if _, err := os.Stat(fromVault); err == nil {
		if err := os.Rename(fromVault, toVault); err != nil {
			return fmt.Errorf("%s を移せません: %w", fromVault, err)
		}
	}

	// 6. 台帳。ここまで来て初めて名前が確定する。
	if !l.Rename(from, to) {
		return fmt.Errorf("台帳を更新できません")
	}
	if err := l.Save(cfg.LedgerPath()); err != nil {
		return err
	}

	fmt.Printf("%s を %s に改名しました (uid=%d frontend=%d)\n", from, to, a.UID, a.Frontend)
	fmt.Printf("\n次に必要な作業:\n")
	fmt.Printf("  1. 設定の apps を %s にして converge を動かす\n", to)
	fmt.Printf("  2. %s のリポジトリからデプロイする (image を取り直すため)\n", to)
	fmt.Printf("     workflow の ssh 先も %s@ に直す\n", toUser)
	return nil
}

// dropSubIDLines は /etc/subuid 形式のファイルから、指定した名前の行を消す。
func dropSubIDLines(path, user string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var kept []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, user+":") {
			continue
		}
		kept = append(kept, line)
	}
	closeErr := f.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	// 書き換えは一時ファイル経由にする。途中で落ちても元が残る。
	tmp := path + ".yunirun.tmp"
	body := strings.Join(kept, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
