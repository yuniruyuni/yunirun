package render

// Grafana のダッシュボード。converge が生成する。
//
// UI で作ったものは Grafana の DB にしか残らず、作り直すと消える。宣言から
// 生成しておけば、ホストを作り直しても同じものが出てくる。

import (
	"encoding/json"
	"fmt"
)

// ds は Prometheus のデータソース指定。
var ds = map[string]any{"type": "prometheus", "uid": PrometheusUID}

// panelSpec は 1 枚のグラフ。
type panelSpec struct {
	Title string
	// Queries は式と凡例の組。1 枚に複数重ねられる。
	Queries []query
	// Unit は Grafana の単位名 (s, reqps, percentunit, bytes, binBps など)。
	Unit string
	// Desc は「何を見ているのか」の説明。読む人が式を読まずに済むように。
	Desc string
	// Stat が真なら数値 1 つで出す。時系列ではなく現在値を見たいもの。
	Stat bool
	// Steps は色を変える閾値 (値, 色)。Stat のときだけ効く。
	Steps []step
}

type query struct{ Expr, Legend string }
type step struct {
	Value *float64
	Color string
}

func f(v float64) *float64 { return &v }

// panelJSON は 1 枚を Grafana の形にする。
func panelJSON(p panelSpec, x, y, w, h int) map[string]any {
	targets := make([]any, 0, len(p.Queries))
	for i, q := range p.Queries {
		targets = append(targets, map[string]any{
			"refId":        string(rune('A' + i)),
			"expr":         q.Expr,
			"legendFormat": q.Legend,
			"datasource":   ds,
			"editorMode":   "code",
			"range":        !p.Stat,
			"instant":      p.Stat,
		})
	}

	defaults := map[string]any{
		"unit":     p.Unit,
		"decimals": 3,
	}
	if len(p.Steps) > 0 {
		steps := make([]any, 0, len(p.Steps))
		for _, s := range p.Steps {
			var v any
			if s.Value != nil {
				v = *s.Value
			}
			steps = append(steps, map[string]any{"value": v, "color": s.Color})
		}
		defaults["thresholds"] = map[string]any{"mode": "absolute", "steps": steps}
		defaults["color"] = map[string]any{"mode": "thresholds"}
	}

	typ := "timeseries"
	options := map[string]any{
		"legend": map[string]any{
			"displayMode": "table",
			"placement":   "bottom",
			// 「いまいくつか」と「この範囲での最大」が要る。前者は現状、
			// 後者は見ていない間に何があったかを示す。
			"calcs": []any{"lastNotNull", "max"},
		},
		"tooltip": map[string]any{"mode": "multi", "sort": "desc"},
	}
	if p.Stat {
		typ = "stat"
		options = map[string]any{
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}},
			"textMode":      "value_and_name",
			"colorMode":     "background",
		}
	}

	return map[string]any{
		"type":        typ,
		"title":       p.Title,
		"description": p.Desc,
		"datasource":  ds,
		"gridPos":     map[string]any{"x": x, "y": y, "w": w, "h": h},
		"fieldConfig": map[string]any{"defaults": defaults, "overrides": []any{}},
		"options":     options,
		"targets":     targets,
	}
}

// dashboardJSON は 2 列に並べて 1 枚の板にする。
func dashboardJSON(uid, title, desc string, panels []panelSpec) string {
	out := make([]any, 0, len(panels))
	x, y := 0, 0
	for _, p := range panels {
		w, h := 12, 8
		if p.Stat {
			w, h = 24, 4
		}
		if x+w > 24 {
			x, y = 0, y+h
		}
		out = append(out, panelJSON(p, x, y, w, h))
		x += w
		if x >= 24 {
			x, y = 0, y+h
		}
	}

	b, err := json.MarshalIndent(map[string]any{
		"uid":           uid,
		"title":         title,
		"description":   desc,
		"tags":          []any{"yunirun"},
		"timezone":      "browser",
		"schemaVersion": 39,
		"refresh":       "30s",
		"time":          map[string]any{"from": "now-6h", "to": "now"},
		// 宣言から作っているので UI では編集させない。編集しても次の収束で
		// 戻り、直したつもりが消える。
		"editable": false,
		"panels":   out,
	}, "", "  ")
	if err != nil {
		// 中身は全部こちらで組み立てているので、ここは通らない。
		panic(fmt.Sprintf("ダッシュボードを組み立てられません: %v", err))
	}
	return string(b) + "\n"
}

// REDDashboard はアプリごとの応答を見る板。
//
// Rate (流量)・Errors (失敗)・Duration (所要時間) の 3 つ。出どころは HAProxy の
// exporter だけで、全アプリの経路がそこを通るのでアプリ側の計装が要らない。
//
// 注意が 2 つある。健康確認は要求数に数えられない (実測で確認済み)。所要時間は
// 直近 1024 件の平均であって時間窓の平均ではないので、パーセンタイルは出せない。
func REDDashboard() string {
	const req = `haproxy_backend_http_responses_total`
	return dashboardJSON("yunirun-red", "RED — アプリの応答",
		"Rate・Errors・Duration。出どころは HAProxy の exporter。",
		[]panelSpec{
			{
				Title: "生きている系", Stat: true, Unit: "short",
				Desc: "アプリごとに何本の複製が応答しているか。0 になると外から到達できない。",
				Queries: []query{{
					Expr:   `sum by (proxy) (haproxy_server_status{state="UP"})`,
					Legend: "{{proxy}}",
				}},
				Steps: []step{{Value: nil, Color: "red"}, {Value: f(1), Color: "orange"}, {Value: f(2), Color: "green"}},
			},
			{
				Title: "Rate — 応答毎秒", Unit: "reqps",
				Desc: "健康確認は含まれない。純粋に外から来た要求への応答。",
				Queries: []query{{
					Expr:   `sum by (proxy) (rate(` + req + `[5m]))`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "Errors — 5xx 毎秒", Unit: "reqps",
				Desc: "アプリ側の失敗。0 でないときは中で何かが起きている。",
				Queries: []query{{
					Expr:   `sum by (proxy) (rate(` + req + `{code="5xx"}[5m]))`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "Errors — 失敗の割合", Unit: "percentunit",
				Desc: "5xx ÷ 全応答。流量が少ないときに割合が跳ねないよう、分母に下限を置いてある。",
				Queries: []query{{
					Expr: `sum by (proxy) (rate(` + req + `{code="5xx"}[5m]))` +
						` / clamp_min(sum by (proxy) (rate(` + req + `[5m])), 0.001)`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "Duration — 応答時間の平均", Unit: "s",
				Desc: "直近 1024 件の平均。時間窓の平均ではないので、急な悪化は均されて見える。",
				Queries: []query{{
					Expr:   `haproxy_backend_response_time_average_seconds`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "Duration — 応答時間の最大", Unit: "s",
				Desc: "平均だけだと長い尾が見えない。exporter はパーセンタイルを出さないので最大で代える。",
				Queries: []query{{
					Expr:   `max by (proxy) (haproxy_server_max_response_time_seconds)`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "4xx 毎秒", Unit: "reqps",
				Desc: "呼ぶ側の誤り。増えたときはアプリの失敗とは限らない。",
				Queries: []query{{
					Expr:   `sum by (proxy) (rate(` + req + `{code="4xx"}[5m]))`,
					Legend: "{{proxy}}",
				}},
			},
			{
				Title: "処理中のセッション", Unit: "short",
				Desc: "詰まっていると増える。HAProxy から見たアプリの飽和。",
				Queries: []query{
					{Expr: `haproxy_backend_current_sessions`, Legend: "{{proxy}} 処理中"},
					{Expr: `haproxy_backend_current_queue`, Legend: "{{proxy}} 待ち"},
				},
			},
		})
}

// USEDashboard はホストの資源を見る板。
//
// 資源ごとに Utilization (使用率)・Saturation (飽和)・Errors (誤り) を並べる。
// 飽和には PSI (/proc/pressure) を使う。使用率が 100% でなくても待たされて
// いることがあり、平均だけでは掴めないため。
func USEDashboard() string {
	return dashboardJSON("yunirun-use", "USE — ホストの資源",
		"資源ごとの使用率・飽和・誤り。出どころは node exporter。",
		[]panelSpec{
			{
				Title: "CPU — 使用率", Unit: "percentunit",
				Desc: "idle 以外の割合。1.0 で全コアが埋まっている。",
				Queries: []query{{
					Expr:   `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m]))`,
					Legend: "使用率",
				}},
			},
			{
				Title: "CPU — 飽和 (待たされた割合)", Unit: "percentunit",
				Desc: "PSI。使用率に余裕があっても、ここが上がっていれば待ちが発生している。",
				Queries: []query{
					{Expr: `rate(node_pressure_cpu_waiting_seconds_total[5m])`, Legend: "待ち"},
				},
			},
			{
				Title: "メモリ — 使用率", Unit: "percentunit",
				Desc: "available を基準にする。キャッシュは空きとして扱われる。",
				Queries: []query{{
					Expr:   `1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes`,
					Legend: "使用率",
				}},
			},
			{
				Title: "メモリ — 飽和", Unit: "percentunit",
				Desc: "PSI。回収のために待たされている割合。上がり始めたら空きが尽きかけている。",
				Queries: []query{
					{Expr: `rate(node_pressure_memory_waiting_seconds_total[5m])`, Legend: "待ち"},
				},
			},
			{
				Title: "ディスク — 使用率 (入出力に費やした割合)", Unit: "percentunit",
				Desc: "1.0 に貼り付くとその装置が上限。",
				Queries: []query{{
					Expr:   `rate(node_disk_io_time_seconds_total[5m])`,
					Legend: "{{device}}",
				}},
			},
			{
				Title: "ディスク — 飽和", Unit: "percentunit",
				Desc: "PSI。入出力の完了を待っている割合。",
				Queries: []query{
					{Expr: `rate(node_pressure_io_waiting_seconds_total[5m])`, Legend: "待ち"},
				},
			},
			{
				Title: "ファイルシステム — 使用率", Unit: "percentunit",
				Desc: "一時領域や重ね合わせは除いてある。",
				Queries: []query{{
					Expr: `1 - node_filesystem_avail_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}` +
						` / node_filesystem_size_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}`,
					Legend: "{{mountpoint}}",
				}},
			},
			{
				Title: "ネットワーク — 流量", Unit: "binBps",
				Desc: "loopback は除いてある。",
				Queries: []query{
					{Expr: `rate(node_network_receive_bytes_total{device!="lo"}[5m])`, Legend: "{{device}} 受信"},
					{Expr: `rate(node_network_transmit_bytes_total{device!="lo"}[5m])`, Legend: "{{device}} 送信"},
				},
			},
			{
				Title: "ネットワーク — 誤り", Unit: "pps",
				Desc: "0 でないときは回線か機器の問題。",
				Queries: []query{
					{Expr: `rate(node_network_receive_errs_total{device!="lo"}[5m])`, Legend: "{{device}} 受信"},
					{Expr: `rate(node_network_transmit_errs_total{device!="lo"}[5m])`, Legend: "{{device}} 送信"},
				},
			},
			{
				Title: "アプリごとの CPU", Unit: "percentunit",
				Desc: "どれがコアをどれだけ使っているか。ホスト全体の使用率では、" +
					"どのアプリが食っているかが分からない。",
				Queries: []query{{
					Expr:   `rate(yunirun_unit_cpu_seconds_total[5m])`,
					Legend: "{{unit}}",
				}},
			},
			{
				Title: "アプリごとのメモリ", Unit: "bytes",
				Desc: "unit ごとの実使用量。",
				Queries: []query{{
					Expr:   `yunirun_unit_memory_bytes`,
					Legend: "{{unit}}",
				}},
			},
			{
				Title: "ネットワーク — 飽和 (取りこぼし)", Unit: "pps",
				Desc: "捌ききれずに落とした数。",
				Queries: []query{
					{Expr: `rate(node_network_receive_drop_total{device!="lo"}[5m])`, Legend: "{{device}} 受信"},
					{Expr: `rate(node_network_transmit_drop_total{device!="lo"}[5m])`, Legend: "{{device}} 送信"},
				},
			},
		})
}
