package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runRemove はアプリの実体を片付ける。
//
// converge は宣言から消えたアプリを止めるところまでしかしない。止めるのは
// 元に戻せるが、ユーザや DB を消すのは戻せないため、明示的な操作にしてある。
//
// DB とロールは既定で残す。アプリを畳んでもデータは残したい場面の方が多く、
// 消す判断は中身を見てからでないとできない。--drop-database を付けたときだけ
// 落とす。
func runRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	dropDB := fs.Bool("drop-database", false, "DB とロールも落とす")
	dryRun := fs.Bool("dry-run", false, "何をするかだけ表示する")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("使い方: yunirun remove [--drop-database] [--dry-run] <名前>")
	}
	name := fs.Arg(0)

	if os.Geteuid() != 0 {
		return fmt.Errorf("remove は root で実行してください")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// 宣言に残っているものは消さない。消しても次の converge が作り直すので
	// 意味が無いうえ、その間だけ落ちる。先に宣言から外させる。
	if _, ok := cfg.Apps[name]; ok {
		return fmt.Errorf("%s はまだ宣言されています。先に services.yunirun.apps から外してください", name)
	}
	l, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return err
	}
	a, ok := l.Entries[name]
	if !ok {
		return fmt.Errorf("台帳に %s がありません", name)
	}

	user := alloc.User(name)
	home := filepath.Join(cfg.HomeDir(), name)
	vault := filepath.Join(cfg.StateDir, "secrets", name)

	if *dryRun {
		fmt.Printf("%s (uid=%d frontend=%d) を片付けます\n", name, a.UID, a.Frontend)
		fmt.Printf("  user@%d.service を止め、linger を外す\n", a.UID)
		fmt.Printf("  %s を消す\n", home)
		fmt.Printf("  ユーザ %s を消す\n", user)
		fmt.Printf("  /etc/subuid, /etc/subgid から %s の行を消す\n", user)
		if _, err := os.Stat(vault); err == nil {
			fmt.Printf("  %s を消す\n", vault)
		}
		fmt.Printf("  台帳から %s を消す (uid と ポートが再利用可能になる)\n", name)
		if *dropDB {
			fmt.Printf("  DB のコンテナを止めて %s を消す\n",
				filepath.Join(cfg.DatabaseDir(), name))
		} else {
			fmt.Printf("  DB のデータ (%s) は残す\n",
				filepath.Join(cfg.DatabaseDir(), name))
		}
		return nil
	}

	r := system.ExecRunner{}

	system.StopUser(ctx, r, user, a.UID)

	if err := os.RemoveAll(home); err != nil {
		return fmt.Errorf("%s を消せません: %w", home, err)
	}
	_ = os.RemoveAll(filepath.Join(runtimeDir, name))

	// userdel は先に home を消してあるので -r を付けない。付けると既に無い
	// ディレクトリについて警告を出す。
	if _, err := r.Run(ctx, nil, "userdel", user); err != nil {
		return fmt.Errorf("ユーザ %s を消せません: %w", user, err)
	}
	for _, p := range []string{"/etc/subuid", "/etc/subgid"} {
		if err := dropSubIDLines(p, user); err != nil {
			return err
		}
	}

	if _, err := os.Stat(vault); err == nil {
		if err := os.RemoveAll(vault); err != nil {
			return fmt.Errorf("%s を消せません: %w", vault, err)
		}
	}

	if *dropDB {
		// DB はコンテナごと消す。データディレクトリを捨てれば済むので、
		// SQL で DROP する必要がない。
		if err := removeDatabaseContainer(ctx, r, cfg, name); err != nil {
			return err
		}
	}

	l.Remove(name)
	if err := l.Save(cfg.LedgerPath()); err != nil {
		return err
	}

	fmt.Printf("%s を片付けました\n", name)
	if !*dropDB {
		fmt.Printf("\nDB のデータは %s に残しています。\n",
			filepath.Join(cfg.DatabaseDir(), name))
		fmt.Printf("中身を確認してから消すなら --drop-database を付けて実行し直してください。\n")
	}
	return nil
}

// removeDatabaseContainer は DB のコンテナと unit とデータを片付ける。
//
// SQL で DROP DATABASE する必要はない。インスタンスごと消えるため。
func removeDatabaseContainer(ctx context.Context, r system.Runner, cfg *config.Config, name string) error {
	svc := name + "-db.service"
	_, _ = r.Run(ctx, nil, "systemctl", "stop", svc)
	if err := os.Remove(filepath.Join(system.SystemUnitDir, name+"-db.container")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := r.Run(ctx, nil, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	dir := filepath.Join(cfg.DatabaseDir(), name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("%s を消せません: %w", dir, err)
	}
	return nil
}
