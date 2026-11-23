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

// TestSearchLiteralMetacharacters 覆盖正则元字符按字面匹配（等价 grep -iF）：
// "a.b*" 只命中字面 "a.b*"，不命中 "axb" / "aaabbb"。
func TestSearchLiteralMetacharacters(t *testing.T) {
	content := "prefix a.b* suffix\nprefix axb suffix\naaabbb\nA.B* uppercase\nplain\n"
	registry := fileRegistry(t, writeTempFile(t, content))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Query: "a.b*"})
	if result.Count != 2 {
		t.Fatalf("count=%d，期望 2（大小写不敏感、仅字面）", result.Count)
	}
	if result.Matches[0].LineNo != 1 || result.Matches[1].LineNo != 4 {
		t.Fatalf("行号=%d/%d，期望 1/4", result.Matches[0].LineNo, result.Matches[1].LineNo)
	}
	for _, m := range result.Matches {
		if !strings.Contains(m.Text, "a.b*") && !strings.Contains(m.Text, "A.B*") {
			t.Fatalf("命中 %q 不含字面 a.b*", m.Text)
		}
	}
	// 常见日志检索元字符组合同样按字面。
	result = mustSearch(t, registry, SearchRequest{Source: "app", Query: "(ERROR|WARN)"})
	if result.Count != 0 {
		t.Fatalf("count=%d，期望 0（无该字面行）", result.Count)
	}
}

// TestSearchTotalAndTruncated 覆盖全文命中总数：达到 limit 后继续扫描计数，
// total > count 即置 truncated。
func TestSearchTotalAndTruncated(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, makeLines(300)))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Query: "line", Limit: 10})
	if result.Total != 300 || result.Count != 10 || result.Limit != 10 {
		t.Fatalf("total=%d count=%d，期望 300/10", result.Total, result.Count)
	}
	if !result.Truncated {
		t.Fatalf("total>count 应置 truncated")
	}
	if result.Matches[0].LineNo != 1 || result.Matches[9].LineNo != 10 {
		t.Fatalf("应保留前 10 条，首末=%d/%d", result.Matches[0].LineNo, result.Matches[9].LineNo)
	}
	full := mustSearch(t, registry, SearchRequest{Source: "app", Query: "line", Limit: 1000})
	if full.Total != 300 || full.Count != 300 || full.Truncated {
		t.Fatalf("全量命中 total=%d count=%d truncated=%v", full.Total, full.Count, full.Truncated)
	}
}

// TestSearchTimeAndLevelFields 覆盖命中行的 time 规范化与 level 解析。
func TestSearchTimeAndLevelFields(t *testing.T) {
	content := "2026-11-22T10:00:00Z ERROR disk failure\n" +
		"2026-11-22 10:01:00 WARN high load\n" +
		"Nov 23 10:17:01 host kernel: error-ish\n"
	registry := fileRegistry(t, writeTempFile(t, content))
	result := mustSearch(t, registry, SearchRequest{Source: "app", Query: "r"})
	if result.Count != 3 {
		t.Fatalf("count=%d，期望 3", result.Count)
	}
	if result.Matches[0].Time != "2026-11-22T10:00:00Z" || result.Matches[0].Level != "error" {
		t.Fatalf("m0=%+v，期望 RFC3339+error", result.Matches[0])
	}
	if result.Matches[1].Time != "2026-11-22T10:01:00+08:00" && result.Matches[1].Time != "2026-11-22T10:01:00Z" {
		t.Fatalf("m1.time=%q，期望本地时区规范形", result.Matches[1].Time)
	}
	if result.Matches[1].Level != "warn" {
		t.Fatalf("m1.level=%q，期望 warn", result.Matches[1].Level)
	}
	if result.Matches[2].Time != "" {
		t.Fatalf("syslog 行无年份时间，期望 time 为空，got %q", result.Matches[2].Time)
	}
	if result.Matches[2].Level != "" {
		t.Fatalf("syslog 行无级别词，期望 level 为空，got %q", result.Matches[2].Level)
	}
}

// TestSearchKeywordCompat 覆盖 0.2.x keyword 字段继续可用，query 优先。
func TestSearchKeywordCompat(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "alpha ERROR\nbeta QUERY\n"))
	byKeyword := mustSearch(t, registry, SearchRequest{Source: "app", Keyword: "error"})
	if byKeyword.Count != 1 || byKeyword.Keyword != "error" {
		t.Fatalf("keyword 兼容失效: %+v", byKeyword)
	}
	both := mustSearch(t, registry, SearchRequest{Source: "app", Query: "alpha", Keyword: "QUERY"})
	if both.Count != 1 || both.Matches[0].LineNo != 1 {
		t.Fatalf("query 应优先于 keyword: %+v", both)
	}
}
