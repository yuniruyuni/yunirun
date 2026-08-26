package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// runRecipient はアプリ側の秘密を暗号化する宛先 (age 公開鍵) を表示する。
//
// アプリの作者はこれに向けて secrets/<ENV_NAME>.age を作る。秘密鍵を持つのは
// ホストだけなので、公開鍵はリポジトリに書いても構わない。
func runRecipient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recipient", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.SecretsKeyPath == "" {
		return fmt.Errorf("secretsKeyPath が設定されていません")
	}
	pub, err := system.Recipient(cfg.SecretsKeyPath)
	if err != nil {
		return fmt.Errorf("公開鍵を導けません: %w", err)
	}
	fmt.Println(pub)
	return nil
}
