package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/assume"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runDoctor は yunirun が置いている前提が今の環境で成り立っているかを確かめる。
//
// この仕組みが依存しているのは、自分で書いたコードよりも外部システムの挙動の
// 方が多い。前提が破れたときに原因の分からない不具合として現れるのではなく、
// 「この前提が成り立っていない」という形で分かるようにする。
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	appFlag := fs.String("app", "", "アプリのユーザとして検査する")
	listOnly := fs.Bool("list", false, "前提を一覧するだけで検査しない")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *listOnly {
		for _, a := range assume.All() {
			mark := " "
			if a.Check != nil {
				mark = "*"
			}
			fmt.Printf("%s %-46s %s\n", mark, a.ID, a.What)
		}
		fmt.Println("\n* が付いたものは doctor で自動検査される。")
		return nil
	}

	env := assume.Env{
		Run: func(ctx context.Context, name string, a ...string) ([]byte, error) {
			return system.ExecRunner{}.Run(ctx, nil, name, a...)
		},
	}
	if *appFlag != "" {
		u, err := user.Lookup("yunirun-" + *appFlag)
		if err != nil {
			return fmt.Errorf("%s のユーザが見つかりません: %w", *appFlag, err)
		}
		uid, _ := strconv.Atoi(u.Uid)
		env.UID = uid
	}

	// アプリごとの検査 (subuid の行と、podman が実際に使う名前空間) は
	// 台帳の割り当てと照らし合わせる。台帳は root しか読めない。
	if os.Geteuid() == 0 {
		apps, err := doctorApps(*cfgPath, *appFlag)
		if err != nil {
			return err
		}
		env.Apps = apps
		env.Namespace = func(ctx context.Context, uid int) (system.UserNamespace, error) {
			return system.ProbeUserNamespace(ctx, system.ExecRunner{}, uid)
		}
	}

	errs := assume.Verify(ctx, env)
	if len(errs) == 0 {
		checked := 0
		for _, a := range assume.All() {
			if a.Check != nil {
				checked++
			}
		}
		fmt.Printf("前提 %d 件を検査し、すべて成り立っています。\n", checked)
		fmt.Printf("(自動検査できないものが %d 件あります。yunirun doctor --list で一覧)\n",
			len(assume.All())-checked)
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d 件の前提が成り立っていません:\n", len(errs))
	for _, e := range errs {
		fmt.Fprintf(&b, "\n%v\n", e)
	}
	fmt.Fprint(os.Stderr, b.String())
	return fmt.Errorf("前提が破れています")
}

// doctorApps は検査対象のアプリを台帳から集める。
//
// 宣言にあり、台帳に割り当てがあり、ユーザが実在するもの。まだ一度も
// 収束していないアプリは検査しても必ず落ちるので含めない。
func doctorApps(cfgPath, only string) ([]assume.App, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	ledger, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return nil, err
	}
	var out []assume.App
	for _, name := range cfg.Names() {
		if only != "" && name != only {
			continue
		}
		a, ok := ledger.Entries[name]
		if !ok || !userExists(alloc.User(name)) {
			continue
		}
		out = append(out, assume.App{User: alloc.User(name), Alloc: a})
	}
	return out, nil
}
