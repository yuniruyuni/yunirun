package main

import (
	"strings"
	"testing"
)

func plan(hasMigrate bool) []string {
	return PlanSteps(PlanInput{
		App: "x", Tag: "abc", Image: "ghcr.io/o/x",
		Colors: []string{"blue", "green"}, HasMigrate: hasMigrate,
	})
}

func indexOf(t *testing.T, steps []string, substr string) int {
	t.Helper()
	for i, s := range steps {
		if strings.Contains(s, substr) {
			return i
		}
	}
	t.Fatalf("手順に %q が無い: %v", substr, steps)
	return -1
}

// 宣言の反映が後ろにあると、古い unit のまま起動してしまう。実際 nginx を
// 80 番で動かすアプリの初回デプロイが、既定の 3000 番を publish する unit の
// ままで healthy にならなかった。
func TestManifestIsAppliedBeforeAnythingStarts(t *testing.T) {
	s := plan(true)
	apply := indexOf(t, s, StepApplyManifest)
	if apply != 0 {
		t.Fatalf("宣言の反映が先頭でない (位置 %d): %v", apply, s)
	}
	if apply > indexOf(t, s, StepRestart) {
		t.Fatal("再起動より後に宣言を反映している")
	}
}

// migration が後ろだと、新しいコードが古いスキーマで動く瞬間ができる。
func TestMigrationRunsBeforeAnyColorIsReplaced(t *testing.T) {
	s := plan(true)
	if indexOf(t, s, StepMigrate) > indexOf(t, s, StepRestart) {
		t.Fatalf("migration が入れ替えより後: %v", s)
	}
}

// image を取る前に migration を走らせると、まだ無い image を使おうとする。
func TestPullHappensBeforeMigration(t *testing.T) {
	s := plan(true)
	if indexOf(t, s, StepPull) > indexOf(t, s, StepMigrate) {
		t.Fatalf("pull が migration より後: %v", s)
	}
}

// 片側が healthy になる前にもう片側を落とすと、両方落ちて停止する。
func TestEachColorIsConfirmedHealthyBeforeTheNextIsTouched(t *testing.T) {
	s := plan(false)
	blueWait, greenRestart := -1, -1
	for i, step := range s {
		if strings.HasPrefix(step, "blue") && strings.Contains(step, StepWaitHealthy) {
			blueWait = i
		}
		if strings.HasPrefix(step, "green") && strings.Contains(step, StepRestart) {
			greenRestart = i
		}
	}
	if blueWait < 0 || greenRestart < 0 {
		t.Fatalf("手順が足りない: %v", s)
	}
	if blueWait > greenRestart {
		t.Fatalf("blue の確認前に green を触っている: %v", s)
	}
}

// migration を持たないアプリで migration の手順が出ると、無い image を
// 実行しようとして失敗する。
func TestMigrationIsSkippedWhenNotDeclared(t *testing.T) {
	for _, s := range plan(false) {
		if strings.Contains(s, StepMigrate) {
			t.Fatalf("migration を持たないのに手順に出ている: %v", s)
		}
	}
}
