// Package render は、設定とマニフェストから配置物を組み立てる。
//
// 副作用を持たない。ここで作った文字列を書き出すのは呼び出し側の仕事にして、
// 生成規則そのものをテストで固定できるようにしてある。
package render

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/manifest"
)

// Colors は blue/green の名前。順序が入れ替え順になる。
var Colors = []string{"blue", "green"}

// MetricsPort は Prometheus 形式の計測値を出す listener のポート。
//
// アプリの割り当て帯 (basePort 以降) の外に取る。
const MetricsPort = 8098

// PlaceholderPort はアプリが 0 個のときに置く待機用 listener のポート。
//
// アプリの割り当て帯 (8100 以降) より手前に取って、実アプリと重ならないようにする。
const PlaceholderPort = 8099

// App は 1 アプリ分の生成に必要な情報をまとめたもの。
type App struct {
	Name     string
	User     string
	Alloc    alloc.Alloc
	Manifest *manifest.Manifest
	// LocalTag はコンテナが参照するローカルタグ。デプロイ時に実体を
	// 付け替えるので、unit 自体は静的なまま中身だけ入れ替わる。
	LocalTag string
	// DBName と DBUser はアプリ本体が使う DB とロール。
	DBName string
	DBUser string
	// TraceSockDir はトレースの口があるホスト側の場所。空なら渡さない。
	TraceSockDir string
	// SockDir はこのアプリ専用 PostgreSQL のソケットがある場所。
	// コンテナにはここだけを見せるので、他アプリの DB へは到達しようがない。
	SockDir string
	// EnvFile は秘密を含む環境変数ファイルの位置。読めるのはそのワークロードの
	// ユーザだけにする。
	//
	// 永続領域に置く。tmpfs に置いていたころ、再起動で消えた env を unit が
	// EnvironmentFile= (先頭の - 無し) で参照して起動に失敗していた。復号済みの
	// 秘密がディスクに残ることになるが、age の識別鍵が既に平文でディスク上に
	// あるので、ディスクを読める者は今でも全ての秘密を復号できる。
	//
	// unit ファイルにはパスしか現れないので、値が世界読み取り可能な場所へ
	// 漏れることはない。runtime の EnvFile には app パスワードしか入れず、
	// owner パスワードは root しか読めない別のファイルに置く。これで
	// 「runtime は owner パスワードを持てない」が構造として成立する。
	EnvFile string
}

// Port は色に対応する publish 先ポートを返す。
func (a App) Port(color string) int {
	if color == "green" {
		return a.Alloc.Green
	}
	return a.Alloc.Blue
}

// ContainerUnit は Quadlet の .container を組み立てる。
func ContainerUnit(a App, color string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("[Unit]")
	p("Description=%s (%s)", a.Name, color)
	p("")
	p("[Container]")
	p("Image=%s", a.LocalTag)
	p("PublishPort=127.0.0.1:%d:%d", a.Port(color), a.Manifest.App.Port)
	p("Environment=PORT=%d", a.Manifest.App.Port)

	// 秘密でない環境変数。値はマニフェストに書かれたものをそのまま渡す。
	// 秘密は EnvironmentFile 経由なのでここには現れない。
	for _, k := range sortedKeys(a.Manifest.App.Env) {
		p("Environment=%s=%s", k, a.Manifest.App.Env[k])
	}

	if a.DBName != "" {
		// PostgreSQL へは TCP ではなく Unix ソケットで繋ぐ。コンテナは独立した
		// netns にいるので、これによりホストの loopback 上の他サービスへは
		// 到達できないまま DB にだけ届く。
		p("Volume=%s:/run/postgresql", a.SockDir)
		p("Environment=PGHOST=/run/postgresql")
		p("Environment=PGPORT=5432")
		p("Environment=DB_USER=%s", a.DBUser)
		p("Environment=DB_NAME=%s", a.DBName)
	}

	if a.TraceSockDir != "" {
		// トレースも DB と同じくソケットで渡す。コンテナはホストの loopback
		// へ到達できないため、TCP では届かない (実測で確認済み)。
		p("Volume=%s:%s", a.TraceSockDir, filepath.Dir(TempoSocketPath))
		// ソケットはグループで守ってある。ユーザ名前空間の中では補助グループが
		// そのまま見えないので、keep-groups でホスト側の所属を保たせる。
		// これが無いと権限で弾かれる (実測で確認済み)。
		p("GroupAdd=keep-groups")
		p("Environment=OTEL_EXPORTER_OTLP_ENDPOINT=unix://%s", TempoSocketPath)
		// 平文で話す。指定しないと gRPC が TLS を試み、WRONG_VERSION_NUMBER で
		// 失敗する (実測で確認済み)。
		p("Environment=OTEL_EXPORTER_OTLP_INSECURE=true")
		p("Environment=OTEL_SERVICE_NAME=%s", a.Name)
	}

	if a.EnvFile != "" {
		p("EnvironmentFile=%s", a.EnvFile)
	}

	p("")
	p("[Service]")
	p("Restart=always")
	// image がまだ無い初回は起動できないので、諦めずに待つ。
	p("RestartSec=10")
	p("")
	p("[Install]")
	p("WantedBy=default.target")
	return b.String()
}

// HAProxy は全アプリ分の設定を組み立てる。
func HAProxy(apps []App) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("global")
	// 標準出力へ出す。コンテナの中には /dev/log が無いので、そこを指すと
	// ログが黙って消える。podman が拾って journald へ流す。
	p("  log stdout format raw local0")
	p("  maxconn 2048")
	p("")
	p("defaults")
	p("  mode http")
	p("  log global")
	p("  option httplog")
	p("  timeout connect 5s")
	p("  timeout client  60s")
	p("  timeout server  60s")

	// 計測用の listener。常に置く。
	//
	// HAProxy は各バックエンドを health check しているので、どの系が落ちて
	// いるかを既に知っている。それが外から見えないと、CDN が古い応答を
	// 返している間オリジンの停止に気付けない。
	p("")
	p("frontend yunirun_metrics")
	p("  bind 127.0.0.1:%d", MetricsPort)
	p("  http-request use-service prometheus-exporter if { path /metrics }")
	// 他のパスは何も返さない。ここは計測専用の口で、アプリへは繋がない。
	p("  http-request return status 404")

	if len(apps) == 0 {
		// listener が 1 つも無いと haproxy は
		//   Configuration file has no error but will not start (no listener)
		// として exit(2) する。アプリを一時的に全部外したときに haproxy が
		// 落ちたままにならないよう、何も受けない listener を置いておく。
		p("")
		p("# アプリが 1 つも無いときのための待機用 listener。")
		p("# haproxy は listener が無いと起動できない。")
		p("frontend yunirun_placeholder")
		p("  bind 127.0.0.1:%d", PlaceholderPort)
		p("  http-request return status 503")
		return b.String()
	}

	sorted := append([]App(nil), apps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, a := range sorted {
		p("")
		// frontend と backend を同名にすると haproxy が
		//   backend 'x' has the same name as frontend 'x'.
		//   This is dangerous and will not be supported anymore in version 3.3.
		// と警告する。接尾辞で分ける。
		p("frontend %s_in", a.Name)
		p("  bind 127.0.0.1:%d", a.Alloc.Frontend)
		p("  default_backend %s_out", a.Name)
		p("")
		p("backend %s_out", a.Name)
		// Host ヘッダを明示する。option httpchk の既定は HTTP/1.0 かつ Host 無しで、
		// Host で振り分けるアプリはそれを 404 にする。curl は常に Host を送るため
		// 手元の確認では 200 に見え、原因が分かりにくい。
		p("  option httpchk")
		p("  http-check send meth GET uri %s ver HTTP/1.1 hdr Host localhost", a.Manifest.App.Health)
		p("  http-check expect status 200")
		for _, c := range Colors {
			p("  server %s 127.0.0.1:%d check inter 3s fall 2 rise 3", c, a.Port(c))
		}
	}
	return b.String()
}

// sortedKeys は map の鍵を名前順で返す。
//
// 生成を決定的にするため。map の反復順に任せると、内容が同じでも converge の
// たびに unit が書き換わって無用な再起動を招く。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WorkloadUnit はアプリ本体以外のワークロードの .container を組み立てる。
//
// 一度だけ走るものなので Restart=always にはしない。
func WorkloadUnit(a App, name string, w WorkloadSpec) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("[Unit]")
	p("Description=%s (%s)", a.Name, name)
	p("")
	p("[Container]")
	p("Image=%s", w.Image)
	// アプリ本体と同じ設定を渡す。ワークロードの多くは同じバイナリを別の
	// 入口で動かすもので、保持期間やバッチ幅といった設定を本体と共有する。
	// 渡さないと既定値で動いてしまい、しかも黙って動くので気付けない。
	//
	// ここに乗るのは秘密でない値だけ。秘密は EnvironmentFile 経由で、
	// ワークロードごとに読めるものが違う (migration だけが owner
	// パスワードを読める) という分離はそのまま保たれる。
	for _, k := range sortedKeys(a.Manifest.App.Env) {
		p("Environment=%s=%s", k, a.Manifest.App.Env[k])
	}
	// ワークロード固有の値は後に置く。systemd は同じ名前が複数あれば
	// 後のものを採るので、これで app.env を上書きできる。
	for _, k := range sortedKeys(w.Env) {
		p("Environment=%s=%s", k, w.Env[k])
	}
	if a.DBName != "" {
		p("Volume=%s:/run/postgresql", a.SockDir)
		p("Environment=PGHOST=/run/postgresql")
		p("Environment=PGPORT=5432")
		p("Environment=DB_USER=%s", w.DBUser)
		p("Environment=DB_NAME=%s", a.DBName)
		p("Environment=DB_APP_NAME=%s", a.DBName)
	}
	if w.EnvFile != "" {
		p("EnvironmentFile=%s", w.EnvFile)
	}
	// Exec はコマンドライン全体を 1 行で書く。引数ごとに 1 行にすると、
	// 最後の行だけが効いて残りが落ちる。引数が 1 つのうちは偶然動くので
	// 気付きにくい。
	if len(w.Args) > 0 {
		p("Exec=%s", strings.Join(w.Args, " "))
	}
	p("")
	p("[Service]")
	p("Type=oneshot")
	return b.String()
}

// WorkloadSpec は 1 ワークロード分の生成に必要な情報。
type WorkloadSpec struct {
	Image   string
	Args    []string
	EnvFile string
	DBUser  string
	// Env はこのワークロードだけの環境変数。app.env より後に置かれるので
	// 同じ名前があればこちらが勝つ。
	Env map[string]string
	// Schedule があれば timer を作る。
	Schedule string
}

// TimerUnit は定期実行の .timer を組み立てる。
func TimerUnit(a App, name string, w WorkloadSpec) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("[Unit]")
	p("Description=%s (%s) の定期実行", a.Name, name)
	p("")
	p("[Timer]")
	p("OnCalendar=%s", w.Schedule)
	// 停止中に予定時刻を過ぎていたら起動後に一度走らせる。
	p("Persistent=true")
	// 同じ時刻に複数のアプリが集中しないよう散らす。
	p("RandomizedDelaySec=15m")
	p("")
	p("[Install]")
	p("WantedBy=timers.target")
	return b.String()
}

// DBSpec はアプリ専用 PostgreSQL の生成に必要な情報。
type DBSpec struct {
	Image   string
	DataDir string
	SockDir string
	EnvFile string
	// Owner は初期化で作られる superuser。健康確認にも使う。
	Owner string
	// Args は postgres へ渡す追加の引数。資源を絞るのに使う。
	Args []string
}

// DBUnit はアプリ専用 PostgreSQL の .container を組み立てる。
//
// root 側の Quadlet として置く。アプリのユーザで動かすと、初期化に要る
// owner のパスワードをそのユーザが読めることになり、runtime が owner の
// 資格情報を持てないという分離が崩れる。
//
// ネットワークは持たせない。他から届く経路を無くし、繋げるのはソケットの
// ディレクトリを共有しているものだけにする。アプリのコンテナには自分の
// ソケットだけを見せるので、他アプリの DB へは到達しようがない。
func DBUnit(a App, w DBSpec) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("[Unit]")
	p("Description=%s の PostgreSQL", a.Name)
	p("")
	p("[Container]")
	p("Image=%s", w.Image)
	// 到達経路を持たせない。
	p("Network=none")
	// postgres として動かし、マウント元の所有者を podman に合わせさせる。
	//
	// 公式 image は /var/lib/postgresql が postgres 所有である前提で書かれて
	// いる。bind mount で root 所有のディレクトリを差し込むと、権限を落とした
	// 後の mkdir が Permission denied で落ちる。
	//
	// :U は podman がマウント元をコンテナのユーザへ chown する指定。uid を
	// こちらで決め打ちしないで済む (image が変われば変わりうるため)。
	p("User=postgres")
	p("Volume=%s:/var/lib/postgresql:U", w.DataDir)
	p("Volume=%s:/var/run/postgresql:U", w.SockDir)
	// POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB が入る。root しか
	// 読めない場所に置く。
	p("EnvironmentFile=%s", w.EnvFile)
	// 初期化のたびに同じ設定で立ち上がるよう、引数は unit に持たせる。
	// Exec はコマンドライン全体を 1 行で書く。
	if len(w.Args) > 0 {
		p("Exec=%s", strings.Join(w.Args, " "))
	}
	// 収束は「応答するまで待つ」で判定するので、ここは systemd 側の目安。
	//
	// ロールを明示する。省略すると pg_isready は postgres を名乗るが、
	// この構成では superuser の名前がアプリごとに違う。存在しないロールへの
	// 接続は毎回 FATAL としてログに残り、健康確認の transient unit も
	// 失敗扱いになる。
	p("HealthCmd=pg_isready -q -U %s", w.Owner)
	p("HealthInterval=10s")
	p("HealthRetries=5")
	p("HealthStartPeriod=60s")
	p("")
	p("[Service]")
	// 落ちたら上げ直す。アプリ側は DB が居ないと動かない。
	p("Restart=always")
	p("RestartSec=10")
	p("")
	p("[Install]")
	p("WantedBy=multi-user.target")
	return b.String()
}

// HAProxyContainer は HAProxy を動かすコンテナの名前。
//
// converge が設定を反映させるときに podman へ渡すので、生成側と一致させる。
const HAProxyContainer = "yunirun-haproxy"

// HAProxyUnit は HAProxy を動かす Quadlet unit を返す。
//
// nixpkgs の haproxy をそのまま systemd で動かしていたのをやめ、コンテナに
// 移した。配信経路に残る最後の「その distribution のパッケージ」だったので、
// これで yunirun の依存が podman と systemd と Quadlet だけになる。
func HAProxyUnit(image, configPath string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("[Unit]")
	p("Description=yunirun: HAProxy")
	p("")
	p("[Container]")
	p("ContainerName=%s", HAProxyContainer)
	p("Image=%s", image)
	// ホストのネットワークをそのまま使う。HAProxy は 127.0.0.1 の frontend を
	// 開き、同じ 127.0.0.1 のアプリへ繋ぐ。ここを分けると両側に穴を開けること
	// になり、隔離した意味が無い。
	p("Network=host")
	p("Volume=%s:%s:ro", configPath, configPath)
	// -W はマスター・ワーカー方式。設定の反映 (USR2) をマスターが受ける。
	// -db は前面で動かす指定で、これが無いとコンテナが即座に終了する。
	p("Exec=haproxy -W -db -f %s", configPath)
	p("")
	p("[Service]")
	p("Restart=always")
	p("RestartSec=5")
	p("")
	p("[Install]")
	p("WantedBy=multi-user.target")
	return b.String()
}
