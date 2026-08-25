package main

import "fmt"

// Step はデプロイの 1 手順。
//
// 手順を値として組み立てるのは、順序の契約をテストで固定するため。実際に
// これまで踏んだ不具合の多くは「何をするか」ではなく「どの順でするか」だった:
//
//   - マニフェストを反映する前に image を起動して、古い unit のまま動いた
//   - migration より先に blue/green を入れ替えて、新旧が食い違った
//   - 片側が healthy になる前にもう片側を落として、無停止でなくなった
//
// これらは実装を並べ替えるだけで静かに再発する。テストで並びを押さえておく。
type Step struct {
	Name string
	Run  func() error
}

// PlanInput は手順を組み立てるのに必要な情報。
type PlanInput struct {
	App        string
	Tag        string
	Image      string
	Colors     []string
	HasMigrate bool
}

// 手順の名前。テストから参照するので定数にしてある。
const (
	StepApplyManifest = "宣言を反映"
	StepLogin         = "ghcr.io にログイン"
	StepPull          = "image を取得"
	StepMigrate       = "schema を適用"
	StepRestart       = "再起動"
	StepWaitHealthy   = "healthy を待つ"
	StepPrune         = "古い image を片付ける"
)

// PlanSteps はデプロイ手順の並びを返す。
//
// 実行そのものは呼び出し側が行う。ここは並びだけを決める。
func PlanSteps(in PlanInput) []string {
	steps := []string{
		// 何よりも先に宣言を反映する。unit を書くのは converge の仕事なので、
		// これが後だと古い unit のまま起動してしまう。
		StepApplyManifest,
		StepLogin,
		StepPull,
	}
	if in.HasMigrate {
		// blue/green より前に一度だけ。失敗したらここで止まり、稼働中の
		// コンテナには触れない。
		steps = append(steps, StepMigrate)
	}
	for _, c := range in.Colors {
		// 片方ずつ入れ替える。落としている間はもう片方が受けるので停止しない。
		steps = append(steps,
			fmt.Sprintf("%s を%s", c, StepRestart),
			fmt.Sprintf("%s の%s", c, StepWaitHealthy))
	}
	// 最後に片付ける。新しい版が healthy になってからでないと、戻り先を
	// 消してしまう。
	steps = append(steps, StepPrune)
	return steps
}
