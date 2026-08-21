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
	// 秘密は Secrets 側に書く。
	Env map[string]string `json:"env"`

	// Secrets は環境変数名から、その値を持つ秘密の名前への対応。
	//
	// 値そのものは書かない。ここに書くのは「どの秘密を、どの環境変数として
	// 渡すか」という対応だけで、値は yunirun が /run/agenix から実行時に読む。
	//
	// 秘密の実体を infra 側 (agenix) に置くのは、アプリのリポジトリに秘密を
	// 置かないという方針のため。アプリ側は名前だけを知っていればよい。
	Secrets map[string]string `json:"secrets"`

	// DatabaseName は使う DB の名前。空ならアプリ名を使う。
	//
	// 既存の DB を引き継ぐ場合に指定する。yuniruyuni.net 側で付けるアプリ名と、
	// 既に動いている DB の名前が食い違うことがあるため。名前が揃っていれば
	// 書かなくてよい。
	DatabaseName string `json:"databaseName"`

	// DatabasePasswords は既存の DB パスワードを持つ秘密の名前。
	// { "owner": "db-password-x", "app": "db-password-x_app" } の形。
	//
	// 指定すると yunirun はパスワードを生成せず、この秘密の値をそのまま使う。
	// 既存の DB を旧システムと並行して使う間、パスワードを変えると旧側の
	// 稼働中コンテナが即座に認証に失敗するため。
	//
	// 移行が済んだら外してよい。外すと次の収束で yunirun が生成した値に
	// 切り替わる (そのときは全コンテナが再起動される前提)。
	DatabasePasswords map[string]string `json:"databasePasswords"`

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
	if err := m.validate(); err != nil {
		return nil, err
	}
	return withDefaults(&m), nil
}

func (m *Manifest) validate() error {
	if m.App.Port < 0 || m.App.Port > 65535 {
		return fmt.Errorf("app.port が範囲外です: %d", m.App.Port)
	}
	if m.App.DatabaseName != "" && !dbNameRE.MatchString(m.App.DatabaseName) {
		// DB 名は SQL とファイルパスの両方に現れる。
		return fmt.Errorf("DB 名に使えない文字が含まれています: %q", m.App.DatabaseName)
	}
	for role, secret := range m.App.DatabasePasswords {
		if role != RoleOwner && role != RoleApp {
			return fmt.Errorf("databasePasswords の鍵は owner か app です: %q", role)
		}
		if !secretRE.MatchString(secret) {
			return fmt.Errorf("秘密の名前に使えない文字が含まれています: %q", secret)
		}
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
	for env, secret := range m.App.Secrets {
		// 環境変数名は unit ファイルへ書き出す。
		if !envRE.MatchString(env) {
			return fmt.Errorf("環境変数名に使えない文字が含まれています: %q", env)
		}
		// 秘密の名前はファイルパスの一部になる。ここを緩めると
		// /run/agenix の外を指せてしまう。
		if !secretRE.MatchString(secret) {
			return fmt.Errorf("秘密の名前に使えない文字が含まれています: %q", secret)
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
