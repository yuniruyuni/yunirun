package system

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// IDMap は /proc/<pid>/uid_map (gid_map) の 1 行。
//
// Inside は名前空間の中の番号、Outside はホスト側の番号、Count は幅。
type IDMap struct {
	Inside  int
	Outside int
	Count   int
}

// ParseIDMap は uid_map / gid_map の中身を読む。
//
// 形式は空白区切りの 3 列。カーネルは右寄せで出すので、桁揃えの空白は
// 区切りと同じに扱う。
func ParseIDMap(s string) ([]IDMap, error) {
	var out []IDMap
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("uid_map の行を読めません: %q", line)
		}
		var n [3]int
		for i, v := range f {
			x, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("uid_map の行を読めません: %q", line)
			}
			n[i] = x
		}
		out = append(out, IDMap{Inside: n[0], Outside: n[1], Count: n[2]})
	}
	return out, nil
}

// CoversOutside はホスト側の [start, start+count) がすべて写像に含まれるかを返す。
//
// 1 つの行に収まっている必要は無い。podman は subuid の範囲を複数の行に
// 分けて張ることがあるので、隣り合う行をつないだ全体で判定する。
func CoversOutside(maps []IDMap, start, count int) bool {
	next := start
	end := start + count
	for next < end {
		advanced := false
		for _, m := range maps {
			if m.Outside <= next && next < m.Outside+m.Count {
				next = m.Outside + m.Count
				advanced = true
			}
		}
		if !advanced {
			return false
		}
	}
	return true
}

// UserNamespace はアプリのユーザの podman が実際に使っているユーザ名前空間。
type UserNamespace struct {
	UIDMap []IDMap
	GIDMap []IDMap
}

// Stale は割り当てた subuid/subgid が名前空間に張られていないかを返す。
//
// /etc/subuid に行があることと、名前空間がそれを使っていることは別。
// rootless podman は最初にユーザ名前空間 (pause プロセス) を作った時点の
// /etc/subuid を写し取り、以降はファイルが変わっても作り直さない。行が
// 欠けた瞬間に作られると、uid 1 個だけの写像のまま残り続ける。
func (n UserNamespace) Stale(a alloc.Alloc) bool {
	return !CoversOutside(n.UIDMap, a.SubUID, alloc.SubUIDSize) ||
		!CoversOutside(n.GIDMap, a.SubUID, alloc.SubUIDSize)
}

// asUser はアプリのユーザとしてコマンドを実行する systemd-run の引数を作る。
//
// ReloadUserUnits と同じく XDG_RUNTIME_DIR を補う。PATH も引き継ぐ。podman は
// newuidmap/newgidmap を PATH から探すが、それは NixOS では /run/wrappers/bin に
// あり、system 側の transient unit の既定 PATH には含まれない。
func asUser(uid int, cmd ...string) []string {
	args := []string{
		"--uid", strconv.Itoa(uid),
		"--setenv", "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(uid),
	}
	if p := userPath(os.Getenv("PATH")); p != "" {
		args = append(args, "--setenv", "PATH="+p)
	}
	args = append(args, "--pipe", "--wait", "--quiet", "--collect")
	return append(args, cmd...)
}

// setuidWrappers は NixOS が setuid の newuidmap/newgidmap を置く場所。
const setuidWrappers = "/run/wrappers/bin"

// userPath はアプリのユーザとして podman を動かすときの PATH を作る。
//
// setuid の wrapper を先頭に置く。converge の PATH には shadow の
// パッケージそのもの (useradd のため) が wrapper より前にあり、そのまま
// 渡すと podman は setuid の付いていない newuidmap を拾って
// "write to uid_map failed: Operation not permitted" で失敗する (VM の e2e で
// 踏んだ)。NixOS 以外では wrapper の場所が無いので何もしない。
func userPath(path string) string {
	if _, err := os.Stat(setuidWrappers); err != nil {
		return path
	}
	if path == "" {
		return setuidWrappers
	}
	return setuidWrappers + ":" + path
}

// ProbeUserNamespace はアプリのユーザの podman が使う名前空間の写像を読む。
//
// podman unshare は既存の名前空間 (pause プロセス、無ければ動いている
// コンテナ) に入るので、実際にコンテナが使っている写像が見える。どれも
// 無ければその場で今の /etc/subuid から作る。
func ProbeUserNamespace(ctx context.Context, r Runner, uid int) (UserNamespace, error) {
	var ns UserNamespace
	for _, f := range []struct {
		path string
		dst  *[]IDMap
	}{
		{"/proc/self/uid_map", &ns.UIDMap},
		{"/proc/self/gid_map", &ns.GIDMap},
	} {
		out, err := r.Run(ctx, nil, "systemd-run", asUser(uid, "podman", "unshare", "cat", f.path)...)
		if err != nil {
			return UserNamespace{}, fmt.Errorf("uid=%d の名前空間を読めません: %w", uid, err)
		}
		m, err := ParseIDMap(string(out))
		if err != nil {
			return UserNamespace{}, err
		}
		*f.dst = m
	}
	return ns, nil
}

// MigrateUserNamespace は podman に名前空間を作り直させる。
//
// podman system migrate は pause プロセスを止め、動いているコンテナも止める。
// 次に podman を使ったときに今の /etc/subuid で作り直される。
func MigrateUserNamespace(ctx context.Context, r Runner, uid int) error {
	if _, err := r.Run(ctx, nil, "systemd-run", asUser(uid, "podman", "system", "migrate")...); err != nil {
		return fmt.Errorf("uid=%d の podman system migrate に失敗しました: %w", uid, err)
	}
	return nil
}

// restartUserInstance はテストから差し替える。本物は bus のソケットが
// 現れるのを待つので、テストの中では終わらない。
var restartUserInstance = RestartUserInstance

// EnsureUserNamespace は、アプリのユーザの名前空間が割り当てた subuid を
// 使っていなければ作り直す。作り直したときは true を返す。
//
// 実際に踏んだ: ホストの再起動で NixOS の activation が /etc/subuid を再生成し、
// yunirun が足した行が消えた。converge が戻すより先にユーザの unit が起動して
// podman が uid 1 個だけの名前空間を作り、行が戻った後もそれを使い続けた。
// root で動く image は 1 個の写像でも動くので気付けず、非 root (uid 65532) の
// image だけが "insufficient UIDs or GIDs available in user namespace" で
// 展開できずに落ちた。/etc/subuid を見る検査はすべて通っていた。
//
// 作り直すとそのユーザのコンテナは一度止まるので、systemd のインスタンスごと
// 入れ直して unit から起動し直させる。写像が欠けたまま動き続けるより短い断を選ぶ。
func EnsureUserNamespace(ctx context.Context, r Runner, name string, a alloc.Alloc) (bool, error) {
	ns, err := ProbeUserNamespace(ctx, r, a.UID)
	if err != nil {
		return false, err
	}
	if !ns.Stale(a) {
		return false, nil
	}
	fmt.Printf("==> %s: podman の名前空間に subuid %d:%d が張られていないので作り直します\n",
		name, a.SubUID, alloc.SubUIDSize)
	if err := MigrateUserNamespace(ctx, r, a.UID); err != nil {
		return false, err
	}
	if err := restartUserInstance(ctx, r, a.UID); err != nil {
		return false, err
	}
	// 作り直した結果を確かめる。/etc/subuid 自体が欠けていれば migrate しても
	// 同じ写像に戻るので、直ったと言わずにエラーにする。
	ns, err = ProbeUserNamespace(ctx, r, a.UID)
	if err != nil {
		return true, err
	}
	if ns.Stale(a) {
		return true, fmt.Errorf("%s: 作り直しても subuid %d:%d が名前空間に張られません (/etc/subuid と /etc/subgid を確認してください)",
			name, a.SubUID, alloc.SubUIDSize)
	}
	return true, nil
}
