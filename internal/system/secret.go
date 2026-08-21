package system

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// passwordAlphabet は生成するパスワードに使う文字。
//
// 引用符・バックスラッシュ・空白を含めない。SQL や shell へ渡す際の
// エスケープを考えなくて済むようにするためで、長さで強度を確保する。
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// PasswordLength は生成するパスワードの長さ。
const PasswordLength = 48

// NewPassword は暗号論的乱数からパスワードを作る。
func NewPassword() (string, error) {
	b := make([]byte, PasswordLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = passwordAlphabet[int(b[i])%len(passwordAlphabet)]
	}
	return string(b), nil
}

// Vault は age で暗号化した秘密の置き場所。
//
// yunirun が生成する秘密 (DB パスワード) はマシンの外に出る必要が無いので
// 人が管理しない。ホスト鍵に加えて管理者鍵にも暗号化しておき、ホストを
// 失っても復旧できるようにする。
type Vault struct {
	Dir            string
	HostKeyPath    string
	HostRecipient  string
	AdminRecipient string
	Runner         Runner
}

// Path は名前に対応する暗号文の位置を返す。
func (v Vault) Path(name string) string {
	return filepath.Join(v.Dir, name+".age")
}

// Put は値を暗号化して保存する。既にあれば何もしない。
//
// 冪等にしてあるのは converge から繰り返し呼ばれるため。作り直すと
// DB 側のパスワードと食い違う。
func (v Vault) Put(ctx context.Context, name, value string) error {
	p := v.Path(name)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(v.Dir, 0o700); err != nil {
		return err
	}
	args := []string{"-r", v.HostRecipient}
	if v.AdminRecipient != "" {
		args = append(args, "-r", v.AdminRecipient)
	}
	// 値は stdin で渡す。argv に載せると ps から見える。
	out, err := v.Runner.Run(ctx, []byte(value), "age", args...)
	if err != nil {
		return fmt.Errorf("%s の暗号化に失敗しました: %w", name, err)
	}
	// 生成物は root のみ。読ませたい相手には materialize 側で権限を付ける。
	return os.WriteFile(p, out, 0o400)
}

// Get は復号した値を返す。
func (v Vault) Get(ctx context.Context, name string) (string, error) {
	b, err := os.ReadFile(v.Path(name))
	if err != nil {
		return "", err
	}
	out, err := v.Runner.Run(ctx, b, "age", "-d", "-i", v.HostKeyPath)
	if err != nil {
		return "", fmt.Errorf("%s の復号に失敗しました: %w", name, err)
	}
	return string(out), nil
}
