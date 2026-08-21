package system

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteUnitsCreatesEveryIntermediateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"a.container": "x"}); err != nil {
		t.Fatal(err)
	}
	// 中間のどれか 1 つでも別の所有者で残ると、systemd-tmpfiles も podman も
	// unsafe path transition として拒否する。
	for _, r := range []string{".config", ".config/containers", ".config/containers/systemd"} {
		if _, err := os.Stat(filepath.Join(home, r)); err != nil {
			t.Fatalf("%s が作られていない: %v", r, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(UnitDir(home), "a.container"))
	if err != nil || string(b) != "x" {
		t.Fatalf("unit が書けていない: %v %q", err, b)
	}
}

func TestWriteUnitsRemovesUnitsNoLongerDeclared(t *testing.T) {
	home := t.TempDir()
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"old.container": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), map[string]string{"new.container": "y"}); err != nil {
		t.Fatal(err)
	}
	// 残すと、構成を変えたときに古いコンテナが動き続ける。
	if _, err := os.Stat(filepath.Join(UnitDir(home), "old.container")); !os.IsNotExist(err) {
		t.Fatal("宣言から消えた unit が残っている")
	}
}

func TestWriteUnitsLeavesUnchangedFilesAlone(t *testing.T) {
	home := t.TempDir()
	units := map[string]string{"a.container": "x"}
	if err := WriteUnits(home, os.Getuid(), os.Getgid(), units); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(UnitDir(home), "a.container")
	st1, _ := os.Stat(p)

	if err := WriteUnits(home, os.Getuid(), os.Getgid(), units); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	// 書き換えると systemd が変更を検知して無用な再起動を招く。
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("内容が同じなのに書き換えている")
	}
}
