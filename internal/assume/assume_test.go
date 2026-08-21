package assume

import (
	"strings"
	"testing"
)

// 前提は実際に踏んだ不具合から書き起こしたもの。黙って消えると、その不具合が
// 「なぜそう書いてあるのか」の記録ごと失われる。
func TestEveryAssumptionExplainsWhyItMatters(t *testing.T) {
	for _, a := range All() {
		if a.ID == "" || a.What == "" {
			t.Fatalf("識別子か内容が空: %+v", a)
		}
		// Why が無いと、後から見た人が消してよいか判断できない。
		if len(a.Why) < 30 {
			t.Fatalf("[%s] なぜ必要かの説明が短すぎる: %q", a.ID, a.Why)
		}
	}
}

func TestAssumptionIDsAreUniqueAndNamespaced(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range IDs() {
		if seen[id] {
			t.Fatalf("識別子が重複: %s", id)
		}
		seen[id] = true
		// どの外部システムの話かが分かる形にする。
		if !strings.Contains(id, "/") {
			t.Fatalf("識別子に系統が無い: %s", id)
		}
	}
}

// 前提のうち相当数が自動検査できないと、明示した意味が薄れる。
func TestMostAssumptionsAreMachineCheckable(t *testing.T) {
	total, checkable := 0, 0
	for _, a := range All() {
		total++
		if a.Check != nil {
			checkable++
		}
	}
	if checkable*2 < total {
		t.Fatalf("自動検査できる前提が半分未満: %d/%d", checkable, total)
	}
}
