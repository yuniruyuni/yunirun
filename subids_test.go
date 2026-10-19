package main

import (
	"slices"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// activation から戻す対象は、台帳にあってユーザが実在するもの。
// 宣言から外して止めただけのアプリも含める (ユーザと podman の状態が残るため)。
func TestSubIDTargetsAreLedgerEntriesWithExistingUsers(t *testing.T) {
	l := &alloc.Ledger{Entries: map[string]alloc.Alloc{
		"web":     {SubUID: 4196608},
		"fighter": {SubUID: 4393216},
		"gone":    {SubUID: 4000000},
	}}
	got := subIDTargets(l, func(u string) bool { return u != "yunirun-gone" })
	if !slices.Equal(got, []string{"fighter", "web"}) {
		t.Fatalf("%v", got)
	}
}
