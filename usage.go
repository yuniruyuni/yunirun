package main

// アプリごとの資源使用を Prometheus の形で出す。
//
// 追加のコンテナを置かない。必要なものは cgroup に既に揃っており、それを
// 読んで並べるだけで足りる。cAdvisor は設定も資源消費も大きく、この規模に
// 見合わない。

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/render"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// target は 1 つの計測対象。
type target struct {
	// App は所属するアプリ。DB や経路など、アプリに属さないものは空。
	App string
	// Kind は何の unit か (app / db / platform)。
	Kind string
	// Unit は unit の名前。
	Unit string
	// Dir は cgroup の位置。
	Dir string
}

// targets は測る対象を並べる。
//
// 台帳を基準にする。宣言に無いものまで測ると、消したはずのアプリが
// いつまでも出てくる。
func targets(cfg *config.Config, allocs map[string]alloc.Alloc) []target {
	var out []target
	for _, name := range cfg.Names() {
		a, ok := allocs[name]
		if !ok {
			continue
		}
		for _, color := range render.Colors {
			unit := fmt.Sprintf("%s-%s.service", name, color)
			out = append(out, target{
				App: name, Kind: "app", Unit: unit,
				Dir: system.UserUnitPath(a.UID, unit),
			})
		}
		// DB は root 側。アプリごとに独立したコンテナ。
		unit := name + "-db.service"
		out = append(out, target{
			App: name, Kind: "db", Unit: unit,
			Dir: system.SystemUnitPath(unit),
		})
	}
	// 経路と計測基盤。アプリには属さないが、資源は食う。
	for _, unit := range []string{
		"yunirun-haproxy.service", "yunirun-prometheus.service",
		"yunirun-loki.service", "yunirun-alloy.service",
		"yunirun-grafana.service", "yunirun-tempo.service",
		"yunirun-node.service",
	} {
		out = append(out, target{
			Kind: "platform", Unit: unit, Dir: system.SystemUnitPath(unit),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit < out[j].Unit })
	return out
}

// renderUsage は Prometheus の形に整える。
func renderUsage(ts []target) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }

	p("# HELP yunirun_unit_cpu_seconds_total unit が使った CPU 時間の累計")
	p("# TYPE yunirun_unit_cpu_seconds_total counter")
	var mem []string
	for _, t := range ts {
		u, ok := system.ReadUsage(t.Dir)
		if !ok {
			// 止まっている unit には cgroup が無い。0 を出すと「動いていて
			// 使っていない」と区別できないので、何も出さない。
			continue
		}
		labels := fmt.Sprintf("app=%q,kind=%q,unit=%q", t.App, t.Kind, t.Unit)
		p("yunirun_unit_cpu_seconds_total{%s} %.6f", labels, float64(u.CPUMicros)/1e6)
		mem = append(mem, fmt.Sprintf("yunirun_unit_memory_bytes{%s} %d", labels, u.MemoryBytes))
	}
	p("# HELP yunirun_unit_memory_bytes unit がいま使っているメモリ")
	p("# TYPE yunirun_unit_memory_bytes gauge")
	for _, line := range mem {
		p("%s", line)
	}
	return b.String()
}

// runUsage は資源使用を出す口を開く。
//
// 127.0.0.1 にだけ bind する。中身に秘密は無いが、外へ開く理由も無い。
func runUsage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "設定ファイル")
	addr := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", render.UsagePort), "待ち受け")
	once := fs.Bool("once", false, "一度だけ出して終える")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	body := func() (string, error) {
		allocs, err := cfg.Allocs()
		if err != nil {
			return "", err
		}
		return renderUsage(targets(cfg, allocs)), nil
	}

	if *once {
		s, err := body()
		if err != nil {
			return err
		}
		fmt.Print(s)
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		s, err := body()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(s))
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "yunirun usage: %s で待ち受けます\n", *addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
