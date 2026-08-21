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
	if !strings.Contains(got, "Volume=/run/postgresql:/run/postgresql") ||
		!strings.Contains(got, "Environment=PGHOST=/run/postgresql") {
		t.Fatalf("Unix ソケット経由になっていない:\n%s", got)
	}
	// TCP で繋ぐと、コンテナからホストの loopback 上の他サービスにも届く。
	if strings.Contains(got, "PGHOST=127.0.0.1") || strings.Contains(got, "PGHOST=localhost") {
		t.Fatalf("TCP で繋ごうとしている:\n%s", got)
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

func TestContainerUnitIsStableAcrossRuns(t *testing.T) {
	// 生成が非決定的だと、内容が同じでも converge のたびに unit が書き換わり
	// 無用な再起動を招く。map の反復順に引きずられないことを確かめる。
	a := sample()
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
