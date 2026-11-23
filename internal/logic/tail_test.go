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

// TestTailDefaultAndMaxLines 覆盖默认 100 行与上限 500 行。
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
	if max.Entries[0].Text != "line-002000" {
		t.Fatalf("首行=%q，期望 line-002000", max.Entries[0].Text)
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

// TestTailPositionIncremental 覆盖增量拉取：首次 tail 得到 position，追加后只回新行、
// 不重复、position 前进到文件尾。
func TestTailPositionIncremental(t *testing.T) {
	path := writeTempFile(t, "line-1\nline-2\nline-3\n")
	registry := fileRegistry(t, path)
	first := mustTail(t, registry, TailRequest{Source: "app", Lines: 100})
	if first.Count != 3 || first.Position == 0 {
		t.Fatalf("first=%+v，期望 3 行与 position", first)
	}
	appended := "line-4\nline-5\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开追加句柄失败: %v", err)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	_ = f.Close()

	second := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Position: first.Position})
	if second.Count != 2 {
		t.Fatalf("second.count=%d，期望只回 2 条新行", second.Count)
	}
	if second.Entries[0].Text != "line-4" || second.Entries[1].Text != "line-5" {
		t.Fatalf("second entries=%+v，期望 line-4/line-5", second.Entries)
	}
	if second.Position != first.Position+int64(len(appended)) {
		t.Fatalf("position=%d，期望 %d", second.Position, first.Position+int64(len(appended)))
	}
	if second.Truncated {
		t.Fatalf("正常增量不应 truncated")
	}
	// 无新增时再拉：空结果且 position 不变。
	third := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Position: second.Position})
	if third.Count != 0 || third.Position != second.Position {
		t.Fatalf("third=%d/%d，期望 0 条且 position 不变", third.Count, third.Position)
	}
}

// TestTailPositionRotation 覆盖文件被轮转/截断（position 超出文件大小）时回退全量。
func TestTailPositionRotation(t *testing.T) {
	path := writeTempFile(t, "old-1\nold-2\n")
	registry := fileRegistry(t, path)
	first := mustTail(t, registry, TailRequest{Source: "app", Lines: 100})
	if err := os.WriteFile(path, []byte("new-1\n"), 0o600); err != nil {
		t.Fatalf("重写文件失败: %v", err)
	}
	second := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Position: first.Position})
	if second.Count != 1 || second.Entries[0].Text != "new-1" {
		t.Fatalf("second=%+v，期望回退全量读 new-1", second)
	}
	if second.Position != int64(len("new-1\n")) {
		t.Fatalf("position=%d，期望新文件大小", second.Position)
	}
}

// TestTailPositionPartialLine 覆盖末尾不完整行不下发：等它补全后才回，且不重复。
func TestTailPositionPartialLine(t *testing.T) {
	path := writeTempFile(t, "complete-1\n")
	registry := fileRegistry(t, path)
	first := mustTail(t, registry, TailRequest{Source: "app", Lines: 100})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开追加句柄失败: %v", err)
	}
	_, _ = f.WriteString("half-writ") // 不带换行
	_ = f.Close()
	second := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Position: first.Position})
	if second.Count != 0 {
		t.Fatalf("不完整行不应下发，got %+v", second.Entries)
	}
	if second.Position != first.Position {
		t.Fatalf("无完整新行时 position 不应推进: %d vs %d", second.Position, first.Position)
	}
	// 补上换行后（文件重写模拟补全），增量只回补全后的新行。
	if err := os.WriteFile(path, []byte("complete-1\nhalf-written\n"), 0o600); err != nil {
		t.Fatalf("补全失败: %v", err)
	}
	third := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Position: first.Position})
	if third.Count != 1 || third.Entries[0].Text != "half-written" {
		t.Fatalf("third=%+v，期望补全后只回 half-written", third.Entries)
	}
}

// TestTailPositionFilterAdvances 覆盖 keyword 过滤时 position 仍推进过全部行（否则会死循环重取）。
func TestTailPositionFilterAdvances(t *testing.T) {
	path := writeTempFile(t, "noise-1\nERROR real\nnoise-2\n")
	registry := fileRegistry(t, path)
	first := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Keyword: "error"})
	if first.Count != 1 || first.Position != int64(len("noise-1\nERROR real\nnoise-2\n")) {
		t.Fatalf("first=%+v，期望 1 条命中且 position 到文件尾", first)
	}
	second := mustTail(t, registry, TailRequest{Source: "app", Lines: 100, Keyword: "error", Position: first.Position})
	if second.Count != 0 || second.Position != first.Position {
		t.Fatalf("second=%+v，期望空且 position 不动", second)
	}
}

// TestTailPositionJumpWindow 覆盖新增内容超过 1 MiB 窗口时跳到最近 1 MiB 并置 truncated。
func TestTailPositionJumpWindow(t *testing.T) {
	filler := strings.Repeat("x", 127) + "\n" // 128B/行，1 MiB = 8192 行
	var appended strings.Builder
	for i := 0; i < 9000; i++ { // 追加 1.125 MiB > 1 MiB 窗口
		appended.WriteString(filler)
	}
	path := writeTempFile(t, "seed\n")
	registry := fileRegistry(t, path)
	first := mustTail(t, registry, TailRequest{Source: "app", Lines: 1})
	if first.Position != int64(len("seed\n")) {
		t.Fatalf("position=%d，期望种子行后", first.Position)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开追加句柄失败: %v", err)
	}
	if _, err := f.WriteString(appended.String()); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	_ = f.Close()
	second := mustTail(t, registry, TailRequest{Source: "app", Lines: MaxTailLines, Position: first.Position})
	if !second.Truncated {
		t.Fatalf("跳过窗口外内容应置 truncated")
	}
	if second.ScannedBytes > MaxIncrementalBytes {
		t.Fatalf("scanned=%d，超过 1 MiB 窗口", second.ScannedBytes)
	}
	if second.Count != MaxTailLines {
		t.Fatalf("count=%d，期望窗口内命中行按上限截取", second.Count)
	}
}

// TestTailLineTimeAndLevel 覆盖尾部条目带 time 与 level。
func TestTailLineTimeAndLevel(t *testing.T) {
	content := "2026-11-22T10:00:00Z INFO ok-1\nplain line\n2026-11-22T10:00:01Z ERROR boom\n"
	registry := fileRegistry(t, writeTempFile(t, content))
	result := mustTail(t, registry, TailRequest{Source: "app", Lines: 3})
	if result.Count != 3 {
		t.Fatalf("count=%d，期望 3", result.Count)
	}
	if result.Entries[0].Level != "info" || result.Entries[0].Time != "2026-11-22T10:00:00Z" {
		t.Fatalf("e0=%+v，期望 info+RFC3339", result.Entries[0])
	}
	if result.Entries[1].Level != "" || result.Entries[1].Time != "" {
		t.Fatalf("e1=%+v，期望无级别无时间", result.Entries[1])
	}
	if result.Entries[2].Level != "error" || result.Entries[2].Time != "2026-11-22T10:00:01Z" {
		t.Fatalf("e2=%+v，期望 error", result.Entries[2])
	}
	if result.Position != int64(len(content)) {
		t.Fatalf("position=%d，期望文件尾", result.Position)
	}
}
