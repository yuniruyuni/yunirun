package system

import "testing"

// 実際に踏んだ形。テーブルはあるが、アプリのロールがどれにも権限を持たない。
// schema 側の GRANT 先が別のロール名になっているときにこうなる。
func TestParseGrantCountsReadsTotalAndGranted(t *testing.T) {
	total, granted, err := parseGrantCounts("1|0\n")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || granted != 0 {
		t.Fatalf("total=%d granted=%d", total, granted)
	}
}

func TestParseGrantCountsIgnoresSurroundingNoise(t *testing.T) {
	total, granted, err := parseGrantCounts("\n 3 | 3 \n\n")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || granted != 3 {
		t.Fatalf("total=%d granted=%d", total, granted)
	}
}

// 読み取れない出力を 0 件として扱うと、権限が無い状態を見逃す。
func TestParseGrantCountsFailsOnUnreadableOutput(t *testing.T) {
	if _, _, err := parseGrantCounts("ERROR: something\n"); err == nil {
		t.Fatal("読み取れない出力を通している")
	}
}
