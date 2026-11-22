package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempFile 在 t.TempDir() 下写一个文件并返回路径。
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return path
}

// fileRegistry 用真实文件构建单来源注册表。
func fileRegistry(t *testing.T, path string) *Registry {
	t.Helper()
	return NewRegistry([]SourceSpec{{Name: "app", Path: path, Type: SourceTypeFile}})
}

// makeLines 生成 n 行 "line-i"（0 起补零）。
func makeLines(n int) string {
	var builder strings.Builder
	for i := 0; i < n; i++ {
		builder.WriteString("line-")
		builder.WriteString(padNumber(i))
		builder.WriteString("\n")
	}
	return builder.String()
}

// padNumber 把整数补零到 6 位，保证行内容等长。
func padNumber(n int) string {
	digits := []byte("000000")
	for i := len(digits) - 1; i >= 0 && n > 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits)
}

// mustTail 调用 tail 并在无错误时返回结果。
func mustTail(t *testing.T, registry *Registry, req TailRequest) *TailResult {
	t.Helper()
	result, ferr := registry.Tail(req)
	if ferr != nil {
		t.Fatalf("Tail 意外错误: %v", ferr)
	}
	return result
}

// mustSearch 调用 search 并在无错误时返回结果。
func mustSearch(t *testing.T, registry *Registry, req SearchRequest) *SearchResult {
	t.Helper()
	result, ferr := registry.Search(req)
	if ferr != nil {
		t.Fatalf("Search 意外错误: %v", ferr)
	}
	return result
}

// mustStats 调用 stats 并在无错误时返回结果。
func mustStats(t *testing.T, registry *Registry, source string) *StatsResult {
	t.Helper()
	result, ferr := registry.Stats(StatsRequest{Source: source})
	if ferr != nil {
		t.Fatalf("Stats 意外错误: %v", ferr)
	}
	return result
}
