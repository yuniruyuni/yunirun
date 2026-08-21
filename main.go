// Command yunirun は yuniruyuni.net の VPS 上でアプリを動かすデプロイシステム。
//
// converge はシステム側の宣言 (どのリポジトリを取り込むか) に実体を一致させる。
// deploy は各アプリの CI から呼ばれ、image の取得と入れ替えを行う。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "converge":
		err = runConverge(ctx, os.Args[2:])
	case "deploy":
		err = runDeploy(ctx, os.Args[2:])
	case "migrate":
		err = runMigrate(ctx, os.Args[2:])
	case "doctor":
		err = runDoctor(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	case "--version", "version":
		fmt.Println("yunirun", version)
		return
	default:
		fmt.Fprintf(os.Stderr, "不明なコマンド: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yunirun:", err)
		os.Exit(1)
	}
}

// version はビルド時に ldflags で埋める。
var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `yunirun - VPS 上でコンテナ化したアプリを動かす

  yunirun converge [--config PATH]
      宣言されたアプリ一覧に実体を一致させる。root で実行する。
      ユーザ・DB・秘密・HAProxy 設定・コンテナ定義を収束させる。

  yunirun deploy <tag> [--app NAME]
      image を取得して blue/green を入れ替える。アプリのユーザで実行する。
      GHCR のトークンを標準入力から受け取る。

  yunirun migrate <app>
      schema を適用する。root で実行する。owner ロール (DDL) を使うため、
      deploy ユーザには実行させず systemd 経由で起動される。

  yunirun doctor [--app NAME] [--list]
      yunirun が置いている前提が今の環境で成り立っているか確かめる。
      この仕組みは外部システムの挙動に多く依存しており、前提が破れたときに
      原因の分からない不具合ではなく前提の名前で分かるようにする。

設定の所在:
  /etc/yunirun/config.json   どのリポジトリを取り込むか (システム側)
  <repo>/yunirun.jsonc       アプリだけが知っていること (アプリ側)
`)
}
