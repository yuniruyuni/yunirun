// Package system は、収束に必要な副作用 (ユーザ・DB・秘密) を扱う。
//
// コマンド実行は Runner 越しにして、組み立てた引数をテストで確認できる
// ようにしてある。秘密を argv に載せていないことの検査もここで行う。
package system

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner は外部コマンドの実行。
type Runner interface {
	// Run は cmd を実行する。stdin が nil でなければ標準入力へ渡す。
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExecRunner は実際にコマンドを起動する Runner。
type ExecRunner struct{}

// Run は exec.CommandContext で起動する。
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// stderr を含めないと原因が分からないが、秘密は stdin 経由でしか
		// 渡していないので、ここに秘密が出ることはない。
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
