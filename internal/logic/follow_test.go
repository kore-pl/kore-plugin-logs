package logic

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// followOnce 调用 Follow 并在无错误时返回结果。
func followOnce(t *testing.T, registry *Registry, req FollowRequest) *FollowResult {
	t.Helper()
	result, ferr := registry.Follow(req)
	if ferr != nil {
		t.Fatalf("Follow 意外错误: %v", ferr)
	}
	return result
}

// appendFile 向文件追加内容（模拟日志持续写入）。
func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("打开文件失败: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatalf("追加内容失败: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
}

// TestFollowCursorIncremental 覆盖游标增量：不重不漏、末尾不完整行不下发、补齐后只回一次。
func TestFollowCursorIncremental(t *testing.T) {
	path := writeTempFile(t, "a\nb\nc\n")
	registry := fileRegistry(t, path)

	first := followOnce(t, registry, FollowRequest{Source: "app"})
	if first.Count != 3 || !first.EOF || first.Cursor == "" {
		t.Fatalf("first=%+v，期望 3 条、eof=true、有游标", first)
	}
	if first.Position != int64(len("a\nb\nc\n")) {
		t.Fatalf("position=%d，期望文件尾", first.Position)
	}

	// 无新增：游标不变、不重复下发。
	idle := followOnce(t, registry, FollowRequest{Source: "app", Cursor: first.Cursor})
	if idle.Count != 0 || !idle.EOF || idle.Cursor != first.Cursor {
		t.Fatalf("idle=%+v，期望 0 条、eof=true、游标不变", idle)
	}

	appendFile(t, path, "d\ne")
	partial := followOnce(t, registry, FollowRequest{Source: "app", Cursor: first.Cursor})
	if partial.Count != 1 || partial.Entries[0].Text != "d" {
		t.Fatalf("partial=%+v，期望只回完整行 d", partial)
	}
	if partial.EOF {
		t.Fatalf("末尾有不完整行时 eof 必须为 false")
	}

	// 补全换行后才下发 e，且不重复 d。
	appendFile(t, path, "\n")
	completed := followOnce(t, registry, FollowRequest{Source: "app", Cursor: partial.Cursor})
	if completed.Count != 1 || completed.Entries[0].Text != "e" || !completed.EOF {
		t.Fatalf("completed=%+v，期望只回 e 且 eof=true", completed)
	}

	// 再取一次：无新增，不重复。
	again := followOnce(t, registry, FollowRequest{Source: "app", Cursor: completed.Cursor})
	if again.Count != 0 || !again.EOF {
		t.Fatalf("again=%+v，期望 0 条", again)
	}
}

// TestFollowRotationReset 覆盖文件轮转/截断：偏移失效后重置为尾部窗口并置 reset。
func TestFollowRotationReset(t *testing.T) {
	path := writeTempFile(t, "old-1\nold-2\n")
	registry := fileRegistry(t, path)
	first := followOnce(t, registry, FollowRequest{Source: "app"})

	if err := os.WriteFile(path, []byte("new-1\n"), 0o600); err != nil {
		t.Fatalf("重写文件失败: %v", err)
	}
	reset := followOnce(t, registry, FollowRequest{Source: "app", Cursor: first.Cursor})
	if !reset.Reset {
		t.Fatalf("reset=%+v，期望 reset=true（偏移超过当前大小）", reset)
	}
	if reset.Count != 1 || reset.Entries[0].Text != "new-1" || !reset.EOF {
		t.Fatalf("reset=%+v，期望回退尾部窗口 1 条", reset)
	}
	if !strings.Contains(reset.Note, "轮转") {
		t.Fatalf("note=%q，期望说明轮转重置", reset.Note)
	}
}

// TestFollowCursorInvalid 覆盖非法游标 / 其它来源游标：按失效处理并重置。
func TestFollowCursorInvalid(t *testing.T) {
	path := writeTempFile(t, "x\ny\n")
	registry := fileRegistry(t, path)
	cases := []struct {
		name, cursor string
	}{
		{"非 base64", "not-base64!!"},
		{"其它来源", encodeFollowCursor("other", 1, 1)},
		{"版本不符", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"source":"app","position":1,"size":1}`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := followOnce(t, registry, FollowRequest{Source: "app", Cursor: tc.cursor})
			if !result.Reset || result.Count != 2 || !result.EOF {
				t.Fatalf("result=%+v，期望 reset=true 且回退尾部 2 行", result)
			}
			if result.Note == "" {
				t.Fatalf("重置应带说明")
			}
		})
	}
}

// TestFollowKeywordAdvances 覆盖 keyword 过滤时游标仍推进过不命中行（否则会死循环重复扫描）。
func TestFollowKeywordAdvances(t *testing.T) {
	path := writeTempFile(t, "seed\n")
	registry := fileRegistry(t, path)
	first := followOnce(t, registry, FollowRequest{Source: "app"})
	appendFile(t, path, "noise-1\nERROR real\nnoise-2\n")
	filtered := followOnce(t, registry, FollowRequest{Source: "app", Cursor: first.Cursor, Keyword: "error"})
	if filtered.Count != 1 || !strings.Contains(filtered.Entries[0].Text, "real") {
		t.Fatalf("filtered=%+v，期望只回 ERROR 行", filtered)
	}
	if filtered.Position != int64(len("seed\nnoise-1\nERROR real\nnoise-2\n")) {
		t.Fatalf("position=%d，期望推进到文件尾（跳过不命中行）", filtered.Position)
	}
	again := followOnce(t, registry, FollowRequest{Source: "app", Cursor: filtered.Cursor, Keyword: "error"})
	if again.Count != 0 || !again.EOF {
		t.Fatalf("again=%+v，期望无重复命中", again)
	}
}

// TestFollowWindowTruncation 覆盖 max_lines 截断：只回前 N 条并置 truncated，剩余靠下一次游标补齐。
func TestFollowWindowTruncation(t *testing.T) {
	path := writeTempFile(t, "seed\n")
	registry := fileRegistry(t, path)
	first := followOnce(t, registry, FollowRequest{Source: "app"})
	appendFile(t, path, makeLines(5))
	part := followOnce(t, registry, FollowRequest{Source: "app", Cursor: first.Cursor, MaxLines: 2})
	if part.Count != 2 || !part.Truncated || part.EOF {
		t.Fatalf("part=%+v，期望 2 条并标记 truncated", part)
	}
	if part.Entries[0].Text != "line-000000" || part.Entries[1].Text != "line-000001" {
		t.Fatalf("entries=%+v，期望按顺序回前两条", part.Entries)
	}
	rest := followOnce(t, registry, FollowRequest{Source: "app", Cursor: part.Cursor, MaxLines: 10})
	if rest.Count != 3 || !rest.EOF || rest.Truncated {
		t.Fatalf("rest=%+v，期望补齐剩余 3 条且 eof", rest)
	}
	// 拼接后不重不漏。
	if rest.Entries[0].Text != "line-000002" || rest.Entries[2].Text != "line-000004" {
		t.Fatalf("rest entries=%+v", rest.Entries)
	}
}

// TestFollowJournalUnsupported 覆盖 journal 来源：无稳定偏移，明确 E_UNSUPPORTED。
func TestFollowJournalUnsupported(t *testing.T) {
	registry := NewRegistry([]SourceSpec{{Name: "journal", Type: SourceTypeJournal}})
	_, ferr := registry.Follow(FollowRequest{Source: "journal"})
	if ferr == nil || ferr.Code != CodeUnsupported {
		t.Fatalf("ferr=%+v，期望 E_UNSUPPORTED", ferr)
	}
}

// TestFollowValidation 覆盖来源/上限/关键字校验。
func TestFollowValidation(t *testing.T) {
	path := writeTempFile(t, "x\n")
	registry := fileRegistry(t, path)
	cases := []struct {
		name string
		req  FollowRequest
	}{
		{"缺 source", FollowRequest{}},
		{"未登记来源", FollowRequest{Source: "bogus"}},
		{"max_lines 超限", FollowRequest{Source: "app", MaxLines: MaxFollowLines + 1}},
		{"max_lines 负数", FollowRequest{Source: "app", MaxLines: -1}},
		{"max_bytes 超限", FollowRequest{Source: "app", MaxBytes: MaxFollowBytes + 1}},
		{"max_bytes 负数", FollowRequest{Source: "app", MaxBytes: -1}},
		{"keyword 超长", FollowRequest{Source: "app", Keyword: strings.Repeat("x", MaxKeywordLen+1)}},
		{"fault 注入", FollowRequest{Source: "app", Fault: FaultTimeout}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ferr := registry.Follow(tc.req); ferr == nil {
				t.Fatalf("期望返回错误")
			}
		})
	}
}
