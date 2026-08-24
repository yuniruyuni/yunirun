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
	case "rename":
		err = runRename(ctx, os.Args[2:])
	case "remove":
		err = runRemove(ctx, os.Args[2:])
	case "databases":
		err = runDatabases(ctx, os.Args[2:])
	case "recipient":
		err = runRecipient(ctx, os.Args[2:])
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

  yunirun rename [--dry-run] <旧名> <新名>
      アプリの名前を変える。root で実行する。
      割り当て (uid, ポート, subuid) は引き継ぐので、Cloudflare の ingress を
      直す必要がない。ホームは捨てるため、改名の後は各アプリのデプロイが要る。

  yunirun remove [--drop-database] [--dry-run] <名前>
      アプリの実体を片付ける。root で実行する。先に宣言から外しておくこと。
      DB とロールは既定で残す。--drop-database を付けたときだけ落とす。

  yunirun databases
      DB を持つアプリの一覧を JSON で出す。バックアップなど外部の処理が
      DB 名やソケットの場所を知るために使う。名前の規約を yunirun の中に
      留めるための口。

  yunirun recipient
      アプリ側の秘密を暗号化する宛先 (age 公開鍵) を表示する。アプリの作者は
      これに向けて secrets/<ENV_NAME>.age を作る。

設定の所在:
  /etc/yunirun/config.json   どのリポジトリを取り込むか (システム側)
  <repo>/yunirun.jsonc       アプリだけが知っていること (アプリ側)
`)
}
