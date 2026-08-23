package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// UnitDir は Quadlet が rootless ユーザの定義を探す場所を返す。
func UnitDir(home string) string {
	return filepath.Join(home, ".config", "containers", "systemd")
}

// SystemdUserDir は systemd がユーザの unit を探す場所を返す。
//
// Quadlet のディレクトリとは別。Quadlet は自分が知る種類 (.container,
// .volume, .network, .pod など) しか処理せず、それ以外のファイルは無視する。
// .timer をそこへ置いても systemd からは見えない。
func SystemdUserDir(home string) string {
	return filepath.Join(home, ".config", "systemd", "user")
}

// WriteUnits は unit ファイル群をユーザのホームへ書き出す。
//
// 中間ディレクトリまで所有者を明示的に移すのが要点。以前 systemd-tmpfiles で
// 末端だけに所有者を指定したところ、.config と .config/containers が root 所有で
// 作られ、
//
//	Detected unsafe path transition /var/lib/x (owned by x) → /var/lib/x/.config (owned by root)
//
// として以降の処理が拒否され、podman も
//
//	path ".../.config" exists and it is not owned by the current user
//
// で起動できなくなった。
func WriteUnits(home string, uid, gid int, units map[string]string) error {
	// .timer は Quadlet のディレクトリへ置いても効かない。Quadlet は自分が
	// 知る種類しか処理せず、それ以外は無視するため、systemd からは見えない
	// ファイルが 1 つ増えるだけになる。置き場所で振り分ける。
	quadlet := map[string]string{}
	plain := map[string]string{}
	for name, content := range units {
		if strings.HasSuffix(name, ".container") ||
			strings.HasSuffix(name, ".volume") ||
			strings.HasSuffix(name, ".network") ||
			strings.HasSuffix(name, ".pod") ||
			strings.HasSuffix(name, ".build") ||
			strings.HasSuffix(name, ".image") ||
			strings.HasSuffix(name, ".kube") {
			quadlet[name] = content
			continue
		}
		plain[name] = content
	}
	if err := writeUnitDir(home, UnitDir(home), uid, gid, quadlet); err != nil {
		return err
	}
	return writeUnitDir(home, SystemdUserDir(home), uid, gid, plain)
}

// writeUnitDir は 1 つのディレクトリを宣言どおりの中身にする。
func writeUnitDir(home, dir string, uid, gid int, units map[string]string) error {
	// home からの各段を順に作り、そのつど所有者を移す。
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		return err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		p := filepath.Join(append([]string{home}, parts[:i+1]...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if err := os.Chown(p, uid, gid); err != nil {
			return fmt.Errorf("%s の所有者を移せません: %w", p, err)
		}
	}

	// 宣言に無い unit は消す。残しておくと、アプリ構成を変えたときに古い
	// コンテナが動き続ける。
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// timers.target.wants のような systemd が作る symlink 置き場は
		// 触らない。enable した結果がここに入る。
		if e.IsDir() {
			continue
		}
		if _, keep := units[e.Name()]; !keep {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}

	for name, content := range units {
		p := filepath.Join(dir, name)
		// 内容が同じなら書かない。書き換えると systemd が変更を検知して
		// 無用な再起動を招く。
		if old, err := os.ReadFile(p); err == nil && string(old) == content {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// ReloadUserUnits はユーザの systemd に定義を読み直させる。
//
// linger を有効にしても user@<uid>.service の起動は即座ではない。ユーザを
// 作った直後に呼ぶと
//
//	Failed to connect to user scope bus via local transport: No such file or directory
//
// で失敗する。起動を待ってから読み直す。
func ReloadUserUnits(ctx context.Context, r Runner, user string, uid int) error {
	if err := waitUserInstance(ctx, r, uid); err != nil {
		return fmt.Errorf("%s の systemd が起動しません: %w", user, err)
	}
	// ssh 経由の非対話セッションでは XDG_RUNTIME_DIR が設定されない。無いと
	// systemctl --user が DBUS_SESSION_BUS_ADDRESS も無いと言って失敗する。
	_, err := r.Run(ctx, nil, "systemd-run",
		"--uid", strconv.Itoa(uid),
		"--setenv", "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(uid),
		"--pipe", "--wait", "--quiet", "--collect",
		"systemctl", "--user", "daemon-reload")
	if err != nil {
		return fmt.Errorf("%s の unit を読み直せません: %w", user, err)
	}
	return nil
}

// userInstanceWaitTries は起動を待つ回数。テストから縮められるよう変数にしてある。
var userInstanceWaitTries = 30

// waitUserInstance はユーザの systemd インスタンスが立ち上がるのを待つ。
func waitUserInstance(ctx context.Context, r Runner, uid int) error {
	unit := fmt.Sprintf("user@%d.service", uid)
	// linger を入れた直後は自動起動を待つより明示的に始める方が速く確実。
	_, _ = r.Run(ctx, nil, "systemctl", "start", unit)

	for i := 0; i < userInstanceWaitTries; i++ {
		if _, err := r.Run(ctx, nil, "systemctl", "is-active", "--quiet", unit); err == nil {
			// bus のソケットが現れるまでにさらに一拍ある。
			if _, err := os.Stat(fmt.Sprintf("/run/user/%d/bus", uid)); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s が起動しませんでした", unit)
}

// EnableUserTimers は timer を有効にして起動する。
//
// ファイルを置いて daemon-reload しただけでは timer は動かない。
// timers.target.wants への symlink が要り、それを作るのが enable。
// --now を付けて、次の収束を待たずにその場で起動させる。
func EnableUserTimers(ctx context.Context, r Runner, uid int, timers []string) error {
	if len(timers) == 0 {
		return nil
	}
	args := []string{
		"--uid", strconv.Itoa(uid),
		"--setenv", "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(uid),
		"--pipe", "--wait", "--quiet", "--collect",
		"systemctl", "--user", "enable", "--now",
	}
	args = append(args, timers...)
	if _, err := r.Run(ctx, nil, "systemd-run", args...); err != nil {
		return fmt.Errorf("timer を有効にできません: %w", err)
	}
	return nil
}

// StopUser はユーザの systemd インスタンスを止める。配下のコンテナごと止まる。
//
// linger も外す。残しておくと再起動で勝手に立ち上がり、止めたつもりのものが
// 動き続ける。
//
// 失敗は無視する。ユーザが既に無い場合や、そもそも起動していない場合に
// エラーになるが、どちらも「止まっている」という目的は達している。
func StopUser(ctx context.Context, r Runner, user string, uid int) {
	_, _ = r.Run(ctx, nil, "loginctl", "disable-linger", user)
	_, _ = r.Run(ctx, nil, "systemctl", "stop", fmt.Sprintf("user@%d.service", uid))
}
