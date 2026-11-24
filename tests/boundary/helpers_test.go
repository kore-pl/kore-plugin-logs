// Package boundary 是 logs 插件的边界用例（G-20 production 判据①）：
// 异常输入 / 并发 / 权限不足 / 资源耗尽 各至少一条，全部断言可观察外部行为。
//
// 与 tests/fault 的分工：fault 走「故障注入字段 + 方法级错误码」，
// boundary 走「真实环境制造边界条件」——真实 chmod 000、真实 /dev/full、
// 真实 goroutine 并发。两者不重复实现，互不替代（tests/fault 原件保持不动）。
package boundary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// seedActive 在 dir 下创建活跃日志文件并返回注册表与活跃文件路径。
func seedActive(t *testing.T, dir, name string, content []byte) (*logic.Registry, string) {
	t.Helper()
	active := filepath.Join(dir, name+".log")
	if err := os.WriteFile(active, content, 0o600); err != nil {
		t.Fatalf("写活跃文件 %s: %v", active, err)
	}
	registry := logic.NewRegistry([]logic.SourceSpec{{Name: name, Path: active, Type: logic.SourceTypeFile}})
	return registry, active
}

// rotatedStamp 返回第 i 个轮转文件的确定性时间戳名：base.<202601NN>T000000Z。
// 格式与 rotateStampLayout 一致，按字典序即时间序。
func rotatedStamp(base string, i int) string {
	return fmt.Sprintf("%s.202601%02dT000000Z", base, i)
}

// seedRotated 在 dir 下造 n 个轮转文件，越小的时间戳越旧（mtime 同步拉开）。
func seedRotated(t *testing.T, dir, base string, n int, size int) []string {
	t.Helper()
	paths := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		path := filepath.Join(dir, rotatedStamp(base, i))
		if err := os.WriteFile(path, []byte(strings.Repeat("x", size)+"\n"), 0o600); err != nil {
			t.Fatalf("写轮转文件 %s: %v", path, err)
		}
		age := timeAgo(i, n)
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatalf("设置轮转文件 mtime %s: %v", path, err)
		}
		paths = append(paths, path)
	}
	return paths
}

// mustChmod 改权限并在用例结束前还原，保证 t.TempDir 能清理干净。
func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	old, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, old.Mode().Perm()) })
}

// requireCode 断言方法级错误码；外部行为断言优先于错误文本。
func requireCode(t *testing.T, label string, ferr *logic.FaultError, code string) {
	t.Helper()
	if ferr == nil {
		t.Fatalf("%s: 期望错误码 %s，实际成功", label, code)
	}
	if ferr.Code != code {
		t.Fatalf("%s: 错误码=%s 期望=%s（message=%q）", label, ferr.Code, code, ferr.Message)
	}
}

// requireMessageContains 断言错误文案包含关键语义。
func requireMessageContains(t *testing.T, label string, ferr *logic.FaultError, want string) {
	t.Helper()
	if ferr == nil {
		t.Fatalf("%s: 期望错误包含 %q，实际成功", label, want)
	}
	if !strings.Contains(ferr.Message, want) {
		t.Fatalf("%s: message=%q 期望包含 %q", label, ferr.Message, want)
	}
}

// timeAgo 给轮转文件一个按序号递减的 mtime：序号越大越新。
func timeAgo(i, n int) time.Time {
	return time.Now().Add(-time.Duration(n-i+1) * time.Hour)
}

// listDirNames 返回目录内的文件名集合（只读探测）。
func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
