package system

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// EnsureUser はアプリを動かす Linux ユーザを収束させる。
//
// NixOS の users.users を使わず imperative に作るのは、yunirun を NixOS に
// 依存させないため。mutableUsers の既定が true なので rebuild でも消えない。
func EnsureUser(ctx context.Context, r Runner, name string, a alloc.Alloc, home string) error {
	if u, err := user.Lookup(name); err == nil {
		// ホームの場所が食い違っていても自動では直さない。
		//
		// usermod は linger でユーザのプロセスが動いていると
		// "user is currently used by process" で失敗する。通すにはアプリを
		// 止める必要があり、収束のために稼働中のサービスを落とすのは本末転倒。
		//
		// 新規ユーザは最初から正しい場所に作られるので、これが起きるのは
		// yunirun 自身の設定を変えた直後だけ。手で直す前提にして、気づけるよう
		// エラーにする。
		if u.HomeDir != home {
			return fmt.Errorf(
				"%s のホームが %s ですが %s であるべきです。"+
					"linger を止めてから usermod で変更してください",
				name, u.HomeDir, home)
		}
		return ensureHome(home, a)
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

// ensureHome はホームを作り、所有者を合わせる。
func ensureHome(home string, a alloc.Alloc) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := os.Chown(home, a.UID, a.GID); err != nil {
		return fmt.Errorf("%s の所有者を移せません: %w", home, err)
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

// EnsureSharedGroup は共有用のグループを作る。gid は指定しない。
//
// アプリの割り当てとは別の資源なので、台帳の番号体系には載せない。既にあれば
// 何もしない。
func EnsureSharedGroup(ctx context.Context, r Runner, name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	if _, err := r.Run(ctx, nil, "groupadd", "--system", name); err != nil {
		return fmt.Errorf("グループ %s を作れません: %w", name, err)
	}
	return nil
}

// GroupGID は名前から gid を引く。
func GroupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// AddToGroup はユーザを補助グループへ入れる。
//
// 既に入っていれば何もしない。無条件に usermod を呼ぶと、linger でプロセスが
// 動いているユーザに対して失敗することがある。
func AddToGroup(ctx context.Context, r Runner, userName, group string) error {
	u, err := user.Lookup(userName)
	if err != nil {
		return err
	}
	gids, err := u.GroupIds()
	if err != nil {
		return err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	for _, id := range gids {
		if id == g.Gid {
			return nil
		}
	}
	if _, err := r.Run(ctx, nil, "usermod", "-aG", group, userName); err != nil {
		return fmt.Errorf("%s を %s に入れられません: %w", userName, group, err)
	}
	return nil
}

// Chgrp は名前でグループを引いて付け替える。
func Chgrp(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, -1, gid)
}

// ChmodWhenExists は現れるのを待ってから権限を付ける。
//
// 相手が作るものを待つ。無条件に chmod すると、まだ無いだけなのか作られない
// のかを区別できない。
func ChmodWhenExists(path string, mode os.FileMode, tries int) error {
	for i := 0; i < tries; i++ {
		if _, err := os.Stat(path); err == nil {
			return os.Chmod(path, mode)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s が現れません", path)
}
