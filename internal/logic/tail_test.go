package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTailLast100Of10k 覆盖 1 万行日志 tail 100 返回真正的最后 100 行。
func TestTailLast100Of10k(t *testing.T) {
	path := writeTempFile(t, makeLines(10000))
	registry := fileRegistry(t, path)
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 100})
	if result.Count != 100 || len(result.Entries) != 100 {
		t.Fatalf("count=%d len=%d，期望 100", result.Count, len(result.Entries))
	}
	if result.Entries[0].Text != "line-009900" || result.Entries[99].Text != "line-009999" {
		t.Fatalf("首行=%q 末行=%q", result.Entries[0].Text, result.Entries[99].Text)
	}
	if result.ScannedBytes > 4*ReadBlockSize {
		t.Fatalf("scanned_bytes=%d，期望远小于文件（%d 行）", result.ScannedBytes, 10000)
	}
	if result.Truncated || result.LineTruncated {
		t.Fatalf("非截断场景 truncated=%v line_truncated=%v", result.Truncated, result.LineTruncated)
	}
}

// TestTailDefaultAndMaxLines 覆盖默认 100 行与上限 2000 行。
func TestTailDefaultAndMaxLines(t *testing.T) {
	path := writeTempFile(t, makeLines(2500))
	registry := fileRegistry(t, path)
	def := mustTail(t, registry, TailRequest{Source: "app"})
	if def.Requested != DefaultTailLines || def.Count != DefaultTailLines {
		t.Fatalf("requested=%d count=%d，期望默认 100", def.Requested, def.Count)
	}
	max := mustTail(t, registry, TailRequest{Source: "app", Lines: MaxTailLines})
	if max.Requested != MaxTailLines || max.Count != MaxTailLines {
		t.Fatalf("requested=%d count=%d，期望 %d", max.Requested, max.Count, MaxTailLines)
	}
	if max.Entries[0].Text != "line-000500" {
		t.Fatalf("首行=%q，期望 line-000500", max.Entries[0].Text)
	}
}

// TestTailKeywordAndSince 覆盖 keyword 子串过滤与 since 行首时间戳过滤。
func TestTailKeywordAndSince(t *testing.T) {
	content := "2026-11-22T10:00:00Z early one\n" +
		"2026-11-22T10:05:00Z middle ERROR\n" +
		"2026-11-22T10:10:00Z late ERROR\n"
	registry := fileRegistry(t, writeTempFile(t, content))

	byKeyword := mustTail(t, registry, TailRequest{Source: "app", Lines: 10, Keyword: "error"})
	if byKeyword.Count != 2 || !strings.Contains(byKeyword.Entries[0].Text, "middle") || !strings.Contains(byKeyword.Entries[1].Text, "late") {
		t.Fatalf("keyword 过滤结果=%+v", byKeyword.Entries)
	}

	bySince := mustTail(t, registry, TailRequest{Source: "app", Lines: 10, Since: "2026-11-22T10:06:00Z"})
	if bySince.Count != 1 || !strings.Contains(bySince.Entries[0].Text, "late") {
		t.Fatalf("since 过滤结果=%+v", bySince.Entries)
	}

	both := mustTail(t, registry, TailRequest{Source: "app", Lines: 10, Keyword: "ERROR", Since: "2026-11-22T10:06:00Z"})
	if both.Count != 1 || !strings.Contains(both.Entries[0].Text, "late") {
		t.Fatalf("keyword+since 结果=%+v", both.Entries)
	}
}

// TestTailSinceSkipsUnparsableLines 覆盖行首时间戳解析失败时跳过过滤（保留该行）。
func TestTailSinceSkipsUnparsableLines(t *testing.T) {
	content := "no-timestamp line\n2026-11-22T10:00:00Z old line\n"
	registry := fileRegistry(t, writeTempFile(t, content))
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 10, Since: "2026-11-22T10:05:00Z"})
	if result.Count != 1 || result.Entries[0].Text != "no-timestamp line" {
		t.Fatalf("结果=%+v，期望保留无法解析时间戳的行", result.Entries)
	}
}

// TestTailKeywordScansUntilEnough 覆盖 keyword 命中稀疏时继续向文件头扫描凑够 N 行。
func TestTailKeywordScansUntilEnough(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < 500; i++ {
		if i%100 == 0 {
			builder.WriteString("ERROR hit-")
			builder.WriteString(padNumber(i))
			builder.WriteString("\n")
		} else {
			builder.WriteString("info filler-")
			builder.WriteString(padNumber(i))
			builder.WriteString("\n")
		}
	}
	registry := fileRegistry(t, writeTempFile(t, builder.String()))
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 3, Keyword: "error"})
	if result.Count != 3 || !strings.Contains(result.Entries[2].Text, "hit-000400") {
		t.Fatalf("结果=%+v，期望最后 3 条 ERROR", result.Entries)
	}
}

// TestTailEdgeBoundaries 覆盖空文件、单行、无尾换行、空行等边界。
func TestTailEdgeBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		lines    int
		expected []string
	}{
		{"空文件", "", 10, nil},
		{"单个换行", "\n", 10, []string{""}},
		{"两个换行", "\n\n", 10, []string{"", ""}},
		{"无尾换行", "a", 10, []string{"a"}},
		{"尾换行", "a\n", 10, []string{"a"}},
		{"多行取尾", "a\nb\nc\n", 2, []string{"b", "c"}},
		{"多行无尾换行", "a\nb\nc", 2, []string{"b", "c"}},
		{"尾部空行", "x\n\n", 1, []string{""}},
		{"请求超过总行数", "a\nb\n", 10, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := fileRegistry(t, writeTempFile(t, tc.content))
			result := mustTail(t, registry, TailRequest{Source: "app", Lines: tc.lines})
			if result.Count != len(tc.expected) {
				t.Fatalf("count=%d，期望 %d（%+v）", result.Count, len(tc.expected), result.Entries)
			}
			for i, want := range tc.expected {
				if result.Entries[i].Text != want {
					t.Fatalf("第 %d 行=%q，期望 %q", i, result.Entries[i].Text, want)
				}
			}
			if result.Truncated {
				t.Fatalf("非预算耗尽场景不应 truncated")
			}
		})
	}
}

// TestTailLongLineTruncated 覆盖单行超 8 KiB 截断并标记。
func TestTailLongLineTruncated(t *testing.T) {
	long := strings.Repeat("a", MaxLineBytes*2)
	registry := fileRegistry(t, writeTempFile(t, long+"\nshort\n"))
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 2})
	if result.Count != 2 || !result.Entries[0].Truncated || !result.LineTruncated {
		t.Fatalf("结果=%+v，期望首行截断", result.Entries)
	}
	if len(result.Entries[0].Text) != MaxLineBytes {
		t.Fatalf("首行长度=%d，期望 %d", len(result.Entries[0].Text), MaxLineBytes)
	}
	if result.Entries[1].Text != "short" {
		t.Fatalf("次行=%q", result.Entries[1].Text)
	}
}

// TestTailBudgetOnHugeSingleLine 覆盖 64 MiB 预算耗尽：无换行大文件不整读。
func TestTailBudgetOnHugeSingleLine(t *testing.T) {
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
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 10})
	elapsed := time.Since(start)
	if !result.Truncated {
		t.Fatalf("无换行大文件应因预算耗尽 truncated=true")
	}
	if result.ScannedBytes != MaxScanBytes {
		t.Fatalf("scanned_bytes=%d，期望 %d", result.ScannedBytes, MaxScanBytes)
	}
	if elapsed > time.Second {
		t.Fatalf("耗时 %v，期望 < 1s", elapsed)
	}
}

// TestTailLargeSparsePerf 覆盖 100 MiB 稀疏文件 tail 200：只读文件尾块，耗时 < 1s。
func TestTailLargeSparsePerf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建大文件失败: %v", err)
	}
	const size = 100 << 20
	if err := file.Truncate(size); err != nil {
		t.Fatalf("创建稀疏文件失败: %v", err)
	}
	tailBlock := makeLines(300)
	if _, err := file.WriteAt([]byte(tailBlock), size-int64(len(tailBlock))); err != nil {
		t.Fatalf("写入尾部日志失败: %v", err)
	}
	_ = file.Close()
	registry := fileRegistry(t, path)

	start := time.Now()
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 200})
	elapsed := time.Since(start)
	if result.Count != 200 || result.Entries[199].Text != "line-000299" {
		t.Fatalf("count=%d 末行=%q", result.Count, result.Entries[result.Count-1].Text)
	}
	if result.ScannedBytes >= size {
		t.Fatalf("scanned_bytes=%d，不应接近文件大小 %d", result.ScannedBytes, size)
	}
	if result.ScannedBytes > 2*ReadBlockSize {
		t.Fatalf("scanned_bytes=%d，期望只读 1-2 个 64 KiB 块", result.ScannedBytes)
	}
	if elapsed > time.Second {
		t.Fatalf("耗时 %v，期望 < 1s", elapsed)
	}
	t.Logf("PERF tail 200 on %d MiB sparse file: elapsed=%v scanned_bytes=%d", size>>20, elapsed, result.ScannedBytes)
}
