package logic

import (
	"strings"
	"testing"
)

func TestListSources(t *testing.T) {
	r, ferr := List(FaultNone)
	if ferr != nil {
		t.Fatalf("List 意外故障: %v", ferr)
	}
	if r.Count != len(samples) {
		t.Fatalf("count=%d，期望 %d", r.Count, len(samples))
	}
	if len(r.Sources) != len(allSources) {
		t.Fatalf("sources=%d，期望 %d", len(r.Sources), len(allSources))
	}
	total := 0
	for _, s := range r.Sources {
		total += s.Count
	}
	if total != len(samples) {
		t.Fatalf("分流合计=%d，期望 %d", total, len(samples))
	}
}

func TestQuerySourceRouting(t *testing.T) {
	r, ferr := Query(QueryRequest{Source: SourceKernel, Fault: FaultNone})
	if ferr != nil {
		t.Fatalf("Query 意外故障: %v", ferr)
	}
	if r.Source != SourceKernel || r.Count != 2 {
		t.Fatalf("source=%s count=%d，期望 kernel/2", r.Source, r.Count)
	}
	for _, e := range r.Entries {
		if e.Source != SourceKernel {
			t.Fatalf("条目来源=%s，期望 kernel", e.Source)
		}
	}
}

func TestQueryKeyword(t *testing.T) {
	r, ferr := Query(QueryRequest{Source: SourceAll, Keyword: "OOM", Fault: FaultNone})
	if ferr != nil {
		t.Fatalf("Query 意外故障: %v", ferr)
	}
	if r.Keyword != "OOM" || r.Count != 1 {
		t.Fatalf("keyword=%q count=%d，期望 OOM/1", r.Keyword, r.Count)
	}
	if !strings.Contains(r.Entries[0].Message, "oom") {
		t.Fatalf("条目消息=%q，期望含 oom", r.Entries[0].Message)
	}
}

func TestQueryLimitValidation(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		want  string
	}{
		{"负数", -1, "不能为负数"},
		{"超限", MaxLimit + 1, "超过上限"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ferr := Query(QueryRequest{Source: SourceAll, Limit: tc.limit})
			if ferr == nil || ferr.Code != "E_INVALID" || !strings.Contains(ferr.Message, tc.want) {
				t.Fatalf("ferr=%v，期望 E_INVALID/%s", ferr, tc.want)
			}
		})
	}
}

func TestQueryLimitTruncation(t *testing.T) {
	r, ferr := Query(QueryRequest{Source: SourceAll, Limit: 3})
	if ferr != nil {
		t.Fatalf("Query 意外故障: %v", ferr)
	}
	if r.Count != 3 || r.Limit != 3 || !r.Truncated {
		t.Fatalf("count=%d limit=%d truncated=%v，期望 3/3/true", r.Count, r.Limit, r.Truncated)
	}
	r2, _ := Query(QueryRequest{Source: SourceAll})
	if r2.Limit != DefaultLimit {
		t.Fatalf("默认 limit=%d，期望 %d", r2.Limit, DefaultLimit)
	}
}

func TestQueryInvalidSource(t *testing.T) {
	_, ferr := Query(QueryRequest{Source: "bogus"})
	if ferr == nil || ferr.Code != "E_INVALID" {
		t.Fatalf("ferr=%v，期望 E_INVALID", ferr)
	}
}
