package fault

import (
	"os"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// cgroupMemoryMaxPath 通过 /proc/self/cgroup 定位本进程 cgroup v2 的 memory.max，
// 避免写死根路径（本机会话在 user.slice scope 子目录下）。
func cgroupMemoryMaxPath() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup/memory.max"
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[2] != "" {
			return "/sys/fs/cgroup" + parts[2] + "/memory.max"
		}
	}
	return "/sys/fs/cgroup/memory.max"
}

// TestFaultCgroup 验证 cgroup memory.max 超限故障返回 E_INTERNAL，并只读
// 探测本进程 cgroup v2 memory.max（Tier B 无该文件时仍以方法级断言为准）。
func TestFaultCgroup(t *testing.T) {
	registry := logic.NewRegistry(nil)
	result, ferr := registry.Sources(logic.FaultCgroup)
	if result != nil {
		t.Fatalf("cgroup 故障下不应有结果，实际=%+v", result)
	}
	if ferr == nil || ferr.Code != "E_INTERNAL" || !strings.Contains(ferr.Message, "cgroup") {
		t.Fatalf("ferr=%v，期望 E_INTERNAL/cgroup", ferr)
	}

	_, ferr = registry.Tail(logic.TailRequest{Source: "system", Fault: logic.FaultCgroup})
	if ferr == nil || ferr.Code != "E_INTERNAL" {
		t.Fatalf("query cgroup ferr=%v，期望 E_INTERNAL", ferr)
	}

	memoryMax := cgroupMemoryMaxPath()
	data, err := os.ReadFile(memoryMax)
	if err != nil {
		// Tier B 无该控制器文件时不算失败：方法级故障断言已通过，
		// 只读探测属加分证据，缺失时明确记录而不伪造通过。
		t.Logf("Tier B: 无法读取 %s: %v（方法级断言仍有效）", memoryMax, err)
		return
	}
	limit := strings.TrimSpace(string(data))
	if limit == "" {
		t.Fatalf("%s 内容为空", memoryMax)
	}
	// limit 为 "max" 或正整数字节数，二者都是合法 cgroup v2 取值。
	if limit != "max" {
		for _, c := range limit {
			if c < '0' || c > '9' {
				t.Fatalf("memory.max=%q，期望 max 或正整数", limit)
			}
		}
	}
}
