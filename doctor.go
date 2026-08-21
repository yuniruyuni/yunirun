package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/assume"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runDoctor は yunirun が置いている前提が今の環境で成り立っているかを確かめる。
//
// この仕組みが依存しているのは、自分で書いたコードよりも外部システムの挙動の
// 方が多い。前提が破れたときに原因の分からない不具合として現れるのではなく、
// 「この前提が成り立っていない」という形で分かるようにする。
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
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
