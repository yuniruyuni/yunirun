// Package alloc は、アプリ名からホスト側の資源を決定的に割り当てる。
//
// ここが独立した package になっているのは、割り当てを 1 箇所に閉じ込めるため。
// 以前この計算を NixOS の設定に手書きしていた時期に、uid を既存ユーザと重複
// させてしまい deploy-web が別アプリ (template) の DB パスワードを読める状態を
// 作った。ホスト側の番号はアプリが知る必要も宣言する必要もない純粋な内部事情
// なので、名前から一意に導出して人が触らないようにする。
package alloc

import "sort"

// Alloc はアプリ 1 つに割り当てられたホスト側の資源。
type Alloc struct {
	// Index は Apps を名前順に並べたときの位置。他の値はすべてここから導く。
	Index int

	// UID と GID は同じ値にする。専用ユーザなので既存グループと共有しない。
	UID int
	GID int

	// Frontend は HAProxy が listen するポート。cloudflared の向き先。
	Frontend int
	// Blue と Green はコンテナを publish するポート。
	Blue  int
	Green int

	// SubUID は rootless podman が使う subuid/subgid の開始値。
	// NixOS の自動割り当て (100000 付近から hash で散らす) と重ならない帯を取る。
	SubUID int
}

// SubUIDSize は 1 ユーザに割り当てる subuid の幅。
// rootless podman の慣習に合わせて 65536 にする。
const SubUIDSize = 65536

// Base は割り当ての起点。
//
// UID を 6000 番台に取るのは、NixOS が system user へ動的に割り当てる範囲
// (400-999) を避けるため。nixpkgs の ids.nix は「uid/gid を指定せず NixOS に
// 任せる」ことを勧めているが、コンテナの unit 配置などで数値が必要になるため
// ここでは固定する。であれば動的割り当てと衝突しない帯を選ぶ必要がある。
type Base struct {
	UID  int
	Port int
	// SubUID は subuid 帯の起点。NixOS が使う 100000 付近を避けて高い位置に取る。
	SubUID int
	// PortStride は 1 アプリが使うポート幅。frontend/blue/green の 3 つに
	// 余裕を持たせて 10 にしてある。
	PortStride int
}

// DefaultBase は運用で使う既定値。
func DefaultBase() Base {
	return Base{UID: 6000, Port: 8100, PortStride: 10, SubUID: 4000000}
}

// MaxApps は割り当てられるアプリ数の上限。
//
// これを超えるとポートが短命ポート帯 (32768 以降) に入り、たまたま使われている
// 番号と衝突して bind に失敗する。静かに壊れるので、境界で明示的に止める。
//
// 8100 から 10 ずつなので (32768 - 8100) / 10 = 2466 が限界。運用上は 2 桁に
// 収まるはずなので、余裕を持って 500 で切る。増やすなら基準値ごと見直す。
const MaxApps = 500

// at は index に対応する割り当てを返す。
//
// 割り当ての実体は Ledger 側にある。ここは index から番号を作る計算だけを持つ。
func at(i int, b Base) Alloc {
	front := b.Port + i*b.PortStride
	return Alloc{
		Index:    i,
		UID:      b.UID + i,
		GID:      b.UID + i,
		Frontend: front,
		Blue:     front + 1,
		Green:    front + 2,
		SubUID:   b.SubUID + i*SubUIDSize,
	}
}

// For は index を名前順に振った割り当てを返す。台帳を持たない場面での参照用。
//
// 運用では使わない。アプリを追加すると既存の番号がずれるため。Ledger.Ensure を
// 使うこと。
func For(names []string, b Base) map[string]Alloc {
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	out := make(map[string]Alloc, len(sorted))
	for i, n := range sorted {
		out[n] = at(i, b)
	}
	return out
}

// User はアプリを動かす Linux ユーザ名を返す。
func User(app string) string { return "yunirun-" + app }
