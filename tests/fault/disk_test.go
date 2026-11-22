package fault

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestFaultDiskFull 验证磁盘满故障返回 E_INTERNAL，并真实写 /dev/full
// 确认 ENOSPC 语义（若系统无 /dev/full 则跳过该真实探测，方法级断言仍执行）。
func TestFaultDiskFull(t *testing.T) {
	registry := logic.NewRegistry(nil)
	result, ferr := registry.Sources(logic.FaultDiskFull)
	if result != nil {
		t.Fatalf("disk-full 故障下不应有结果，实际=%+v", result)
	}
	if ferr == nil || ferr.Code != "E_INTERNAL" || !strings.Contains(ferr.Message, "ENOSPC") {
		t.Fatalf("ferr=%v，期望 E_INTERNAL/ENOSPC", ferr)
	}

	_, ferr = registry.Tail(logic.TailRequest{Source: "system", Fault: logic.FaultDiskFull})
	if ferr == nil || ferr.Code != "E_INTERNAL" {
		t.Fatalf("query disk-full ferr=%v，期望 E_INTERNAL", ferr)
	}

	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("系统无 /dev/full，跳过真实 ENOSPC 探测")
	}
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开 /dev/full 失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write([]byte("x"))
	if err == nil {
		t.Fatal("写 /dev/full 应返回 ENOSPC")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("写 /dev/full 错误=%v，期望 ENOSPC", err)
	}
}
