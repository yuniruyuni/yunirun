// Package config は、システム側 (NixOS) が置く /etc/yunirun/config.json を読む。
//
// ここに書くのは「どのリポジトリを取り込むか」という取り込みの意思決定だけ。
// これがそのまま opkssh の認可リストと対応し、セキュリティ境界になる。
// アプリの中身に関する設定は各リポジトリの yunirun.jsonc 側にある。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// DefaultPath は NixOS モジュールが書き出す場所。
const DefaultPath = "/etc/yunirun/config.json"

// Config は yunirun 全体の設定。
type Config struct {
	// Domain は各アプリのホスト名を導出する元。<app>.<domain> になる。
	Domain string `json:"domain"`
	// StateDir は yunirun が持つ状態の置き場所。
	StateDir string `json:"stateDir"`
	// AdminRecipient は生成した秘密を暗号化する管理者の age 公開鍵。
	// ホスト鍵を失ったときの復旧経路として必ず含める。
	AdminRecipient string `json:"adminRecipient"`
	// HostKeyPath はホスト側の age 秘密鍵。
	HostKeyPath string `json:"hostKeyPath"`
	// Apps はアプリ名からリポジトリ (owner/name) への対応。
	Apps map[string]string `json:"apps"`
}

var (
	appRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Load は path から設定を読む。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s を読めません: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Domain == "" {
		return fmt.Errorf("domain が空です")
	}
	if c.StateDir == "" {
		return fmt.Errorf("stateDir が空です")
	}
	for name, repo := range c.Apps {
		// アプリ名は Linux ユーザ名・DB 名・unit 名の元になる。ここを緩めると
		// それらすべてに任意の文字列が流れ込む。
		if !appRE.MatchString(name) {
			return fmt.Errorf("アプリ名に使えない文字が含まれています: %q", name)
		}
		if !repoRE.MatchString(repo) {
			return fmt.Errorf("%s のリポジトリ指定が不正です: %q", name, repo)
		}
	}
	return nil
}

// Names は宣言されたアプリ名を名前順で返す。
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Apps))
	for n := range c.Apps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Allocs はホスト側資源の割り当てを返す。
func (c *Config) Allocs() map[string]alloc.Alloc {
	return alloc.For(c.Names(), alloc.DefaultBase())
}

// Hostname はアプリの公開ホスト名を返す。
func (c *Config) Hostname(app string) string { return app + "." + c.Domain }
