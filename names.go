package main

import (
	"github.com/yuniruyuni/yunirun/internal/manifest"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// dbNamesFor はアプリの DB 名一式を返す。
//
// converge と migrate の両方から呼ぶ。片方だけがマニフェストの指定を見落とすと、
// migration だけが別の DB を見て「テーブルが無い」状態になる。実際に migrate 側が
// アプリ名から導いたままだったため、既存 DB を指定しても post2 という別の DB に
// 繋ごうとして失敗した。導出を 1 箇所に集める。
func dbNamesFor(app string, m *manifest.Manifest) system.DBNames {
	name := app
	if m != nil && m.App.DatabaseName != "" {
		name = m.App.DatabaseName
	}
	return system.NamesFor(name)
}
