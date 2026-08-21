package main

import (
	"testing"

	"github.com/yuniruyuni/yunirun/internal/manifest"
)

// converge と migrate が同じ規則で DB 名を導かないと、migration だけが別の DB を
// 見て「テーブルが無い」状態になる。実際に migrate 側がアプリ名から導いたままで、
// 既存 DB を指定しても別の DB に繋ごうとして失敗した。
func TestDBNamesFollowsTheManifest(t *testing.T) {
	m, err := manifest.Parse([]byte(`{"app":{"databaseName":"streamer_post"}}`))
	if err != nil {
		t.Fatal(err)
	}
	n := dbNamesFor("post2", m)
	if n.Database != "streamer_post" || n.Owner != "streamer_post" || n.App != "streamer_post_app" {
		t.Fatalf("マニフェストの指定が反映されていない: %+v", n)
	}
}

func TestDBNamesDefaultsToAppName(t *testing.T) {
	m, _ := manifest.Parse([]byte(`{}`))
	n := dbNamesFor("costume", m)
	if n.Database != "costume" || n.App != "costume_app" {
		t.Fatalf("既定がアプリ名になっていない: %+v", n)
	}
}

func TestDBNamesToleratesMissingManifest(t *testing.T) {
	// マニフェスト未受領でも落ちない。converge は初回にこの状態を通る。
	n := dbNamesFor("x", nil)
	if n.Database != "x" {
		t.Fatalf("%+v", n)
	}
}
