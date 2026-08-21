package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
)

// runRename は台帳上のアプリ名を変える。
//
// 割り当て (uid, ポート, subuid) をそのまま引き継ぐ。名前を変えただけで新しい
// 番号が振られると、稼働中のコンテナが旧ポートに取り残されて停止する。
//
// 変えるのは台帳だけ。ユーザ・DB・unit は次の converge が新しい名前で作り直す。
// 旧名の資源は残るので、確認してから手で消す。自動で消すと、改名を間違えた
// ときに戻せない。
func runRename(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rename", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("使い方: yunirun rename <旧名> <新名>")
	}
	from, to := fs.Arg(0), fs.Arg(1)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	l, err := alloc.LoadLedger(cfg.LedgerPath())
	if err != nil {
		return err
	}
	if !l.Rename(from, to) {
		return fmt.Errorf("%s を %s へ改名できません (旧名が無いか、新名が使用中)", from, to)
	}
	if err := l.Save(cfg.LedgerPath()); err != nil {
		return err
	}

	a := l.Entries[to]
	fmt.Printf("台帳を更新しました: %s -> %s (uid=%d frontend=%d)\n", from, to, a.UID, a.Frontend)
	fmt.Printf("\n次に必要な作業:\n")
	fmt.Printf("  1. config.json の apps を新しい名前にする\n")
	fmt.Printf("  2. yunirun converge を実行する (ユーザと unit が作り直される)\n")
	fmt.Printf("  3. 旧名の資源を確認して消す:\n")
	fmt.Printf("       ユーザ  yunirun-%s\n", from)
	fmt.Printf("       ホーム  %s/%s\n", cfg.HomeDir(), from)
	fmt.Printf("       秘密    %s/secrets/%s\n", cfg.StateDir, from)
	fmt.Printf("     DB は yunirun.jsonc の databaseName を指していれば影響しない。\n")
	return nil
}
