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
	if u, err := user.Lookup(name); err == nil {
		// ホームの場所が変わったら追従する。既存ユーザを作り直すと uid が
		// 変わってファイルの所有者がずれるので、変更だけを当てる。
		if u.HomeDir != home {
			if _, err := r.Run(ctx, nil, "usermod", "--home", home, "--move-home", name); err != nil {
				return fmt.Errorf("%s のホームを %s へ移せません: %w", name, home, err)
			}
		}
		return nil
	}
	// グループを先に作る。uid と同じ番号にして、他ユーザとグループを共有しない。
	// 共有すると、そのグループに付けた読み取り権限が意図しない相手へ渡る。
	if err := ensureGroup(ctx, r, name, a.GID); err != nil {
		return err
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

// ensureGroup はグループを収束させる。
//
// 「同名で既にある」と「番号が別のグループに取られている」を区別する。前者は
// 収束済みなので続行してよいが、後者で続行するとユーザが他人のグループに属し、
// そのグループに付けた読み取り権限が意図しない相手へ渡る。
func ensureGroup(ctx context.Context, r Runner, name string, gid int) error {
	if g, err := user.LookupGroup(name); err == nil {
		if g.Gid != strconv.Itoa(gid) {
			return fmt.Errorf("グループ %s は gid=%s で既にあります (期待は %d)", name, g.Gid, gid)
		}
		return nil
	}
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
		return fmt.Errorf("gid=%d は %s に使われています", gid, g.Name)
	}
	if _, err := r.Run(ctx, nil, "groupadd", "--system", "--gid", strconv.Itoa(gid), name); err != nil {
		return fmt.Errorf("グループ %s を作れません: %w", name, err)
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
