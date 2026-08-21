package manifest

import "testing"

func TestParseAppliesConventionDefaults(t *testing.T) {
	m, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.App.Port != DefaultPort || m.App.Health != DefaultHealth {
		t.Fatalf("既定値が入っていない: %+v", m.App)
	}
}

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	m, err := Load("/nonexistent/yunirun.jsonc")
	if err != nil {
		t.Fatalf("ファイルが無いのはエラーではない: %v", err)
	}
	if m.App.Port != DefaultPort {
		t.Fatalf("既定値が入っていない: %+v", m.App)
	}
}

func TestParseAcceptsCommentsAndTrailingCommas(t *testing.T) {
	m, err := Parse([]byte(`{
  // nginx は PORT を見ないので明示する
  "app": { "port": 80, "health": "/" },
  /* 追加のワークロード */
  "workloads": {
    "cleanup": { "schedule": "02:23", "args": ["--batch=cleanup"] },
  },
}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.App.Port != 80 || m.App.Health != "/" {
		t.Fatalf("app が読めていない: %+v", m.App)
	}
	if m.Workloads["cleanup"].Schedule != "02:23" {
		t.Fatalf("workload が読めていない: %+v", m.Workloads)
	}
}

// コメント除去を素朴に実装すると文字列中の // を壊す。ここが壊れると
// URL が静かに切り詰められて、原因の分かりにくい不具合になる。
func TestParseKeepsDoubleSlashInsideStrings(t *testing.T) {
	m, err := Parse([]byte(`{
  "workloads": {
    "migration": { "image": "https://example.com/a//b", "args": ["x//y"] }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	w := m.Workloads["migration"]
	if w.Image != "https://example.com/a//b" {
		t.Fatalf("文字列中の // が壊れた: %q", w.Image)
	}
	if w.Args[0] != "x//y" {
		t.Fatalf("引数中の // が壊れた: %q", w.Args[0])
	}
}

func TestMigrationDefaultsToOwnerRoleAndOthersToApp(t *testing.T) {
	m, err := Parse([]byte(`{"workloads":{"migration":{},"cleanup":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Workloads["migration"].Role; got != RoleOwner {
		t.Fatalf("migration の role = %q, owner であるべき", got)
	}
	// cleanup が owner を既定で持つと、日次で動くものに DDL 権限が付く。
	if got := m.Workloads["cleanup"].Role; got != RoleApp {
		t.Fatalf("cleanup の role = %q, app であるべき", got)
	}
}

func TestParseRejectsUnsafeWorkloadName(t *testing.T) {
	for _, bad := range []string{"../x", "a b", "A", "-x", ""} {
		if _, err := Parse([]byte(`{"workloads":{"` + bad + `":{}}}`)); err == nil {
			t.Fatalf("workload 名 %q を受け入れてしまった", bad)
		}
	}
}

func TestParseRejectsOutOfRangePort(t *testing.T) {
	if _, err := Parse([]byte(`{"app":{"port":70000}}`)); err == nil {
		t.Fatal("範囲外のポートを受け入れてしまった")
	}
}

// DB を使わないアプリに DB とロールを作ると、消し忘れた資源が溜まるうえ
// 不要な資格情報が生成される。既定は「使わない」。
func TestDatabaseIsOptOutByDefault(t *testing.T) {
	m, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.App.Database {
		t.Fatal("宣言していないのに DB を使うことになっている")
	}
}

func TestDatabaseCanBeDeclared(t *testing.T) {
	m, err := Parse([]byte(`{"app":{"database":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !m.App.Database {
		t.Fatal("宣言が反映されていない")
	}
}
