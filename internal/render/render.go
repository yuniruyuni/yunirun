// Package render は、設定とマニフェストから配置物を組み立てる。
//
// 副作用を持たない。ここで作った文字列を書き出すのは呼び出し側の仕事にして、
// 生成規則そのものをテストで固定できるようにしてある。
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/manifest"
)

// Colors は blue/green の名前。順序が入れ替え順になる。
var Colors = []string{"blue", "green"}

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
	// EnvFile は秘密を含む環境変数ファイルの位置。tmpfs 上に置き、読めるのは
	// そのワークロードのユーザだけにする。
	//
	// unit ファイルにはパスしか現れないので、値がディスクや世界読み取り可能な
	// 場所へ漏れることがない。runtime の EnvFile には app パスワードしか
	// 入れず、owner パスワードは root しか読めない別のファイルに置く。
	// これで「runtime は owner パスワードを持てない」が構造として成立する。
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

	if a.DBName != "" {
		// PostgreSQL へは TCP ではなく Unix ソケットで繋ぐ。コンテナは独立した
		// netns にいるので、これによりホストの loopback 上の他サービスへは
		// 到達できないまま DB にだけ届く。
		p("Volume=/run/postgresql:/run/postgresql")
		p("Environment=PGHOST=/run/postgresql")
		p("Environment=PGPORT=5432")
		p("Environment=DB_USER=%s", a.DBUser)
		p("Environment=DB_NAME=%s", a.DBName)
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
	p("  log /dev/log local0")
	p("  maxconn 2048")
	p("")
	p("defaults")
	p("  mode http")
	p("  log global")
	p("  option httplog")
	p("  timeout connect 5s")
	p("  timeout client  60s")
	p("  timeout server  60s")

	sorted := append([]App(nil), apps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, a := range sorted {
		p("")
		p("frontend %s", a.Name)
		p("  bind 127.0.0.1:%d", a.Alloc.Frontend)
		p("  default_backend %s", a.Name)
		p("")
		p("backend %s", a.Name)
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
	if a.DBName != "" {
		p("Volume=/run/postgresql:/run/postgresql")
		p("Environment=PGHOST=/run/postgresql")
		p("Environment=PGPORT=5432")
		p("Environment=DB_USER=%s", w.DBUser)
		p("Environment=DB_NAME=%s", a.DBName)
		p("Environment=DB_APP_NAME=%s", a.DBName)
	}
	if w.EnvFile != "" {
		p("EnvironmentFile=%s", w.EnvFile)
	}
	for _, arg := range w.Args {
		p("Exec=%s", arg)
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
