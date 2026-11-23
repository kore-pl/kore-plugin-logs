package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aggregateRegistry 构造两条文件来源（app / api），用于多源聚合断言。
func aggregateRegistry(t *testing.T, appLines, apiLines string) *Registry {
	t.Helper()
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.log")
	apiPath := filepath.Join(dir, "api.log")
	if err := os.WriteFile(appPath, []byte(appLines), 0o600); err != nil {
		t.Fatalf("写 app.log 失败: %v", err)
	}
	if err := os.WriteFile(apiPath, []byte(apiLines), 0o600); err != nil {
		t.Fatalf("写 api.log 失败: %v", err)
	}
	return NewRegistry([]SourceSpec{
		{Name: "app", Path: appPath, Type: SourceTypeFile},
		{Name: "api", Path: apiPath, Type: SourceTypeFile},
	})
}

// TestAggregateGroupsBySource 覆盖跨来源聚合与按来源分组（每条带 source 归属）。
func TestAggregateGroupsBySource(t *testing.T) {
	registry := aggregateRegistry(t,
		"2026-11-23T10:00:00Z app hit one\n2026-11-23T10:00:01Z app miss\n",
		"2026-11-23T10:00:02Z api hit two\n")
	result, ferr := registry.Aggregate(AggregateRequest{Keyword: "hit", Limit: 10})
	if ferr != nil {
		t.Fatalf("Aggregate 意外错误: %v", ferr)
	}
	if len(result.Groups) != 2 || len(result.Sources) != 2 {
		t.Fatalf("分组数=%d sources=%v，期望 2 组", len(result.Groups), result.Sources)
	}
	if result.Count != 2 || result.Errors != 0 {
		t.Fatalf("count=%d errors=%d，期望 2/0", result.Count, result.Errors)
	}
	app, api := result.Groups[0], result.Groups[1]
	if app.Source != "app" || api.Source != "api" {
		t.Fatalf("分组顺序/归属不符: %s %s", app.Source, api.Source)
	}
	if app.Count != 1 || api.Count != 1 || len(app.Entries) != 1 || len(api.Entries) != 1 {
		t.Fatalf("分组条目数不符: app=%d api=%d", app.Count, api.Count)
	}
	if app.Entries[0].Source != "app" || !strings.Contains(app.Entries[0].Text, "app hit one") {
		t.Fatalf("app 条目归属/内容不符: %+v", app.Entries[0])
	}
	if api.Entries[0].Source != "api" || !strings.Contains(api.Entries[0].Text, "api hit two") {
		t.Fatalf("api 条目归属/内容不符: %+v", api.Entries[0])
	}
	if app.Entries[0].Time == "" {
		t.Fatalf("行首 RFC3339 时间未解析: %+v", app.Entries[0])
	}
}

// TestAggregateLimitTruncates 覆盖全局 limit 截断：先到先得，后续来源空组并标记截断。
func TestAggregateLimitTruncates(t *testing.T) {
	registry := aggregateRegistry(t,
		makeLines(4),
		makeLines(4),
	)
	result, ferr := registry.Aggregate(AggregateRequest{Limit: 3})
	if ferr != nil {
		t.Fatalf("Aggregate 意外错误: %v", ferr)
	}
	if result.Count != 3 || !result.Truncated || result.Limit != 3 {
		t.Fatalf("count=%d truncated=%v limit=%d，期望 3/true/3", result.Count, result.Truncated, result.Limit)
	}
	if len(result.Groups) != 2 {
		t.Fatalf("分组数=%d，期望 2", len(result.Groups))
	}
	if result.Groups[0].Count != 3 || len(result.Groups[0].Entries) != 3 {
		t.Fatalf("首组条目数=%d，期望 3", result.Groups[0].Count)
	}
	if len(result.Groups[1].Entries) != 0 || !result.Groups[1].Truncated {
		t.Fatalf("额度耗尽后第二组应为空且标记截断: %+v", result.Groups[1])
	}
}

// TestAggregateExplicitSourcesAndFilters 覆盖显式 sources 子集、去重与 keyword 过滤。
func TestAggregateExplicitSourcesAndFilters(t *testing.T) {
	registry := aggregateRegistry(t,
		"app keep\napp drop\n",
		"api keep\n",
	)
	result, ferr := registry.Aggregate(AggregateRequest{Sources: []string{"api", "api"}, Keyword: "keep", Limit: 5})
	if ferr != nil {
		t.Fatalf("Aggregate 意外错误: %v", ferr)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "api" {
		t.Fatalf("显式 sources 去重/保序不符: %v", result.Sources)
	}
	if len(result.Groups) != 1 || result.Groups[0].Count != 1 {
		t.Fatalf("单来源聚合不符: %+v", result.Groups)
	}
}

// TestAggregateRejectsBadParams 覆盖未登记来源、非法 limit 与非法 since。
func TestAggregateRejectsBadParams(t *testing.T) {
	registry := aggregateRegistry(t, "x\n", "y\n")
	cases := []struct {
		name string
		req  AggregateRequest
		code string
	}{
		{"未登记来源", AggregateRequest{Sources: []string{"nope"}}, CodeInvalid},
		{"limit 超上限", AggregateRequest{Limit: MaxTailLines + 1}, CodeInvalid},
		{"limit 为负", AggregateRequest{Limit: -1}, CodeInvalid},
		{"since 非法", AggregateRequest{Since: "2026-11-23"}, CodeInvalid},
		{"keyword 超长", AggregateRequest{Keyword: strings.Repeat("x", MaxKeywordLen+1)}, CodeInvalid},
	}
	for _, c := range cases {
		if _, ferr := registry.Aggregate(c.req); ferr == nil || ferr.Code != c.code {
			t.Fatalf("%s 错误=%v，期望 %s", c.name, ferr, c.code)
		}
	}
}
