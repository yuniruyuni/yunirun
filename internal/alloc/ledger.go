package alloc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Ledger は一度決めた割り当てを記録する台帳。
//
// 名前順のインデックスから導出する方式は使えない。アルファベット順で前に入る
// 名前を追加すると既存アプリの番号が全てずれ、稼働中のコンテナは旧ポートのまま
// HAProxy が新ポートを見る (= 停止) うえ、ファイルの所有者 uid も実ユーザと
// ずれてデータが読めなくなる。稼働中のアプリの uid を後から変えることは
// 実質不可能なので、収束で追従することもできない。
//
// 一度決めた番号は動かさない。アプリを消しても他の番号は詰めない。
type Ledger struct {
	// Entries はアプリ名から割り当てへの対応。
	Entries map[string]Alloc `json:"entries"`
}

// LoadLedger は台帳を読む。無ければ空の台帳を返す。
func LoadLedger(path string) (*Ledger, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Ledger{Entries: map[string]Alloc{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%s を読めません: %w", path, err)
	}
	if l.Entries == nil {
		l.Entries = map[string]Alloc{}
	}
	return &l, nil
}

// Save は台帳を書き出す。
//
// 一時ファイルへ書いてから rename する。途中で落ちたときに台帳が壊れると、
// 次の収束で既存アプリに別の番号を振ってしまう。
func (l *Ledger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Ensure は names に対する割り当てを返す。
//
// 既に記録があるものはその番号を使い、新規には未使用の最小番号を与える。
// 台帳に無い既存アプリが増えたときだけ内容が変わる。
func (l *Ledger) Ensure(names []string, b Base) map[string]Alloc {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	out := make(map[string]Alloc, len(sorted))
	for _, n := range sorted {
		if a, ok := l.Entries[n]; ok {
			out[n] = a
			continue
		}
		a, err := l.allocate(b)
		if err != nil {
			// 上限を超えたら配らない。配ると短命ポート帯と衝突して
			// 静かに bind に失敗する。
			return out
		}
		l.Entries[n] = a
		out[n] = a
	}
	return out
}

// EnsureStrict は Ensure と同じだが、上限を超えたらエラーを返す。
func (l *Ledger) EnsureStrict(names []string, b Base) (map[string]Alloc, error) {
	out := l.Ensure(names, b)
	for _, n := range names {
		if _, ok := out[n]; !ok {
			return nil, fmt.Errorf("割り当ての上限 (%d) に達しました: %s", MaxApps, n)
		}
	}
	return out, nil
}

// allocate は未使用の最小 index を割り当てる。
//
// 消えたアプリの番号は再利用する。番号が無限に増えるのを避けるためだが、
// 再利用の前に古いユーザとデータが消えている必要がある。converge は宣言から
// 消えたアプリのユーザを削除しないので、実際には手で消すまで再利用されない。
func (l *Ledger) allocate(b Base) (Alloc, error) {
	used := map[int]bool{}
	for _, a := range l.Entries {
		used[a.Index] = true
	}
	i := 0
	for used[i] {
		i++
	}
	if i >= MaxApps {
		return Alloc{}, fmt.Errorf("割り当ての上限 (%d) に達しました", MaxApps)
	}
	return at(i, b), nil
}

// Remove は台帳から消す。番号を再利用できるようにする。
func (l *Ledger) Remove(name string) { delete(l.Entries, name) }

// Rename は台帳上のアプリ名を変える。割り当てはそのまま引き継ぐ。
//
// 名前を変えただけで新しい番号が振られると、稼働中のコンテナが旧ポートに
// 取り残されて停止する。移行のために付けた接尾辞を外すときなど、実体は
// 同じまま名前だけ変わる場面がある。
//
// 移行先の名前が既に使われている場合は何もしない。上書きすると別のアプリの
// 割り当てを奪うことになる。
func (l *Ledger) Rename(from, to string) bool {
	a, ok := l.Entries[from]
	if !ok {
		return false
	}
	if _, taken := l.Entries[to]; taken {
		return false
	}
	l.Entries[to] = a
	delete(l.Entries, from)
	return true
}
