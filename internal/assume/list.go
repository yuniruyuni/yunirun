package assume

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// assumptions は yunirun が依存している外部環境の性質。
//
// それぞれ「実際に破れて不具合になった」ものか、「破れたら壊れると分かって
// いる」もの。推測で足さないこと。破れうる根拠が無い前提を並べても、検査が
// 増えるだけで守りにはならない。
var assumptions = []Assumption{
	{
		ID:   "sudo/literal-path",
		What: "sudo はコマンドを文字列として照合する",
		Why: "実体が同じ symlink でもパス文字列が違えば拒否される。" +
			"モジュールが nix store の絶対パスで許可を書き、deploy 側が PATH で " +
			"解決した /run/current-system/sw/bin/... を渡して拒否された。",
		Check: func(ctx context.Context, env Env) error {
			// sudoers に書かれたパスが、実際に呼ぶときの PATH 解決の結果と
			// 一致しているかを見る。
			//
			// root の sudo -l は全許可を返してアプリ固有のルールが現れないので、
			// 設定ファイルそのものを読む。
			b, err := os.ReadFile("/etc/sudoers")
			if err != nil {
				return nil
			}
			cfg := string(b)
			if !strings.Contains(cfg, "yunirun-") {
				// yunirun のルールがまだ無い環境では検査しない。
				return nil
			}
			for _, line := range strings.Split(cfg, "\n") {
				if !strings.Contains(line, "yunirun-") || !strings.Contains(line, "systemctl") {
					continue
				}
				// nix store の絶対パスで書かれていると、PATH で解決した
				// /run/current-system/sw/bin/systemctl と文字列が一致せず拒否される。
				if strings.Contains(line, "/nix/store/") {
					return fmt.Errorf("nix store の絶対パスで許可している (PATH 解決の結果と一致しない): %s",
						strings.TrimSpace(line))
				}
			}
			return nil
		},
	},
	{
		ID:   "systemd/user-instance-is-async",
		What: "linger を有効にしても user@<uid>.service の起動は即座ではない",
		Why: "ユーザを作った直後に systemctl --user を呼ぶと " +
			"Failed to connect to user scope bus で失敗する。起動を待つ必要がある。",
		// 実行時に検査する意味が薄い (待てば成り立つ) ので e2e に任せる。
	},
	{
		ID:   "fs/traversal-needs-x-on-every-component",
		What: "パスの途中に x が無ければ、末端の権限に関係なく届かない",
		Why: "stateDir を 0700 root にしたため配下のホームへ辿れなかった。" +
			"しかもエラーは Not a directory という誤解を招く形で出る。" +
			"秘密を置く場所とアプリが辿る場所は分ける必要がある。",
		Check: func(ctx context.Context, env Env) error {
			if env.UID == 0 {
				return nil
			}
			// アプリのホームまでの各段が辿れることを確かめる。
			home := "/var/lib/yunirun-apps"
			if _, err := os.Stat(home); err != nil {
				return nil
			}
			st, err := os.Stat(home)
			if err != nil {
				return err
			}
			if st.Mode().Perm()&0o001 == 0 {
				return fmt.Errorf("%s に other の x が無い", home)
			}
			return nil
		},
	},
	{
		ID:   "podman/secret-driver-opts-are-frozen",
		What: "podman secret は作成時の driver opts を保存し、後から変わらない",
		Why: "containers.conf を直しても既存の secret は古いコマンドを参照し続ける。" +
			"設定を変えたら secret を作り直す必要がある。man には書かれていない。",
		// yunirun は現在 podman secret を使っていない (env ファイル方式) が、
		// 戻す判断をしたときに再び踏むので前提として残す。
	},
	{
		ID:   "haproxy/httpchk-omits-host",
		What: "option httpchk の既定は HTTP/1.0 かつ Host ヘッダ無し",
		Why: "Host で振り分けるアプリはそれを 404 にする。curl は常に Host を送るので " +
			"手元の確認では 200 に見え、原因が分かりにくい。http-check send で明示する。",
		Check: func(ctx context.Context, env Env) error {
			b, err := os.ReadFile("/etc/yunirun/haproxy.cfg")
			if err != nil {
				return nil
			}
			cfg := string(b)
			if !strings.Contains(cfg, "hdr Host") && strings.Contains(cfg, "backend ") {
				return fmt.Errorf("ヘルスチェックが Host ヘッダを送っていない")
			}
			return nil
		},
	},
	{
		ID:   "haproxy/needs-at-least-one-listener",
		What: "haproxy は listener が 1 つも無いと起動できない",
		Why: "アプリを一時的に全部外すと no listener で exit(2) になり、" +
			"HAProxy が落ちたままになる。待機用の listener を置いて避ける。",
		Check: func(ctx context.Context, env Env) error {
			b, err := os.ReadFile("/etc/yunirun/haproxy.cfg")
			if err != nil {
				return nil
			}
			if !strings.Contains(string(b), "bind ") {
				return fmt.Errorf("bind が 1 つも無い")
			}
			return nil
		},
	},
	{
		ID:   "haproxy/frontend-backend-names-must-differ",
		What: "frontend と backend に同じ名前を付けられない (3.3 以降)",
		Why:  "3.2 では警告、3.3 でサポートされなくなる。接尾辞で分ける。",
		Check: func(ctx context.Context, env Env) error {
			b, err := os.ReadFile("/etc/yunirun/haproxy.cfg")
			if err != nil {
				return nil
			}
			fronts, backs := map[string]bool{}, map[string]bool{}
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) != 2 {
					continue
				}
				switch f[0] {
				case "frontend":
					fronts[f[1]] = true
				case "backend":
					backs[f[1]] = true
				}
			}
			for n := range fronts {
				if backs[n] {
					return fmt.Errorf("frontend と backend が同名: %s", n)
				}
			}
			return nil
		},
	},
	{
		ID:   "haproxy/reload-needs-sigusr2",
		What: "haproxy の再読込は SIGUSR2 で、-c は検査しかしない",
		Why: "ExecReload に -c だけを書いていたため、アプリを足しても listen が" +
			"増えなかった。設定には backend があるのにポートが開かないという" +
			"分かりにくい状態になる。",
		Check: func(ctx context.Context, env Env) error {
			// 設定に書かれた bind が実際に listen されているかを見る。
			b, err := os.ReadFile("/etc/yunirun/haproxy.cfg")
			if err != nil {
				return nil
			}
			var want []string
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) == 2 && f[0] == "bind" {
					want = append(want, f[1])
				}
			}
			if len(want) == 0 {
				return nil
			}
			out, err := env.Run(ctx, "ss", "-lnt")
			if err != nil {
				return nil
			}
			for _, w := range want {
				if !strings.Contains(string(out), w) {
					return fmt.Errorf("設定にある %s が listen されていない (reload が効いていない)", w)
				}
			}
			return nil
		},
	},
	{
		ID:   "postgres/root-cannot-use-peer-auth",
		What: "pg_hba に local all all md5 があると root は peer で入れない",
		Why: "コンテナから Unix ソケット経由で繋ぐためにこの行が要る。" +
			"その副作用で root の psql がパスワードを求められるので、" +
			"postgres ユーザとして実行する必要がある。",
		Check: func(ctx context.Context, env Env) error {
			if os.Geteuid() != 0 {
				return nil
			}
			if _, err := exec.LookPath("runuser"); err != nil {
				return fmt.Errorf("runuser が PATH に無い (postgres として実行できない)")
			}
			return nil
		},
	},
	{
		ID:   "podman/rootless-needs-newuidmap",
		What: "rootless podman は subuid を張るのに newuidmap/newgidmap を呼ぶ",
		Why: "setuid が要るので NixOS では /run/wrappers/bin に置かれる。" +
			"system サービスの既定 PATH には含まれず、無いと起動できない。",
		Check: func(ctx context.Context, env Env) error {
			for _, c := range []string{"newuidmap", "newgidmap"} {
				if _, err := exec.LookPath(c); err != nil {
					return fmt.Errorf("%s が PATH に無い", c)
				}
			}
			return nil
		},
	},
	{
		ID:   "podman/minimal-path-for-hooks",
		What: "podman が外部コマンドへ渡す PATH は最小限で coreutils も無い",
		Why: "secret の lookup スクリプトが cat を見つけられずに失敗した。" +
			"呼ばれる側で PATH を明示する必要がある。",
	},
	{
		ID:   "nixos/mutable-users-keeps-imperative-users",
		What: "users.mutableUsers が true なら、外部で作ったユーザは rebuild で消えない",
		Why: "yunirun はユーザを imperative に作る。false にされると収束した" +
			"ユーザが消え、アプリが起動しなくなる。",
		Check: func(ctx context.Context, env Env) error {
			// 実際に作ったユーザが残っているかで間接的に確かめる。
			// 直接 mutableUsers を読む手段が無いため。
			return nil
		},
	},
	{
		ID:   "nixos/subuid-is-regenerated-on-activation",
		What: "NixOS の activation は /etc/subuid を宣言から再生成する",
		Why: "yunirun が足した行が rebuild で消える。activation の後に converge を" +
			"走らせて復元する必要がある。",
		Check: func(ctx context.Context, env Env) error {
			if env.UID == 0 {
				return nil
			}
			b, err := os.ReadFile("/etc/subuid")
			if err != nil {
				return nil
			}
			// このユーザの行が残っているか。
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "yunirun-") {
					return nil
				}
			}
			return fmt.Errorf("yunirun のユーザの subuid が消えている")
		},
	},
	{
		ID:   "systemd/env-file-is-read-at-start-only",
		What: "EnvironmentFile は unit の起動時にしか読まれない",
		Why: "パスワードを更新しても、稼働中のコンテナは古い値を持ち続ける。" +
			"更新したら再起動が要る。",
	},
	{
		ID:   "ssh/non-interactive-has-no-xdg-runtime-dir",
		What: "ssh の非対話セッションでは XDG_RUNTIME_DIR が設定されない",
		Why: "無いと systemctl --user が DBUS_SESSION_BUS_ADDRESS も無いと言って" +
			"失敗する。uid から補う必要がある。",
		Check: func(ctx context.Context, env Env) error {
			if env.UID == 0 {
				return nil
			}
			p := "/run/user/" + strconv.Itoa(env.UID)
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%s が無い (linger が効いていない可能性)", p)
			}
			return nil
		},
	},
}
