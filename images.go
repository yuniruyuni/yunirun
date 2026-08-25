package main

// デプロイで溜まった古い image を片付ける。
//
// pull するだけで消していなかったため、デプロイのたびに増え続けていた。
// 実測で post の領域に 47 個 4.2GB、root 側に 107 個 40GB が溜まり、
// そのうち 37GB (93%) が使われていないものだった。

import (
	"context"
	"fmt"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/system"
)

// KeepImages は残す世代の数。
//
// 直前の版へ戻せるだけの余裕は持たせる。1 つしか残さないと、新しい版が
// 壊れていたときに手元に戻り先が無くなる。
const KeepImages = 3

// pruneImages は指定した image の古い版を消す。
//
// 消さないもの:
//   - 動いているコンテナが使っているもの (podman が拒むので二重の守り)
//   - 新しい方から KeepImages 個
//
// 失敗しても止めない。掃除ができないことと、デプロイが失敗したことは別。
func pruneImages(ctx context.Context, r system.Runner, image string) {
	// 作成が新しい順に並ぶ。同じ image の別タグだけを対象にする。
	out, err := r.Run(ctx, nil, "podman", "images",
		"--filter", "reference="+image, "--sort", "created",
		"--format", "{{.ID}}")
	if err != nil {
		return
	}
	ids := strings.Fields(string(out))
	// --sort created は古い順なので、後ろが新しい。新しい方を残す。
	if len(ids) <= KeepImages {
		return
	}
	for _, id := range ids[:len(ids)-KeepImages] {
		// 使われていれば podman が拒む。それでよい。
		_, _ = r.Run(ctx, nil, "podman", "rmi", id)
	}
}

// pruneDangling はタグの無くなった image を消す。
//
// 同じタグへ pull し直すと、前の実体はタグを失って残る。放っておくと
// 溜まり続けるが、参照が無いので消して困るものではない。
func pruneDangling(ctx context.Context, r system.Runner) {
	_, _ = r.Run(ctx, nil, "podman", "image", "prune", "-f")
}

// PruneAfterDeploy はデプロイの後始末をする。
//
// デプロイの成否とは分ける。掃除に失敗してもデプロイは成功でよい。
func PruneAfterDeploy(ctx context.Context, r system.Runner, image string) {
	pruneImages(ctx, r, image)
	pruneDangling(ctx, r)
}

// describePrune は何を消すかを説明する。テストと表示で使う。
func describePrune(image string) string {
	return fmt.Sprintf("%s の古い版 (新しい %d 世代を残す)", image, KeepImages)
}
