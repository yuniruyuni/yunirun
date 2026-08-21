package main

import "testing"

func TestImageRefDefaultsToAppName(t *testing.T) {
	if got := imageRef("yuniruyuni", "costume", ""); got != "ghcr.io/yuniruyuni/costume" {
		t.Fatalf("got %q", got)
	}
}

// yuniruyuni.net 側で付ける名前 (DB やユーザの名前になる) と、リポジトリが
// push する image 名は本来別のもの。食い違う場合はアプリ側が宣言する。
func TestImageRefHonorsDeclaredName(t *testing.T) {
	if got := imageRef("yuniruyuni", "template2", "template"); got != "ghcr.io/yuniruyuni/template" {
		t.Fatalf("got %q", got)
	}
}

func TestImageRefKeepsFullyQualifiedReference(t *testing.T) {
	if got := imageRef("yuniruyuni", "x", "docker.io/library/nginx"); got != "docker.io/library/nginx" {
		t.Fatalf("got %q", got)
	}
}
