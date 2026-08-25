// Package config は、システム側 (NixOS) が置く /etc/yunirun/config.json を読む。
//
// ここに書くのは「どのリポジトリを取り込むか」という取り込みの意思決定だけ。
// これがそのまま opkssh の認可リストと対応し、セキュリティ境界になる。
// アプリの中身に関する設定は各リポジトリの yunirun.jsonc 側にある。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/yuniruyuni/yunirun/internal/alloc"
	"github.com/yuniruyuni/yunirun/internal/render"
)

// DefaultPath は NixOS モジュールが書き出す場所。
const DefaultPath = "/etc/yunirun/config.json"

// Config は yunirun 全体の設定。
type Config struct {
	// Domain は各アプリのホスト名を導出する元。<app>.<domain> になる。
	Domain string `json:"domain"`
	// StateDir は yunirun が持つ状態の置き場所。
	StateDir string `json:"stateDir"`
	// AdminRecipient は生成した秘密を暗号化する管理者の age 公開鍵。
	// ホスト鍵を失ったときの復旧経路として必ず含める。
	AdminRecipient string `json:"adminRecipient"`
	// HostKeyPath はホスト側の age 秘密鍵。
	HostKeyPath string `json:"hostKeyPath"`

	// SecretsKeyPath はアプリ側の秘密を復号する age 秘密鍵。
	//
	// HostKeyPath とは分ける。あちらは ssh のホスト鍵から導いているので、
	// ホストを作り直すと変わる。アプリのリポジトリにある暗号文は人が
	// 暗号化したもので、鍵が変わると全アプリで暗号化し直しになる。
	//
	// 対応する公開鍵は yunirun recipient で表示できる。アプリ側はそれに
	// 向けて暗号化する。
	SecretsKeyPath string `json:"secretsKeyPath"`
	// Apps はアプリ名からリポジトリ (owner/name) への対応。
	Apps map[string]string `json:"apps"`

	// HomesDir はアプリのホームを置く場所。空なら既定値。
	HomesDir string `json:"homesDir"`

	// DBDir はアプリ専用 PostgreSQL のデータとソケットを置く場所。空なら既定値。
	//
	// ホームとは分ける。ホームは rename や remove が捨てるので、そこに
	// データを置くと名前を変えただけで消える。
	DBDir string `json:"dbDir"`

	// DBImage はアプリ専用 PostgreSQL の image。空なら既定値。
	DBImage string `json:"dbImage"`

	// HAProxyImage は経路を担う HAProxy の image。空なら既定値。
	HAProxyImage_ string `json:"haproxyImage"`

	// Observability は計測基盤 (メトリクス・ログ・可視化) の設定。
	Observability Observability `json:"observability"`

	// EnvDir は unit が EnvironmentFile= で読む env ファイルの置き場所。
	// 空なら既定値。
	//
	// tmpfs ではなく永続領域に置く。ここが /run だったころ、再起動で消えた
	// env を unit が EnvironmentFile= (先頭の - 無し) で参照して起動に失敗して
	// いた。converge は同じ target に居るだけで順序関係が無く、しかもアプリ側
	// はユーザ unit なのでシステム unit を After= できない。順序を張るのでは
	// なく、依存そのものを消す。
	EnvDir string `json:"envDir"`

	// BasePort と BaseUID は割り当ての起点。既存の仕組みと並行して動かす間、
	// 帯を重ねないために外から指定できるようにしてある。0 なら既定値。
	BasePort int `json:"basePort"`
	BaseUID  int `json:"baseUID"`
}

var (
	appRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Load は path から設定を読む。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s を読めません: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Domain == "" {
		return fmt.Errorf("domain が空です")
	}
	if c.StateDir == "" {
		return fmt.Errorf("stateDir が空です")
	}
	for name, repo := range c.Apps {
		// アプリ名は Linux ユーザ名・DB 名・unit 名の元になる。ここを緩めると
		// それらすべてに任意の文字列が流れ込む。
		if !appRE.MatchString(name) {
			return fmt.Errorf("アプリ名に使えない文字が含まれています: %q", name)
		}
		if !repoRE.MatchString(repo) {
			return fmt.Errorf("%s のリポジトリ指定が不正です: %q", name, repo)
		}
	}
	return nil
}

// Names は宣言されたアプリ名を名前順で返す。
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Apps))
	for n := range c.Apps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Base は割り当ての起点を返す。
func (c *Config) Base() alloc.Base {
	b := alloc.DefaultBase()
	if c.BasePort != 0 {
		b.Port = c.BasePort
	}
	if c.BaseUID != 0 {
		b.UID = c.BaseUID
	}
	return b
}

// HomeDir はアプリのホームを置く場所を返す。
//
// stateDir の外に置く。stateDir は台帳と秘密のために root 専用にしておきたいが、
// パスの途中が辿れないと配下のホームへも届かないため。
// DatabaseDir は DB のデータとソケットを置く場所を返す。
func (c *Config) DatabaseDir() string {
	if c.DBDir != "" {
		return c.DBDir
	}
	return "/var/lib/yunirun-db"
}

// EnvironmentDir は unit が読む env ファイルの置き場所を返す。
//
// stateDir の中には置けない。stateDir は台帳と秘密のために root 専用だが、
// アプリの unit はユーザ unit なので、自分の runtime.env まで辿れる必要がある。
func (c *Config) EnvironmentDir() string {
	if c.EnvDir != "" {
		return c.EnvDir
	}
	return "/var/lib/yunirun-env"
}

// EnvPath はアプリの env ファイルの位置を返す。
func (c *Config) EnvPath(app, name string) string {
	return filepath.Join(c.EnvironmentDir(), app, name)
}

// Observability は計測基盤の設定。
//
// yunirun が管理する付帯コンテナとして立てる。アプリと違って deploy されない
// ので、unit も設定も converge が生成する。
type Observability struct {
	// Enable が false なら何も立てない。
	Enable bool `json:"enable"`
	// Dir はデータの置き場所。空なら既定値。
	Dir string `json:"dir"`
	// Retention は保持期間。空なら既定値。
	Retention string `json:"retention"`
	// AlertWebhook はアラートの送り先。空なら送り先を作らない。
	//
	// その先が Discord なのかメールなのかを yunirun は知らない。変えるときに
	// こちらを触らずに済むよう、webhook 1 つに寄せる。
	AlertWebhook string `json:"alertWebhook"`

	PrometheusImage string `json:"prometheusImage"`
	LokiImage       string `json:"lokiImage"`
	AlloyImage      string `json:"alloyImage"`
	GrafanaImage    string `json:"grafanaImage"`
	NodeImage       string `json:"nodeImage"`
	TempoImage      string `json:"tempoImage"`
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// TraceSocketDir はトレースの口の置き場所を返す。
//
// 設定ファイルの位置に依らないので、Spec を組まずに引けるようにしてある。
func (o Observability) TraceSocketDir() string {
	return o.Spec("").TraceSocketDir()
}

// Spec は既定値を埋めた組み立て指示を返す。
func (o Observability) Spec(confDir string) render.StackSpec {
	return render.StackSpec{
		Dir:     or(o.Dir, "/var/lib/yunirun-obs"),
		ConfDir: confDir,
		// タグを固定する。動くものが勝手に変わると、壊れたときに何が
		// 変わったのかを追えない。
		PrometheusImage: or(o.PrometheusImage, "docker.io/prom/prometheus:v3.14.0"),
		LokiImage:       or(o.LokiImage, "docker.io/grafana/loki:3.7.6"),
		AlloyImage:      or(o.AlloyImage, "docker.io/grafana/alloy:v1.19.0"),
		GrafanaImage:    or(o.GrafanaImage, "docker.io/grafana/grafana:13.2.0"),
		NodeImage:       or(o.NodeImage, "quay.io/prometheus/node-exporter:v1.12.1"),
		TempoImage:      or(o.TempoImage, "docker.io/grafana/tempo:3.0.0"),
		Retention:       or(o.Retention, "30d"),
		AlertWebhook:    o.AlertWebhook,
	}
}

// HAProxyImage は HAProxy に使う image を返す。
//
// Prometheus exporter を持つものが要る。公式 image は USE_PROMEX=1 で
// 組まれているので、そのまま使える。
func (c *Config) HAProxyImage() string {
	if c.HAProxyImage_ != "" {
		return c.HAProxyImage_
	}
	return "docker.io/library/haproxy:3.2-alpine"
}

// DatabaseImage は DB に使う image を返す。
func (c *Config) DatabaseImage() string {
	if c.DBImage != "" {
		return c.DBImage
	}
	return "docker.io/library/postgres:18-alpine"
}

func (c *Config) HomeDir() string {
	if c.HomesDir != "" {
		return c.HomesDir
	}
	return "/var/lib/yunirun-apps"
}

// LedgerPath は割り当て台帳の位置を返す。
func (c *Config) LedgerPath() string {
	return filepath.Join(c.StateDir, "allocations.json")
}

// Allocs は台帳に基づく割り当てを返す。
//
// 台帳が無いアプリには新しい番号を振るが、ここでは保存しない。保存は converge が
// 行う。deploy 側から呼んだときに台帳を書き換えてしまわないようにするため。
func (c *Config) Allocs() (map[string]alloc.Alloc, error) {
	l, err := alloc.LoadLedger(c.LedgerPath())
	if err != nil {
		return nil, err
	}
	return l.Ensure(c.Names(), c.Base()), nil
}

// Hostname はアプリの公開ホスト名を返す。
func (c *Config) Hostname(app string) string { return app + "." + c.Domain }
