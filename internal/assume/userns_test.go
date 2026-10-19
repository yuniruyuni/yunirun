package assume

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/system"
)

func check(t *testing.T, id string) func(context.Context, Env) error {
	t.Helper()
	for _, a := range All() {
		if a.ID == id {
			if a.Check == nil {
				t.Fatalf("[%s] に検査が無い", id)
			}
			return a.Check
		}
	}
	t.Fatalf("[%s] が無い", id)
	return nil
}

func fighterApp() App {
	return App{User: "yunirun-fighter", Alloc: alloc.Alloc{UID: 6006, GID: 6006, SubUID: 4393216}}
}

func nsOf(t *testing.T, s string) system.UserNamespace {
	t.Helper()
	m, err := system.ParseIDMap(s)
	if err != nil {
		t.Fatal(err)
	}
	return system.UserNamespace{UIDMap: m, GIDMap: m}
}

// 本番で踏んだ形。/etc/subuid には行があり、それを見る検査はすべて通って
// いたが、podman は行が欠けていた間に作った名前空間を使い続けていた。
func TestDoctorCatchesStaleNamespaceEvenWhenSubuidLineIsPresent(t *testing.T) {
	dir := t.TempDir()
	files := []string{filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")}
	for _, p := range files {
		if err := os.WriteFile(p, []byte("yunirun-fighter:4393216:65536\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{
		Apps:       []App{fighterApp()},
		SubIDFiles: files,
		Namespace: func(context.Context, int) (system.UserNamespace, error) {
			return nsOf(t, "0 6006 1\n"), nil
		},
	}
	if err := check(t, "nixos/subuid-is-regenerated-on-activation")(context.Background(), env); err != nil {
		t.Fatalf("行はあるのに落ちた: %v", err)
	}
	err := check(t, "podman/user-namespace-is-fixed-when-created")(context.Background(), env)
	if err == nil {
		t.Fatal("古い名前空間を見逃した")
	}
	if !strings.Contains(err.Error(), "yunirun-fighter") {
		t.Fatalf("どのアプリか分からない: %v", err)
	}

	env.Namespace = func(context.Context, int) (system.UserNamespace, error) {
		return nsOf(t, "0 6006 1\n1 4393216 65536\n"), nil
	}
	if err := check(t, "podman/user-namespace-is-fixed-when-created")(context.Background(), env); err != nil {
		t.Fatalf("正常な名前空間で落ちた: %v", err)
	}
}

func TestDoctorCatchesMissingSubuidLine(t *testing.T) {
	dir := t.TempDir()
	files := []string{filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")}
	// activation が再生成した直後の形。subgid にだけ残っている。
	if err := os.WriteFile(files[0], []byte("alice:100000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[1], []byte("yunirun-fighter:4393216:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := Env{Apps: []App{fighterApp()}, SubIDFiles: files}
	err := check(t, "nixos/subuid-is-regenerated-on-activation")(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), files[0]) {
		t.Fatalf("subuid の欠けを見逃した: %v", err)
	}
}
