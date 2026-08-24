package main

// アプリ側の秘密。
//
// 値はアプリのリポジトリに secrets/<ENV_NAME>.age として置かれ、deploy が
// 暗号文のまま運ぶ。ファイル名がそのまま環境変数名の宣言を兼ねるので、
// マニフェストには列挙しない。
//
// これ以前は infra 側 (agenix) に置いてアプリは名前だけを宣言していた。秘密を
// 増やすたびに二つのリポジトリを跨ぐ必要があり、しかも deploy の時点で値が
// 揃っているかどうかを converge 側から知りようがなかった。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuniruyuni/yunirun/internal/config"
	"github.com/yuniruyuni/yunirun/internal/system"
)

// envNameRE は環境変数名として許す形。
//
// 名前は暗号文のファイル名からそのまま作る。ここを緩めると、区切り文字を
// 含む名前が保存先の外を指せるようになる。
var envNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ageArmorHeader は armor 形式の age 暗号文の先頭。
//
// 復号は converge の時点まで行わないので、受け取った時点では中身を確かめ
// られない。せめて「age の暗号文ではないもの」は入口で弾く。
const ageArmorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"

// appSecretsDir は暗号文を保存する場所。
//
// 永続領域に置く。deploy が運んでくるものなので、ここが消えると再起動後に
// 秘密を作り直せない。マニフェストと同じ理由。
func appSecretsDir(cfg *config.Config, app string) string {
	return filepath.Join(cfg.StateDir, "appsecrets", app)
}

// inboxSecretsDir は deploy が暗号文を置く場所。
func inboxSecretsDir(app string) string {
	return filepath.Join(inboxDir(app), "secrets")
}

// checkSecretName は環境変数名として使えるかを見る。
func checkSecretName(name string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("秘密の名前に使えない文字が含まれています: %q", name)
	}
	return nil
}

// checkSecretBody は age の暗号文らしさを見る。
func checkSecretBody(name string, b []byte) error {
	if !strings.HasPrefix(strings.TrimLeft(string(b), " \t\r\n"), ageArmorHeader) {
		return fmt.Errorf("%s が age の暗号文ではありません (armor 形式で渡してください)", name)
	}
	return nil
}

// adoptAppSecrets は deploy が置いた暗号文を永続領域へ引き取る。
//
// inbox に secrets/ があるときは、そちらを正とする。アプリ側で秘密を消した
// ことを反映させたいので、差分ではなく総入れ替えにする。
//
// 引き取りは一時ディレクトリを作ってから差し替える。途中で失敗したときに、
// 一部だけ新しいという状態を残さないため。
func adoptAppSecrets(cfg *config.Config, app string) error {
	src := inboxSecretsDir(app)
	ents, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		// deploy が運んでこなかった。既に引き取ってあるものを使う。
		return nil
	}
	if err != nil {
		return err
	}

	dst := appSecretsDir(cfg, app)
	tmp := dst + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".age") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".age")
		if err := checkSecretName(name); err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := checkSecretBody(name, b); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmp, name+".age"), b, 0o400); err != nil {
			return err
		}
	}

	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// loadAppSecrets は保存してある暗号文を復号して返す。
func loadAppSecrets(ctx context.Context, r system.Runner, cfg *config.Config, app string) (map[string]string, error) {
	dir := appSecretsDir(cfg, app)
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	key := cfg.SecretsKeyPath
	out := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".age") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".age")
		if key == "" {
			return nil, fmt.Errorf("%s の秘密 %s を復号できません: secretsKeyPath が設定されていません", app, name)
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		v, err := r.Run(ctx, b, "age", "-d", "-i", key)
		if err != nil {
			return nil, fmt.Errorf("%s の秘密 %s を復号できません: %w", app, name, err)
		}
		// 末尾改行を落とす。エディタや echo が付けたものがそのまま環境変数へ
		// 入ると、認証などが静かに失敗する。
		out[name] = strings.TrimRight(string(v), "\r\n")
	}
	return out, nil
}
