package main

import (
	"context"
	"testing"
)

// recordingRunner は実行されたコマンドを記録する。何が発行されたかだけを
// 見たいので、結果は常に空で返す。
type recordingRunner struct {
	calls [][]string
}

func (r *recordingRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil, nil
}

// RunEnv は env を無視して Run に流す。argv に何が載ったかだけを見たいので、
// 環境変数の中身はここでは記録しない。
func (r *recordingRunner) RunEnv(ctx context.Context, stdin []byte, _ []string, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, stdin, name, args...)
}

func TestImageRefDefaultsToAppName(t *testing.T) {
	if got := imageRef("yuniruyuni", "costume", ""); got != "ghcr.io/yuniruyuni/costume" {
		t.Fatalf("got %q", got)
	}
}

// yuniruyuni.net 側で付ける名前 (DB やユーザの名前になる) と、リポジトリが
// push する image 名は本来別のもの。食い違う場合はアプリ側が宣言する。
func TestImageRefHonorsDeclaredName(t *testing.T) {
	if got := imageRef("yuniruyuni", "template2", "template"); got != "ghcr.io/yuniruyuni/template" {
		t.Fatalf("got %q", got)
	}
}

func TestImageRefKeepsFullyQualifiedReference(t *testing.T) {
	if got := imageRef("yuniruyuni", "x", "docker.io/library/nginx"); got != "docker.io/library/nginx" {
		t.Fatalf("got %q", got)
	}
}

// converge は Type=oneshot かつ RemainAfterExit=yes で、一度成功すると
// active (exited) のまま留まる。既に active な unit への systemctl start は
// 何もせず成功するため、start を使うとデプロイは「宣言を反映」と表示しながら
// 実際には収束せず、マニフェストの変更が一切効かなくなる。
//
// 実際にこれで DB 名の変更が反映されず migration が落ちた。restart であることを
// 固定する。
func TestApplyManifestRestartsConvergeInsteadOfStartingIt(t *testing.T) {
	r := &recordingRunner{}
	info := &AppInfo{}
	err := runStep(context.Background(), r, StepApplyManifest, stepCtx{
		app: "template", info: &info,
	})
	// loadAppInfo は本物のファイルを読むので失敗しうる。見たいのは発行された
	// コマンドの方なので、そこは問わない。
	_ = err

	var found bool
	for _, c := range r.calls {
		if len(c) >= 4 && c[0] == "sudo" && c[2] == "systemctl" {
			if c[3] == "start" {
				t.Fatalf("converge を start で起動している。既に active なら何も起きない: %v", c)
			}
			if c[3] == "restart" && c[4] == "yunirun-converge.service" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("converge を restart する呼び出しが無い: %v", r.calls)
	}
}
