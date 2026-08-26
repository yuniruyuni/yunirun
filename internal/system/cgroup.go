package system

// アプリごとの資源使用を cgroup から読む。
//
// cAdvisor を置かないのは、必要なものが cgroup に既に揃っているから。
// アプリは rootless のユーザ unit、DB や HAProxy は root のシステム unit と
// 場所が分かれているが、どちらも同じ形式で読める。
//
// 「どのアプリが CPU を食っているか」が見えないと、原因の切り分けに時間が
// かかる。実際、1 つのアプリが CPU の 56% を使い続けていることに気付くまで
// 長くかかった。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Usage は 1 つの unit の資源使用。
type Usage struct {
	// CPUMicros は起動してから使った CPU 時間 (マイクロ秒)。
	// 累計なので、差を取って率にする。
	CPUMicros int64
	// MemoryBytes はいま使っているメモリ。
	MemoryBytes int64
}

// UserUnitPath は rootless のユーザ unit の cgroup を返す。
func UserUnitPath(uid int, unit string) string {
	return fmt.Sprintf(
		"/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice/%s",
		uid, uid, unit)
}

// SystemUnitPath は root のシステム unit の cgroup を返す。
func SystemUnitPath(unit string) string {
	return filepath.Join("/sys/fs/cgroup/system.slice", unit)
}

// ReadUsage は cgroup から資源使用を読む。
//
// 無いものは見つからないと返す。止まっている unit には cgroup が無い。
func ReadUsage(dir string) (Usage, bool) {
	cpu, ok := readCPUUsage(filepath.Join(dir, "cpu.stat"))
	if !ok {
		return Usage{}, false
	}
	mem, _ := readInt(filepath.Join(dir, "memory.current"))
	return Usage{CPUMicros: cpu, MemoryBytes: mem}, true
}

// readCPUUsage は cpu.stat の usage_usec を読む。
func readCPUUsage(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "usage_usec ")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func readInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
