package system

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// これらは age(1) が実際に作った暗号文。使い捨ての鍵なので公開して構わない。
//
// ライブラリへ移す前に置いてある暗号文が読めなくなると、DB のパスワードを
// 失って復旧できない。CLI との相互運用を固定値で押さえておく。
const (
	fixtureKey   = "AGE-SECRET-KEY-17YECZA2V4DA5PAM4Q85S0HG6HVUJ3SMU7H2LSDYQNHVDELDULJ6SZEY2VG"
	fixturePub   = "age1mxfduscue2lmtcpgmukdwa0rn2gns8vgnpsske7kfegnfwyt5uss2lhman"
	fixturePlain = "s3cret-値"

	// age -r <pub> で作った素の形式。yunirun 自身が置くのはこちら。
	fixtureBinB64 = "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSBQWk96YUJ5Q05RMTkrRGM1bi81MENHL1lhS1k5bkpLa2l0TkhJSENQQkdNCmp3YWU0V2J0Mk1ERjJNMnNJWmdua2xLeVJKYlhlc3hLU1lRSDUzSnU3MFUKLS0tIGlmQXAvQzBtQlI4L3hoVm9tLzdPeWZ4ajFZK2tLeG9STGliUklKKzkvQVkK/4rTP4j95gpjL6p8p5FkK/Xyk9S+X2TFsy5B/7dIO0uPlkvr5G8VYfDO"

	// age -a -r <pub> で作った armor 形式。アプリの作者が置くのはこちら。
	fixtureArmor = `-----BEGIN AGE ENCRYPTED FILE-----
YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSA2eTNpMklsMXRORDNJNWtt
TUNZMmlOVHk2TzAvazE1QmFPUTAyVWlNa3p3CnhmR3JSV2hrdDdqSFNrTFAyeUZw
NkpLWGZ4bUZaL2o2czVqdHJHTyt1dmMKLS0tIGZUa2Z0RDlhNkp5d0dLeWVtcEdZ
RkxpNDNwUWRIYzdIUTljRlhjYjlVbUUKWNu8NLiGE3AzUFT1uHnLzLvrX9auxoH7
hF7XFX67++qVvnLfeuA9diSS
-----END AGE ENCRYPTED FILE-----
`
)

func fixtureKeyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key.txt")
	// agenix が置くファイルと同じく、注釈行が混じっていても読めること。
	body := "# created: 2026-08-26T00:00:00Z\n# public key: " + fixturePub + "\n" + fixtureKey + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecryptReadsAgeCLIOutput(t *testing.T) {
	key := fixtureKeyFile(t)
	bin, err := base64.StdEncoding.DecodeString(fixtureBinB64)
	if err != nil {
		t.Fatal(err)
	}
	for name, ct := range map[string][]byte{
		"素の形式":  bin,
		"armor": []byte(fixtureArmor),
	} {
		got, err := Decrypt(ct, key)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != fixturePlain {
			t.Errorf("%s: %q ではなく %q", name, fixturePlain, got)
		}
	}
}

func TestRecipientMatchesAgeKeygen(t *testing.T) {
	got, err := Recipient(fixtureKeyFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if got != fixturePub {
		t.Errorf("age-keygen -y は %q を返す。得たのは %q", fixturePub, got)
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	key := fixtureKeyFile(t)
	ct, err := Encrypt(fixturePlain, fixturePub, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(ct, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fixturePlain {
		t.Errorf("%q ではなく %q", fixturePlain, got)
	}
}

// 宛先を取り違えると、復旧用の管理者鍵で開けない暗号文が静かにできる。
func TestEncryptRejectsNoRecipient(t *testing.T) {
	if _, err := Encrypt("x", "", ""); err == nil {
		t.Fatal("宛先が無いのに通った")
	}
}

// age(1) に依存しなくなったので、新しいホストで鍵を用意する手段はこれだけ。
func TestNewIdentityRoundTrips(t *testing.T) {
	body, pub, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// 書いたものが鍵として読め、公開鍵が一致すること。
	got, err := Recipient(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Fatalf("公開鍵が一致しない: %q と %q", got, pub)
	}
	ct, err := Encrypt("v", pub)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decrypt(ct, p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "v" {
		t.Fatalf("%q", out)
	}
}
