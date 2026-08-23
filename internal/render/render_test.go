package render

import (
	"strings"
	"testing"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/manifest"
)

func sample() App {
	m, _ := manifest.Parse([]byte(`{}`))
	return App{
		Name:     "fighter",
		User:     "yunirun-fighter",
		Alloc:    alloc.Alloc{UID: 6000, Frontend: 8100, Blue: 8101, Green: 8102},
		Manifest: m,
		LocalTag: "localhost/fighter:current",
		DBName:   "fighter",
		DBUser:   "fighter_app",
		SockDir:  "/var/lib/yunirun-db/fighter/sock",
		EnvFile:  "/run/yunirun/fighter/runtime.env",
	}
}

func TestContainerUnitPublishesOnlyToLoopback(t *testing.T) {
	got := ContainerUnit(sample(), "blue")
	if !strings.Contains(got, "PublishPort=127.0.0.1:8101:3000") {
		t.Fatalf("loopback への publish になっていない:\n%s", got)
	}
	// 0.0.0.0 に開くと Cloudflare を迂回して直接叩けてしまう。
	if strings.Contains(got, "PublishPort=0.0.0.0") {
		t.Fatalf("外部に publish している:\n%s", got)
	}
}

func TestContainerUnitUsesUnixSocketForDatabase(t *testing.T) {
	got := ContainerUnit(sample(), "blue")
	if !strings.Contains(got, "Environment=PGHOST=/run/postgresql") {
		t.Fatalf("Unix ソケット経由になっていない:\n%s", got)
	}
	// TCP で繋ぐと、コンテナからホストの loopback 上の他サービスにも届く。
	if strings.Contains(got, "PGHOST=127.0.0.1") || strings.Contains(got, "PGHOST=localhost") {
		t.Fatalf("TCP で繋ごうとしている:\n%s", got)
	}
}

// 見せるのは自分専用 PostgreSQL のソケットだけ。共有ディレクトリを渡すと、
// パスワードを知っている相手が他アプリの DB へ繋げてしまう。
func TestContainerUnitMountsOnlyItsOwnSocketDirectory(t *testing.T) {
	got := ContainerUnit(sample(), "blue")
	if !strings.Contains(got, "Volume=/var/lib/yunirun-db/fighter/sock:/run/postgresql") {
		t.Fatalf("自分のソケットを見せていない:\n%s", got)
	}
	if strings.Contains(got, "Volume=/run/postgresql:/run/postgresql") {
		t.Fatalf("共有のソケットディレクトリを渡している:\n%s", got)
	}
}

// DB は到達経路を持たない。ネットワークを与えると、同じホスト上の他の
// コンテナから届きうる。
func TestDBUnitHasNoNetworkAndKeepsSecretsInAFile(t *testing.T) {
	got := DBUnit(sample(), DBSpec{
		Image:   "docker.io/library/postgres:18-alpine",
		DataDir: "/var/lib/yunirun-db/fighter/data",
		SockDir: "/var/lib/yunirun-db/fighter/sock",
		EnvFile: "/run/yunirun/fighter/db.env",
		Args:    []string{"-c", "shared_buffers=32MB"},
	})
	if !strings.Contains(got, "Network=none") {
		t.Fatalf("到達経路を持たせている:\n%s", got)
	}
	if strings.Contains(got, "PublishPort") {
		t.Fatalf("ポートを公開している:\n%s", got)
	}
	if !strings.Contains(got, "EnvironmentFile=/run/yunirun/fighter/db.env") {
		t.Fatalf("EnvironmentFile が無い:\n%s", got)
	}
	// unit ファイルは平文で置かれる。値が出てはいけない。
	if strings.Contains(got, "POSTGRES_PASSWORD=") {
		t.Fatalf("パスワードを直接埋め込んでいる:\n%s", got)
	}
}

func TestContainerUnitReferencesEnvFileWithoutEmbeddingValues(t *testing.T) {
	got := ContainerUnit(sample(), "blue")
	// unit にはパスしか現れない。値が出ると、unit を読める相手に秘密が渡る。
	if !strings.Contains(got, "EnvironmentFile=/run/yunirun/fighter/runtime.env") {
		t.Fatalf("EnvironmentFile が無い:\n%s", got)
	}
	if strings.Contains(got, "DB_PASSWORD=") {
		t.Fatalf("値を直接埋め込んでいる:\n%s", got)
	}
}

func TestContainerUnitOmitsDatabaseWhenUnused(t *testing.T) {
	a := sample()
	a.DBName, a.DBUser = "", ""
	got := ContainerUnit(a, "blue")
	if strings.Contains(got, "postgresql") || strings.Contains(got, "DB_USER") {
		t.Fatalf("DB を使わないのに DB 設定が出ている:\n%s", got)
	}
}

func TestContainerUnitIncludesDeclaredEnv(t *testing.T) {
	a := sample()
	m, _ := manifest.Parse([]byte(`{"app":{"env":{"APP_URL":"https://x.example","B":"2"}}}`))
	a.Manifest = m
	got := ContainerUnit(a, "blue")
	if !strings.Contains(got, "Environment=APP_URL=https://x.example") {
		t.Fatalf("宣言した env が出ていない:\n%s", got)
	}
}

// 秘密は EnvironmentFile 経由で渡す。unit に値が出ると、unit を読める相手に
// 秘密が渡る。
func TestContainerUnitKeepsSecretsOutOfTheUnitFile(t *testing.T) {
	a := sample()
	m, _ := manifest.Parse([]byte(`{"app":{"secrets":{"TOKEN":"some-secret"}}}`))
	a.Manifest = m
	got := ContainerUnit(a, "blue")
	// 秘密の名前すら unit には出さない (値は当然出さない)。
	if strings.Contains(got, "TOKEN=") {
		t.Fatalf("秘密が unit に現れている:\n%s", got)
	}
}

func TestContainerUnitIsStableAcrossRuns(t *testing.T) {
	// 生成が非決定的だと、内容が同じでも converge のたびに unit が書き換わり
	// 無用な再起動を招く。map の反復順に引きずられないことを確かめる。
	a := sample()
	// 反復順に引きずられないことを確かめるため、複数の env を入れる。
	m, _ := manifest.Parse([]byte(`{"app":{"env":{"C":"3","A":"1","B":"2"}}}`))
	a.Manifest = m
	first := ContainerUnit(a, "blue")
	for i := 0; i < 20; i++ {
		if ContainerUnit(a, "blue") != first {
			t.Fatal("生成結果が実行ごとに変わる")
		}
	}
}

func TestHAProxySendsHostHeaderOnHealthCheck(t *testing.T) {
	got := HAProxy([]App{sample()})
	// Host を送らないと、Host で振り分けるアプリが 404 を返して DOWN のままになる。
	if !strings.Contains(got, "http-check send meth GET uri /health ver HTTP/1.1 hdr Host localhost") {
		t.Fatalf("Host ヘッダを送っていない:\n%s", got)
	}
}

func TestHAProxyOrdersAppsDeterministically(t *testing.T) {
	a, b := sample(), sample()
	b.Name = "costume"
	x := HAProxy([]App{a, b})
	y := HAProxy([]App{b, a})
	if x != y {
		t.Fatal("入力順で設定が変わる")
	}
	if strings.Index(x, "frontend costume") > strings.Index(x, "frontend fighter") {
		t.Fatal("名前順になっていない")
	}
}

// 同名にすると haproxy 3.3 で動かなくなる。
func TestHAProxyGivesFrontendAndBackendDistinctNames(t *testing.T) {
	got := HAProxy([]App{sample()})
	if !strings.Contains(got, "frontend fighter_in") || !strings.Contains(got, "backend fighter_out") {
		t.Fatalf("名前が分かれていない:\n%s", got)
	}
	if strings.Contains(got, "frontend fighter\n") {
		t.Fatalf("frontend が backend と同名:\n%s", got)
	}
}

func TestWorkloadUnitIsOneshotNotRestarting(t *testing.T) {
	got := WorkloadUnit(sample(), "cleanup", WorkloadSpec{Image: "x", DBUser: "fighter_app"})
	if !strings.Contains(got, "Type=oneshot") {
		t.Fatalf("oneshot になっていない:\n%s", got)
	}
	// 一度だけ走るものを Restart=always にすると回り続ける。
	if strings.Contains(got, "Restart=always") {
		t.Fatalf("再起動し続ける設定になっている:\n%s", got)
	}
}

func TestWorkloadUnitUsesGivenRoleNotAlwaysOwner(t *testing.T) {
	got := WorkloadUnit(sample(), "cleanup", WorkloadSpec{Image: "x", DBUser: "fighter_app"})
	// cleanup が owner で繋ぐと、日次で動くものに DDL 権限が付く。
	if !strings.Contains(got, "Environment=DB_USER=fighter_app") {
		t.Fatalf("app ロールで繋いでいない:\n%s", got)
	}
}

func TestTimerUnitSchedulesAndCatchesUp(t *testing.T) {
	got := TimerUnit(sample(), "cleanup", WorkloadSpec{Schedule: "*-*-* 02:23:00"})
	if !strings.Contains(got, "OnCalendar=*-*-* 02:23:00") {
		t.Fatalf("スケジュールが入っていない:\n%s", got)
	}
	// 停止中に時刻を過ぎた分を落とさない。
	if !strings.Contains(got, "Persistent=true") {
		t.Fatalf("Persistent が無い:\n%s", got)
	}
}

// listener が 1 つも無いと haproxy は "no listener" で exit(2) する。
// アプリを一時的に全部外したときに haproxy が落ちたままにならないようにする。
func TestHAProxyStillHasAListenerWithNoApps(t *testing.T) {
	got := HAProxy(nil)
	if !strings.Contains(got, "frontend yunirun_placeholder") {
		t.Fatalf("待機用 listener が無い:\n%s", got)
	}
	if !strings.Contains(got, "bind 127.0.0.1:8099") {
		t.Fatalf("bind が無い:\n%s", got)
	}
}

func TestHAProxyPlaceholderDisappearsOnceAppsExist(t *testing.T) {
	got := HAProxy([]App{sample()})
	// アプリがあるのに待機用が残ると、余計なポートを占有し続ける。
	if strings.Contains(got, "yunirun_placeholder") {
		t.Fatalf("待機用 listener が残っている:\n%s", got)
	}
}

// ワークロードの多くはアプリ本体と同じバイナリを別の入口で動かすもので、
// 保持期間やバッチ幅といった設定を本体と共有する。渡さないと既定値で動き、
// しかも黙って動くので気付けない。
func TestWorkloadUnitCarriesTheAppEnvironment(t *testing.T) {
	a := App{
		Name:   "fighter",
		DBName: "fighter",
		Manifest: &manifest.Manifest{App: manifest.App{
			Env: map[string]string{
				"SHARE_RETENTION_DAYS": "30",
				"PGUSER":               "fighter_app",
			},
		}},
	}
	out := WorkloadUnit(a, "cleanup", WorkloadSpec{
		Image: "localhost/fighter:current", DBUser: "fighter_app",
	})
	for _, want := range []string{
		"Environment=PGUSER=fighter_app",
		"Environment=SHARE_RETENTION_DAYS=30",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q が無い:\n%s", want, out)
		}
	}
}

// 秘密はここに現れてはいけない。unit ファイルは平文で置かれる。
func TestWorkloadUnitKeepsSecretsInTheEnvFile(t *testing.T) {
	a := App{
		Name:   "fighter",
		DBName: "fighter",
		Manifest: &manifest.Manifest{App: manifest.App{
			Secrets: map[string]string{"TOKEN": "fighter-token"},
		}},
	}
	out := WorkloadUnit(a, "cleanup", WorkloadSpec{
		Image: "x", DBUser: "fighter_app", EnvFile: "/run/yunirun/fighter/runtime.env",
	})
	if strings.Contains(out, "TOKEN") {
		t.Fatalf("秘密の名前が unit に出ている:\n%s", out)
	}
	if !strings.Contains(out, "EnvironmentFile=/run/yunirun/fighter/runtime.env") {
		t.Fatalf("EnvironmentFile が無い:\n%s", out)
	}
}

// 同じバイナリでも入口が違えば適切な値が違う。fighter の cleanup は接続
// プールを 1 に絞り、文が長いので statement timeout を伸ばす。
func TestWorkloadEnvOverridesTheAppEnv(t *testing.T) {
	a := App{
		Name:   "fighter",
		DBName: "fighter",
		Manifest: &manifest.Manifest{App: manifest.App{
			Env: map[string]string{"PGPOOL_MAX": "5", "SHARE_RETENTION_DAYS": "30"},
		}},
	}
	out := WorkloadUnit(a, "cleanup", WorkloadSpec{
		Image: "x", DBUser: "fighter_app",
		Env: map[string]string{"PGPOOL_MAX": "1", "CLEANUP_BATCH_SIZE": "500"},
	})
	// systemd は同じ名前が複数あれば後のものを採る。順序が逆になっていないか。
	app := strings.Index(out, "Environment=PGPOOL_MAX=5")
	own := strings.Index(out, "Environment=PGPOOL_MAX=1")
	if app < 0 || own < 0 {
		t.Fatalf("両方の宣言が要る:\n%s", out)
	}
	if own < app {
		t.Fatalf("ワークロード固有の値が先に来ている。上書きにならない:\n%s", out)
	}
	for _, want := range []string{
		"Environment=SHARE_RETENTION_DAYS=30",
		"Environment=CLEANUP_BATCH_SIZE=500",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q が無い:\n%s", want, out)
		}
	}
}

// Quadlet の Exec はコマンドライン全体を 1 行で書く。引数ごとに 1 行に
// すると最後の行だけが効き、残りが落ちる。引数が 1 つのうちは偶然動くので
// 気付きにくい。実際 postgres が
//
//	invalid argument: "autovacuum_max_workers=1"
//
// で起動できなかった。
func TestExecIsOneLineForTheWholeCommand(t *testing.T) {
	a := App{Name: "fighter", Manifest: &manifest.Manifest{}}
	for _, got := range []string{
		WorkloadUnit(a, "cleanup", WorkloadSpec{
			Image: "x", Args: []string{"--batch=cleanup", "--verbose"},
		}),
		DBUnit(a, DBSpec{
			Image: "x", Args: []string{"-c", "shared_buffers=32MB", "-c", "max_connections=20"},
		}),
	} {
		n := strings.Count(got, "\nExec=")
		if n != 1 {
			t.Fatalf("Exec が %d 行ある。1 行でなければ引数が落ちる:\n%s", n, got)
		}
	}
	if !strings.Contains(DBUnit(a, DBSpec{Image: "x",
		Args: []string{"-c", "shared_buffers=32MB"}}), "Exec=-c shared_buffers=32MB") {
		t.Fatal("引数が繋がっていない")
	}
}
