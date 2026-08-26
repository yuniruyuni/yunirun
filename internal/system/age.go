package system

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// armorHeader は armor 形式の age 暗号文の先頭。
//
// yunirun 自身が作る暗号文は素のまま。armor を読めるようにしてあるのは、
// アプリの作者が `age -a` で作ったものを受け取るため。
const armorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"

// Encrypt は値を宛先へ向けて暗号化する。
//
// age(1) を呼ばずライブラリで行う。外部コマンドに依存しないことのほかに、
// 値がプロセス境界を越えないという利点がある。
func Encrypt(value string, recipients ...string) ([]byte, error) {
	var rs []age.Recipient
	for _, s := range recipients {
		if s == "" {
			continue
		}
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("宛先 %s を解釈できません: %w", s, err)
		}
		rs = append(rs, r)
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("宛先がありません")
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, value); err != nil {
		return nil, err
	}
	// Close で終端が書かれる。忘れると復号できない暗号文ができる。
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decrypt は鍵ファイルの identity で復号する。armor 形式も素の形式も読む。
func Decrypt(ciphertext []byte, keyPath string) ([]byte, error) {
	ids, err := Identities(keyPath)
	if err != nil {
		return nil, err
	}
	var src io.Reader = bytes.NewReader(ciphertext)
	if strings.HasPrefix(strings.TrimLeft(string(ciphertext), " \t\r\n"), armorHeader) {
		src = armor.NewReader(src)
	}
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// Identities は鍵ファイルを読む。
func Identities(keyPath string) ([]age.Identity, error) {
	f, err := os.Open(keyPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("%s を鍵として読めません: %w", keyPath, err)
	}
	return ids, nil
}

// Recipient は鍵ファイルから対応する公開鍵を導く。age-keygen -y にあたる。
func Recipient(keyPath string) (string, error) {
	ids, err := Identities(keyPath)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			return x.Recipient().String(), nil
		}
	}
	return "", fmt.Errorf("%s に X25519 の鍵がありません", keyPath)
}

// NewIdentity は新しい age の鍵を作り、identity ファイルの中身と公開鍵を返す。
//
// age-keygen に依存しなくなったので、新しいホストで鍵を用意する手段として要る。
func NewIdentity() (string, string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	pub := id.Recipient().String()
	body := fmt.Sprintf("# public key: %s\n%s\n", pub, id.String())
	return body, pub, nil
}
