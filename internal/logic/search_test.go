package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSearchLineNumbers 覆盖全文件扫描的行号、大小写不敏感与扫描字节数。
func TestSearchLineNumbers(t *testing.T) {
	content := "alpha\nERROR one\nbeta\nERROR two\n"
	registry := fileRegistry(t, writeTempFile(t, content))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "error"})
	if result.Count != 2 || result.Matches[0].LineNo != 2 || result.Matches[1].LineNo != 4 {
		t.Fatalf("matches=%+v，期望行号 2/4", result.Matches)
	}
	if result.ScannedBytes != int64(len(content)) {
		t.Fatalf("scanned_bytes=%d，期望 %d", result.ScannedBytes, len(content))
	}
	if result.Truncated {
		t.Fatalf("未达上限不应 truncated")
	}
}

// TestSearchLimitTruncation 覆盖达到 limit 即截断并标记。
func TestSearchLimitTruncation(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, makeLines(100)))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "line", Limit: 1})
	if result.Count != 1 || !result.Truncated || result.Limit != 1 {
		t.Fatalf("result=%+v，期望 limit=1/truncated", result)
	}
	if result.Matches[0].LineNo != 1 {
		t.Fatalf("line_no=%d，期望 1（正序扫描）", result.Matches[0].LineNo)
	}
}

// TestSearchAcrossChunkBoundary 覆盖 64 KiB 块边界上的行号连续性。
func TestSearchAcrossChunkBoundary(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, makeLines(10000)))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "line-009999"})
	if result.Count != 1 || result.Matches[0].LineNo != 10000 {
		t.Fatalf("matches=%+v，期望唯一命中第 10000 行", result.Matches)
	}
}

// TestSearchLongLine 覆盖超 8 KiB 行截断并标记，且不影响后续行号。
func TestSearchLongLine(t *testing.T) {
	long := strings.Repeat("a", MaxLineBytes*3)
	registry := fileRegistry(t, writeTempFile(t, long+"\nERROR next\n"))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "aaa"})
	if result.Count != 1 || !result.Matches[0].Truncated || len(result.Matches[0].Text) != MaxLineBytes {
		t.Fatalf("matches=%+v，期望超长行截断", result.Matches)
	}
	result = mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "error"})
	if result.Count != 1 || result.Matches[0].LineNo != 2 {
		t.Fatalf("matches=%+v，期望第 2 行命中", result.Matches)
	}
}

// TestSearchNoMatchAndEmpty 覆盖无命中（空数组）与空文件。
func TestSearchNoMatchAndEmpty(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "abc\ndef\n"))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "zzz"})
	if result.Count != 0 || result.Matches == nil {
		t.Fatalf("matches=%+v，期望空数组而非 null", result.Matches)
	}
	empty := fileRegistry(t, writeTempFile(t, ""))
	result = mustSearch(t, empty, SearchRequest{Source: "app", Keyword: "x"})
	if result.Count != 0 || result.ScannedBytes != 0 || result.Truncated {
		t.Fatalf("result=%+v", result)
	}
}

// TestSearchLastLineWithoutNewline 覆盖文件末行无换行时的行号。
func TestSearchLastLineWithoutNewline(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "a\nb"))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "b"})
	if result.Count != 1 || result.Matches[0].LineNo != 2 {
		t.Fatalf("matches=%+v，期望第 2 行", result.Matches)
	}
}

// TestSearchScanBudget 覆盖 64 MiB 扫描上限：大文件不整读且 truncated=true。
func TestSearchScanBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建大文件失败: %v", err)
	}
	if err := file.Truncate(100 << 20); err != nil {
		t.Fatalf("创建稀疏文件失败: %v", err)
	}
	_ = file.Close()
	registry := fileRegistry(t, path)
	start := time.Now()
	result := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "needle"})
	elapsed := time.Since(start)
	if !result.Truncated || result.ScannedBytes != MaxScanBytes {
		t.Fatalf("truncated=%v scanned=%d，期望 true/%d", result.Truncated, result.ScannedBytes, MaxScanBytes)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("matches=%+v，期望无命中", result.Matches)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("耗时 %v，期望 < 2s", elapsed)
	}
}
