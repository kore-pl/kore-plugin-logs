package logic

import (
	"strings"
	"testing"
)

// journalToken 从 journal 行里挑一个较长 ASCII 词作为 search 关键词；挑不到返回空。
func journalToken(line string) string {
	best := ""
	for _, field := range strings.Fields(line) {
		if len(field) < 4 || len(field) > 40 {
			continue
		}
		ascii := true
		for i := 0; i < len(field); i++ {
			c := field[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '[' || c == ']') {
				ascii = false
				break
			}
		}
		if ascii && len(field) > len(best) {
			best = field
		}
	}
	return best
}

// TestJournalSource 覆盖 journal 来源的探测、tail、stats 与 search；
// 环境无 journalctl 或 journal 不可读/为空时跳过（Tier B 允许）。
func TestJournalSource(t *testing.T) {
	registry := NewRegistry([]SourceSpec{{Name: "journal", Path: "", Type: SourceTypeJournal}})
	sources, ferr := registry.Sources(FaultNone)
	if ferr != nil {
		t.Fatalf("Sources 意外错误: %v", ferr)
	}
	status := sources.Sources[0]
	if !status.Exists {
		t.Skip("未安装 journalctl，跳过 journal 用例")
	}
	if !status.Readable {
		t.Skipf("journal 不可读: %s", status.Note)
	}
	if status.Type != SourceTypeJournal || status.File != "journal" {
		t.Fatalf("status=%+v", status)
	}

	stats := mustStats(t, registry, "journal")
	if stats.Lines != nil || stats.Readable != true || stats.Path != "journal/journal" {
		t.Fatalf("stats=%+v", stats)
	}

	tail := mustTail(t, registry, TailRequest{Source: "journal", Lines: 5})
	if tail.Count == 0 {
		t.Skip("journal 无条目，跳过 tail/search 断言")
	}
	if tail.Count > 5 || tail.File != "journal" {
		t.Fatalf("tail=%+v", tail)
	}
	for _, entry := range tail.Entries {
		if entry.Text == "" {
			t.Fatalf("journal 条目为空: %+v", entry)
		}
	}

	token := journalToken(tail.Entries[tail.Count-1].Text)
	if token == "" {
		t.Skip("无法从 journal 行提取关键词")
	}
	search := mustSearch(t, registry, SearchRequest{Source: "journal", Keyword: token, Limit: 5})
	if search.Count == 0 {
		t.Fatalf("journal search keyword=%q 无命中", token)
	}
	if search.Matches[0].LineNo < 1 || search.Matches[0].Text == "" {
		t.Fatalf("matches=%+v", search.Matches)
	}
}
