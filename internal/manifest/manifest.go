// Package manifest は、アプリリポジトリが置く yunirun.jsonc を読む。
//
// このファイルに書くのは「アプリだけが知っていること」に限る。ホスト側の
// ポートや uid、DB 名やロール名は yunirun が名前から導出するので現れない。
// 多くのアプリは既定値で足りるため、ファイル自体が存在しないのが普通。
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/tidwall/jsonc"
)

// DefaultPort と DefaultHealth は、宣言が無いときに使う規約。
const (
	DefaultPort   = 3000
	DefaultHealth = "/health"
)

// Manifest は yunirun.jsonc の内容。
type Manifest struct {
	App       App                 `json:"app"`
	Workloads map[string]Workload `json:"workloads"`
}

// App はアプリ本体の性質。
type App struct {
	// Image は GHCR 上の image 名。省略するとアプリ名を使う。
	//
	// アプリ名と image 名が食い違う場合に指定する。yuniruyuni.net 側で付ける
	// 名前 (DB やユーザの名前になる) と、リポジトリが push する image 名は
	// 本来別のものなので、後者はアプリ側が宣言する。
	Image string `json:"image"`

	// Port はコンテナ内で listen するポート。yunirun は PORT 環境変数でも
	// 同じ値を渡すので、素直なアプリは宣言しなくてよい。nginx のように
	// PORT を見ないものだけが書く。
	Port int `json:"port"`
	// Health は HAProxy が叩くパス。
	Health string `json:"health"`

	// Env は秘密でない環境変数。
	//
	// OAuth の client_id のように仕様上公開される値や、アプリ自身の URL など。
	// unit ファイルへそのまま書き出されるので、秘密はここに置かない。
	// 秘密はリポジトリの secrets/<ENV_NAME>.age に暗号文として置く。
	Env map[string]string `json:"env"`

	// DatabaseName は使う DB の名前。空ならアプリ名を使う。
	//
	// 既存の DB を引き継ぐ場合に指定する。yuniruyuni.net 側で付けるアプリ名と、
	// 既に動いている DB の名前が食い違うことがあるため。名前が揃っていれば
	// 書かなくてよい。
	DatabaseName string `json:"databaseName"`

	// Database はこのアプリが PostgreSQL を使うか。
	//
	// 使わないアプリに DB とロールを作ると、消し忘れた資源が溜まるうえ
	// 不要な資格情報が生成される。既定は false で、必要なアプリだけが宣言する。
	Database bool `json:"database"`
}

// Workload はアプリ本体以外の実行単位。
//
// migration と cleanup を同じ形で扱う。両者の違いは「デプロイのたびに 1 回
// 走る」か「スケジュールで走る」かだけで、どちらもアプリ本体とは別の権限で
// 動く点は共通しているため。
type Workload struct {
	// Image を省略するとアプリ本体と同じ image を使う。fighter の cleanup が
	// これで、引数だけを変えて起動する。
	Image string `json:"image"`
	// Args は entrypoint に渡す引数。
	Args []string `json:"args"`
	// Schedule があれば systemd timer として登録する。無ければデプロイ時に
	// 一度だけ実行する。形式は systemd の OnCalendar。
	Schedule string `json:"schedule"`
	// Role は接続する DB ロール。owner は DDL、app は DML のみ。
	// migration だけが owner を要求する。
	Role string `json:"role"`
	// Env はこのワークロードだけの環境変数。app.env に同じ名前があれば
	// こちらが勝つ。同じバイナリでも入口が違えば適切な値が違うため
	// (fighter の cleanup は接続プールを 1 に絞り、文が長いので
	// statement timeout を伸ばす)。
	Env map[string]string `json:"env"`
}

// 名前に使える文字を絞る。ここを緩めると unit 名やファイルパスに任意の文字列が
// 流れ込むため、アプリ側から渡る値としては最も危険な部類になる。
var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envRE    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	secretRE = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
	dbNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Load は path から読む。ファイルが無い場合は既定値だけの Manifest を返す。
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return withDefaults(&Manifest{}), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse は JSONC のバイト列を読む。
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	// jsonc.ToJSON はコメントと末尾カンマを空白へ潰す。文字列リテラルの
	// 中身は保持されるので https:// を壊さない。
	if err := json.Unmarshal(jsonc.ToJSON(b), &m); err != nil {
		return nil, fmt.Errorf("yunirun.jsonc を読めません: %w", err)
	}
	if err := rejectRemoved(jsonc.ToJSON(b)); err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return withDefaults(&m), nil
}

// removed は受け付けなくなった宣言と、その代わり。
//
// encoding/json は知らない鍵を黙って捨てる。捨てたままにすると、秘密を
// 宣言しているつもりのアプリが、環境変数の無い状態で起動してしまう。
// 認証がなぜか失敗するという形でしか現れないので、ここで断る。
var removed = map[string]string{
	"secrets": "app.secrets は廃止しました。" +
		"秘密はリポジトリの secrets/<ENV_NAME>.age に暗号文として置いてください " +
		"(宛先は yunirun recipient で表示できます)",
	"databasePasswords": "app.databasePasswords は廃止しました。" +
		"DB パスワードは yunirun が生成して管理します",
	// 経路の記録から伏せる仕組みだったが、記録先を 1 つ塞いでも、
	// 記録先が増えるたびに同じ検討が要る。URL に載せなければ、どの記録にも
	// 現れない。
	"redactPaths": "app.redactPaths は廃止しました。" +
		"URL に載せられない値は、URL のフラグメント (#) に置いて、" +
		"サーバへは本文で渡してください " +
		"(フラグメントは送信されないので、経路にも CDN にも届きません)",
}

func rejectRemoved(b []byte) error {
	// App だけを鍵の集合として読み直す。値の形は問わない。
	var raw struct {
		App map[string]json.RawMessage `json:"app"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	for key, msg := range removed {
		if _, ok := raw.App[key]; ok {
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

func (m *Manifest) validate() error {
	if m.App.Port < 0 || m.App.Port > 65535 {
		return fmt.Errorf("app.port が範囲外です: %d", m.App.Port)
	}
	if m.App.DatabaseName != "" && !dbNameRE.MatchString(m.App.DatabaseName) {
		// DB 名は SQL とファイルパスの両方に現れる。
		return fmt.Errorf("DB 名に使えない文字が含まれています: %q", m.App.DatabaseName)
	}
	for env, v := range m.App.Env {
		if !envRE.MatchString(env) {
			return fmt.Errorf("環境変数名に使えない文字が含まれています: %q", env)
		}
		// 値は unit ファイルの 1 行として書き出す。改行が入ると別の設定行を
		// 差し込める。
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("環境変数 %s の値に改行が含まれています", env)
		}
	}
	for name, w := range m.Workloads {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("workload 名に使えない文字が含まれています: %q", name)
		}
		if w.Role != "" && w.Role != RoleOwner && w.Role != RoleApp {
			return fmt.Errorf("workload %s の role が不正です: %q", name, w.Role)
		}
	}
	return nil
}

// DB ロールの種別。
const (
	RoleOwner = "owner"
	RoleApp   = "app"
)

func withDefaults(m *Manifest) *Manifest {
	if m.App.Port == 0 {
		m.App.Port = DefaultPort
	}
	if m.App.Health == "" {
		m.App.Health = DefaultHealth
	}
	for name, w := range m.Workloads {
		if w.Role == "" {
			// migration は既定で owner。それ以外は app に閉じる。
			if name == "migration" {
				w.Role = RoleOwner
			} else {
				w.Role = RoleApp
			}
			m.Workloads[name] = w
		}
	}
	return m
}
