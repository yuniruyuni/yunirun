package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"sort"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runSubIDs は台帳にあるアプリのユーザの subuid/subgid を /etc に戻す。
//
// NixOS の activation から、users の直後に呼ばれる。activation は
// /etc/subuid を宣言から再生成し、宣言の外で作った yunirun のユーザの行を
// 消す。以前はこれを converge が後から戻していたが、ホストの再起動では
// その間にユーザの unit が起動し、podman が行の無い状態でユーザ名前空間を
// 作ってしまった (戻した後もその名前空間が使われ続け、非 root の image が
// 起動できなくなった)。起動時の activation は systemd より前に走るので、
// ここで戻せば欠けた状態を誰も見ない。
//
// activation の中で動くので、ネットワークも podman も systemd も使わない。
// 読むのは設定と台帳だけで、台帳に番号を新しく振ることもしない (振るのは
// converge の仕事)。
func runSubIDs(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("subids", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("subids は root で実行してください")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ledger, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return err
	}
	for _, name := range subIDTargets(ledger, userExists) {
		u := alloc.User(name)
		changed, err := system.EnsureSubIDs(u, ledger.Entries[name])
		if err != nil {
			return fmt.Errorf("%s の subuid を戻せません: %w", u, err)
		}
		if changed {
			fmt.Printf("yunirun: %s の subuid/subgid を戻しました\n", u)
		}
	}
	return nil
}

// subIDTargets は subuid を戻す対象のアプリを名前順で返す。
//
// 台帳にあり、ユーザが実在するもの。宣言から外して止めただけのアプリも
// 含める。ユーザが残っている限り podman の状態も残っているので、行だけ
// 消えると戻したときに同じ問題を踏む。片付けた (remove した) アプリは
// 台帳からもユーザからも消えているので対象にならない。
func subIDTargets(l *alloc.Ledger, exists func(string) bool) []string {
	var out []string
	for name := range l.Entries {
		if exists(alloc.User(name)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func userExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}
