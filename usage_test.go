package main

import (
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
)

func usageCfg() (*config.Config, map[string]alloc.Alloc) {
	return &config.Config{Apps: map[string]string{"post": "x/post", "web": "x/web"}},
		map[string]alloc.Alloc{"post": {UID: 6004}, "web": {UID: 6003}}
}

// アプリの unit と DB は置き場所が違う。片方しか見ないと、DB の資源が
// 見えないまま「アプリは軽い」と読み違える。
func TestTargetsCoverBothUserAndSystemUnits(t *testing.T) {
	cfg, allocs := usageCfg()
	var user, sys bool
	for _, x := range targets(cfg, allocs) {
		if strings.Contains(x.Dir, "user-6004.slice") && x.Kind == "app" {
			user = true
		}
		if strings.HasPrefix(x.Dir, "/sys/fs/cgroup/system.slice") && x.Kind == "db" {
			sys = true
		}
	}
	if !user {
		t.Fatal("rootless のアプリを見ていない")
	}
	if !sys {
		t.Fatal("root 側の DB を見ていない")
	}
}

// 宣言に無いものまで測ると、消したアプリがいつまでも出てくる。
func TestTargetsFollowTheDeclaration(t *testing.T) {
	cfg, allocs := usageCfg()
	for _, x := range targets(cfg, allocs) {
		if x.Kind == "app" && x.App != "post" && x.App != "web" {
			t.Fatalf("宣言に無いものを測っている: %s", x.Unit)
		}
	}
}

// 経路と計測基盤も資源を食う。アプリだけ見ても全体像にならない。
func TestTargetsIncludeThePlatform(t *testing.T) {
	cfg, allocs := usageCfg()
	var found bool
	for _, x := range targets(cfg, allocs) {
		if x.Unit == "yunirun-haproxy.service" && x.Kind == "platform" {
			found = true
		}
	}
	if !found {
		t.Fatal("経路を測っていない")
	}
}

// 止まっている unit には cgroup が無い。0 を出すと「動いていて使っていない」
// と区別できない。
func TestStoppedUnitsAreOmittedNotZeroed(t *testing.T) {
	got := renderUsage([]target{{App: "gone", Kind: "app", Unit: "gone.service", Dir: "/sys/fs/cgroup/does-not-exist"}})
	if strings.Contains(got, "gone.service") {
		t.Fatalf("止まっている unit を 0 で出している:\n%s", got)
	}
	// 型の宣言だけは出る (取り込み側が空でも困らないように)
	if !strings.Contains(got, "# TYPE yunirun_unit_cpu_seconds_total counter") {
		t.Fatalf("型の宣言が無い:\n%s", got)
	}
}
