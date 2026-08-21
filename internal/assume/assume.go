// Package assume は yunirun が前提としている外部環境の性質を、実行可能な形で
// 明示する。
//
// この仕組みが依存しているのは、自分で書いたコードよりも外部システムの挙動の
// 方が多い。実際に踏んだ不具合 14 件のうち 11 件は「外部の仕様を知らなかった」
// ことに起因していた。しかもその多くは man に書かれていない。
//
// 形式手法でモデル化しても、モデルに書く仕様自体が間違っていれば同じ間違いを
// する。そこで「こういう条件が満たされている限り動く」という前提を明示し、
// その前提自体を実物で検査する形にした。
//
// 前提が破れたときに、原因の分からない不具合として現れるのではなく、
// 「この前提が成り立っていない」という形で分かるのが狙い。
package assume

import (
	"context"
	"fmt"
	"sort"
)

// Assumption は 1 つの前提。
type Assumption struct {
	// ID は安定した識別子。エラーメッセージやテストから参照する。
	ID string
	// What は前提の内容。
	What string
	// Why はなぜそれに依存しているか。破れたときに何が起きるか。
	Why string
	// Check は前提が成り立っているか確かめる。nil なら実行時に検査しない
	// (e2e でのみ確かめる) ことを意味する。
	Check func(ctx context.Context, env Env) error
}

// Env は検査に必要な外部への入口。
type Env struct {
	// Run は外部コマンドを実行する。
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// UID は検査対象のアプリユーザ。0 なら未指定。
	UID int
}

// All は yunirun が置いているすべての前提。
//
// 順序は ID 順に正規化されるので、追加位置は自由。
func All() []Assumption {
	a := append([]Assumption{}, assumptions...)
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
	return a
}

// Verify は Check を持つ前提をすべて検査する。
//
// 途中で止めない。破れている前提をまとめて報告する方が、直す側にとって
// 情報が多い。
func Verify(ctx context.Context, env Env) []error {
	var errs []error
	for _, as := range All() {
		if as.Check == nil {
			continue
		}
		if err := as.Check(ctx, env); err != nil {
			errs = append(errs, fmt.Errorf("[%s] %s: %w\n  なぜ必要か: %s",
				as.ID, as.What, err, as.Why))
		}
	}
	return errs
}

// IDs はすべての前提の識別子を返す。テストが「前提が黙って消えていないか」を
// 確かめるのに使う。
func IDs() []string {
	out := make([]string, 0, len(assumptions))
	for _, a := range All() {
		out = append(out, a.ID)
	}
	return out
}
