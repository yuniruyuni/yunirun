package system

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// EnsureUser はアプリを動かす Linux ユーザを収束させる。
//
// NixOS の users.users を使わず imperative に作るのは、yunirun を NixOS に
// 依存させないため。mutableUsers の既定が true なので rebuild でも消えない。
func EnsureUser(ctx context.Context, r Runner, name string, a alloc.Alloc, home string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	// グループを先に作る。uid と同じ番号にして、他ユーザとグループを共有しない。
	// 共有すると、そのグループに付けた読み取り権限が意図しない相手へ渡る。
	if _, err := r.Run(ctx, nil, "groupadd", "--system", "--gid", strconv.Itoa(a.GID), name); err != nil {
		if _, e := user.LookupGroup(name); e != nil {
			return fmt.Errorf("グループ %s を作れません: %w", name, err)
		}
	}
	if _, err := r.Run(ctx, nil, "useradd",
		"--system",
		"--uid", strconv.Itoa(a.UID),
		"--gid", strconv.Itoa(a.GID),
		"--home-dir", home,
		"--create-home",
		// ssh からコマンドを実行させるためシェルが要る。
		"--shell", "/bin/sh",
		name,
	); err != nil {
		return fmt.Errorf("ユーザ %s を作れません: %w", name, err)
	}
	return nil
}

// EnsureSubIDs は rootless podman が使う subuid/subgid を収束させる。
//
// NixOS の activation がこのファイルを宣言から再生成するため、yunirun が
// 足した行は rebuild で消える。converge を activation の後に走らせて
// 復元する前提にしてある。
func EnsureSubIDs(name string, a alloc.Alloc) error {
	line := fmt.Sprintf("%s:%d:%d", name, a.SubUID, alloc.SubUIDSize)
	for _, p := range []string{"/etc/subuid", "/etc/subgid"} {
		if err := appendLineIfMissing(p, line); err != nil {
			return err
		}
	}
	return nil
}

func appendLineIfMissing(path, line string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	// 同じユーザの古い行が残っていると podman が困るので、先頭一致で除く。
	prefix := strings.SplitN(line, ":", 2)[0] + ":"
	var kept []string
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, prefix) {
			continue
		}
		kept = append(kept, l)
	}
	kept = append(kept, line)
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// EnsureLinger はログインセッションが無くてもユーザのサービスが動くようにする。
func EnsureLinger(ctx context.Context, r Runner, name string) error {
	_, err := r.Run(ctx, nil, "loginctl", "enable-linger", name)
	return err
}
