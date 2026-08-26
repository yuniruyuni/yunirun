package system

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 実際に踏んだ形。テーブルはあるが、アプリのロールがどれにも権限を持たない。
// schema 側の GRANT 先が別のロール名になっているときにこうなる。
func TestParseGrantCountsReadsTotalAndGranted(t *testing.T) {
	total, granted, err := parseGrantCounts("1|0\n")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || granted != 0 {
		t.Fatalf("total=%d granted=%d", total, granted)
	}
}

func TestParseGrantCountsIgnoresSurroundingNoise(t *testing.T) {
	total, granted, err := parseGrantCounts("\n 3 | 3 \n\n")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || granted != 3 {
		t.Fatalf("total=%d granted=%d", total, granted)
	}
}

// 読み取れない出力を 0 件として扱うと、権限が無い状態を見逃す。
func TestParseGrantCountsFailsOnUnreadableOutput(t *testing.T) {
	if _, _, err := parseGrantCounts("ERROR: something\n"); err == nil {
		t.Fatal("読み取れない出力を通している")
	}
}

// 公式イメージは初期化の途中で一時的なサーバを立て、そこで POSTGRES_DB を
// 作る。この間 postgres には繋がるので、postgres で判定すると「応答したのに
// 目的の DB が無い」という窓を通り抜けてしまう。e2e が間欠的に落ちていた。
type initializingDB struct {
	askedFor []string
	// ready になるまでは目的の DB だけが無い状態を模す。
	targetReadyAfter int
	calls            int
}

func (f *initializingDB) Run(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
	return nil, nil
}

func (f *initializingDB) RunEnv(_ context.Context, _ []byte, _ []string, _ string, args ...string) ([]byte, error) {
	db := ""
	for i, a := range args {
		if a == "-d" && i+1 < len(args) {
			db = args[i+1]
		}
	}
	f.askedFor = append(f.askedFor, db)
	if db == "postgres" {
		// 初期化中でも postgres には繋がる。
		return nil, nil
	}
	f.calls++
	if f.calls <= f.targetReadyAfter {
		return nil, fmt.Errorf(`database "%s" does not exist`, db)
	}
	return nil, nil
}

func TestWaitReadyWaitsForTheTargetDatabaseNotPostgres(t *testing.T) {
	defer func(d time.Duration) { readyInterval = d }(readyInterval)
	readyInterval = time.Millisecond
	f := &initializingDB{targetReadyAfter: 2}
	c := Conn{SocketDir: "/sock", Owner: "beta", Password: "x"}
	if err := WaitReady(context.Background(), f, c, "beta", 10); err != nil {
		t.Fatal(err)
	}
	for _, db := range f.askedFor {
		if db == "postgres" {
			t.Fatal("postgres で判定している。初期化中でも繋がるので窓を通り抜ける")
		}
	}
	if len(f.askedFor) < 3 {
		t.Fatalf("目的の DB が出来るまで待っていない: %v", f.askedFor)
	}
}

// 待っても出来ないなら、何が使えないのかを言う。
func TestWaitReadyReportsWhichDatabase(t *testing.T) {
	defer func(d time.Duration) { readyInterval = d }(readyInterval)
	readyInterval = time.Millisecond
	f := &initializingDB{targetReadyAfter: 100}
	c := Conn{SocketDir: "/sock", Owner: "beta", Password: "x"}
	err := WaitReady(context.Background(), f, c, "beta", 1)
	if err == nil {
		t.Fatal("落ちていない")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("どの DB か言っていない: %v", err)
	}
}
