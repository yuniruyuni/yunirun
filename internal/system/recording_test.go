package system

import (
	"context"
	"fmt"
	"strings"
)

// recordingRunner は実行されたコマンドを記録するだけで、常に失敗を返す。
// 順序の検証に使う。
type recordingRunner struct {
	commands []string
}

func (r *recordingRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	return nil, fmt.Errorf("記録用なので常に失敗する")
}

// RunEnv は env を無視して Run に流す。argv に何が載ったかだけを見たいので、
// 環境変数の中身はここでは記録しない。
func (r *recordingRunner) RunEnv(ctx context.Context, stdin []byte, _ []string, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, stdin, name, args...)
}
