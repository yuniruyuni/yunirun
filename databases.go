package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/manifest"
)

// Database はバックアップなどの外部処理が必要とする 1 アプリ分の情報。
type Database struct {
	App       string `json:"app"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	SocketDir string `json:"socketDir"`
	// OwnerPasswordFile は owner のパスワードが入った env ファイル。
	// root しか読めない。
	OwnerPasswordFile string `json:"ownerPasswordFile"`
}

// runDatabases は DB を持つアプリの一覧を出す。
//
// 名前の規約 (DB 名とロール名の導出、ソケットの置き場所) を yunirun の中に
// 留めるために要る。バックアップの側で規約を推測すると、どちらかを変えた
// ときに静かにずれる。ずれても「取れた分だけ成功」に見えるのが厄介で、
// 気付くのは復元しようとした時になる。
func runDatabases(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("databases", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	out := []Database{}
	for _, name := range cfg.Names() {
		// 宣言が失われていると DB を使わないアプリに見え、黙って
		// バックアップの対象から外れる。存在を必須にする。
		if _, err := os.Stat(storedManifestPath(cfg, name)); err != nil {
			return fmt.Errorf("%s の宣言が見つかりません: %w", name, err)
		}
		m, err := manifest.Load(storedManifestPath(cfg, name))
		if err != nil {
			return err
		}
		if !m.App.Database {
			continue
		}
		n := dbNamesFor(name, m)
		out = append(out, Database{
			App:               name,
			Name:              n.Database,
			Owner:             n.Owner,
			SocketDir:         filepath.Join(cfg.DatabaseDir(), name, "sock"),
			OwnerPasswordFile: filepath.Join(runtimeDir, name, "migration.env"),
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
