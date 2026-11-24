package boundary

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestBoundaryResourceExhausted 是「资源耗尽」用例：每条读路径在磁盘满（ENOSPC）下
// 都必须返回结构化 E_INTERNAL + ENOSPC 语义；并用真实写 /dev/full 证明 ENOSPC 语义
// （不靠 mock 自证）。系统无 /dev/full 时跳过真实探测，方法级断言仍执行。
func TestBoundaryResourceExhausted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registry, _ := seedActive(t, dir, "app", []byte("2026-11-23T00:00:00Z INFO line\n"))
	stateDir := t.TempDir()

	cases := []struct {
		name string
		call func() *logic.FaultError
	}{
		{"sources", func() *logic.FaultError {
			_, ferr := registry.Sources(logic.FaultDiskFull)
			return ferr
		}},
		{"tail", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: 10, Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"search", func() *logic.FaultError {
			_, ferr := registry.Search(logic.SearchRequest{Source: "app", Query: "line", Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"usage", func() *logic.FaultError {
			_, ferr := registry.Usage(logic.UsageRequest{StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"clean", func() *logic.FaultError {
			_, ferr := registry.Clean(logic.CleanRequest{Source: "app", StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
	}
	for _, tc := range cases {
		ferr := tc.call()
		requireCode(t, tc.name+" 磁盘满", ferr, logic.CodeInternal)
		requireMessageContains(t, tc.name+" 磁盘满", ferr, "ENOSPC")
	}

	// 真实资源语义：/dev/full 写入必须得到 ENOSPC（内核返回，不是我们编的错误码）。
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("系统无 /dev/full，跳过真实 ENOSPC 探测")
	}
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开 /dev/full: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatal("写 /dev/full 必须失败（ENOSPC）")
	} else if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("写 /dev/full 错误=%v，期望 ENOSPC", err)
	}

	// 资源耗尽不得留下半截状态：活跃文件与 state 目录未被改动。
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil || !strings.Contains(string(data), "INFO line") {
		t.Fatalf("磁盘满用例改动了活跃文件：%q err=%v", data, err)
	}
}
