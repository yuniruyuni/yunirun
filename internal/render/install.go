package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
)

// InstallInput は install が置くものを決めるのに要る情報。
//
// 実行時の探索結果 (自分の位置や systemctl の位置) を外から渡す形にして、
// 置く中身をテストから確かめられるようにしてある。
type InstallInput struct {
	// Exe は yunirun 自身の絶対パス。unit の ExecStart になる。
	Exe string
	// Systemctl は sudo に許す systemctl の絶対パス。
	//
	// sudo はコマンドを文字列で照合する。呼び出し側が PATH で解決した結果と
	// 一致しなければ、実体が同じでも拒否される。
	Systemctl string
	// Apps はアプリ名の一覧。sudoers の許可はここから作る。
	Apps []string
	// Dirs は用意するディレクトリ。パスから 8 進のモードへの対応。
	Dirs map[string]string
	// Usage は資源使用を出す unit を置くか。
	Usage bool
}

// InstallFile は install が置く 1 つのファイル。
type InstallFile struct {
	Path    string
	Mode    uint32
	Content string
}

// InstallUnits は置く unit とその他の設定ファイルを返す。
func InstallUnits(in InstallInput) []InstallFile {
	out := []InstallFile{
		{
			Path: "/etc/systemd/system/yunirun-converge.service",
			Mode: 0o644,
			Content: unit("yunirun: 宣言されたアプリ一覧に実体を一致させる", []string{
				// image の取得に network が要る。順序だけでは target 自体が
				// 起動しないので wants も要る。
				"After=network-online.target",
				"Wants=network-online.target",
			}, []string{
				"Type=oneshot",
				"RemainAfterExit=true",
				"ExecStart=" + in.Exe + " converge",
			}, "multi-user.target"),
		},
		{
			Path: "/etc/systemd/system/yunirun-migrate@.service",
			Mode: 0o644,
			Content: unit("yunirun: %i の schema を適用する", nil, []string{
				"Type=oneshot",
				"ExecStart=" + in.Exe + " migrate %i",
			}, ""),
		},
		{
			Path:    "/etc/tmpfiles.d/yunirun.conf",
			Mode:    0o644,
			Content: tmpfiles(in.Dirs),
		},
		{
			Path:    "/etc/sudoers.d/yunirun",
			Mode:    0o440,
			Content: sudoers(in.Systemctl, in.Apps),
		},
	}
	if in.Usage {
		out = append(out, InstallFile{
			Path: "/etc/systemd/system/yunirun-usage.service",
			Mode: 0o644,
			Content: unit("yunirun: アプリごとの資源使用を出す", []string{
				"After=yunirun-converge.service",
			}, []string{
				"ExecStart=" + in.Exe + " usage",
				"Restart=always",
				"RestartSec=10s",
				// 読むのは cgroup と台帳だけ。書くものは無い。
				"ProtectSystem=strict",
				"ProtectHome=true",
				"PrivateTmp=true",
				"NoNewPrivileges=true",
			}, "multi-user.target"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func unit(desc string, unitLines, serviceLines []string, wantedBy string) string {
	var b strings.Builder
	b.WriteString("# yunirun install が生成。手で書き換えても次の install で戻る。\n")
	b.WriteString("[Unit]\nDescription=" + desc + "\n")
	for _, l := range unitLines {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n[Service]\n")
	for _, l := range serviceLines {
		b.WriteString(l + "\n")
	}
	if wantedBy != "" {
		b.WriteString("\n[Install]\nWantedBy=" + wantedBy + "\n")
	}
	return b.String()
}

// tmpfiles は再起動後もディレクトリが在るようにする規則を返す。
//
// mkdir で済ませない理由は /run/yunirun。tmpfs なので再起動で消える。
func tmpfiles(dirs map[string]string) string {
	var b strings.Builder
	b.WriteString("# yunirun install が生成。手で書き換えても次の install で戻る。\n")
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "d %s %s root root -\n", p, dirs[p])
	}
	return b.String()
}

// sudoers は deploy ユーザに許すことを返す。
//
// 許すのは自分のアプリの migration を起動することと、converge を回すことだけ。
// converge は宣言に無いことをしないので、他のアプリへは影響しない。
//
// converge は restart で許す。RemainAfterExit=true で active (exited) のまま
// 留まるため、start では何も起きない。
func sudoers(systemctl string, apps []string) string {
	var b strings.Builder
	b.WriteString("# yunirun install が生成。手で書き換えても次の install で戻る。\n")
	sorted := append([]string(nil), apps...)
	sort.Strings(sorted)
	for _, a := range sorted {
		u := alloc.User(a)
		fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s start yunirun-migrate@%s.service\n", u, systemctl, a)
		fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s restart yunirun-converge.service\n", u, systemctl)
	}
	return b.String()
}
