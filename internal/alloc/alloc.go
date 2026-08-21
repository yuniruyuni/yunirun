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
}

// Base は割り当ての起点。
//
// UID を 6000 番台に取るのは、NixOS が system user へ動的に割り当てる範囲
// (400-999) を避けるため。nixpkgs の ids.nix は「uid/gid を指定せず NixOS に
// 任せる」ことを勧めているが、コンテナの unit 配置などで数値が必要になるため
// ここでは固定する。であれば動的割り当てと衝突しない帯を選ぶ必要がある。
type Base struct {
	UID  int
	Port int
	// PortStride は 1 アプリが使うポート幅。frontend/blue/green の 3 つに
	// 余裕を持たせて 10 にしてある。
	PortStride int
}

// DefaultBase は運用で使う既定値。
func DefaultBase() Base {
	return Base{UID: 6000, Port: 8100, PortStride: 10}
}

// For は名前順に並べた apps に対する割り当てを返す。
//
// 名前順に固定するので、アプリを足しても既存アプリの番号は動かない…わけでは
// ない点に注意。アルファベット順で間に入る名前を足すと後ろがずれる。ずれても
// converge が追従できるよう、番号に依存する状態 (unit ファイル等) は毎回
// 生成し直す前提にしてある。
func For(names []string, b Base) map[string]Alloc {
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	out := make(map[string]Alloc, len(sorted))
	for i, n := range sorted {
		front := b.Port + i*b.PortStride
		out[n] = Alloc{
			Index:    i,
			UID:      b.UID + i,
			GID:      b.UID + i,
			Frontend: front,
			Blue:     front + 1,
			Green:    front + 2,
		}
	}
	return out
}

// User はアプリを動かす Linux ユーザ名を返す。
func User(app string) string { return "yunirun-" + app }
