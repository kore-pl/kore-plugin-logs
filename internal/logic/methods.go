package logic

import (
	"fmt"
	"strings"
)

// List 实现 logs.list：按来源分流统计，返回固定样例的来源概况。
func List(f Fault) (*ListResult, *FaultError) {
	if ferr := checkFault(f); ferr != nil {
		return nil, ferr
	}
	sources := make([]SourceInfo, 0, len(allSources))
	for _, s := range allSources {
		n := 0
		for _, e := range samples {
			if e.Source == s {
				n++
			}
		}
		sources = append(sources, SourceInfo{Source: s, Count: n})
	}
	return &ListResult{Sources: sources, Count: len(samples), Note: "skeleton: sample entries, no host log read"}, nil
}

// Query 实现 logs.query：source/keyword/limit 校验 + 分流 + 过滤 + 限量。
func Query(req QueryRequest) (*QueryResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	// 空 source 归一为 all（不传即查全部），显式非法值仍由 validateQuery 拒绝。
	if req.Source == "" {
		req.Source = SourceAll
	}
	if perr := validateQuery(req); perr != nil {
		return nil, perr
	}
	limit := req.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	keyword := strings.TrimSpace(req.Keyword)
	matched := make([]LogEntry, 0, len(samples))
	for _, e := range samples {
		if req.Source != SourceAll && e.Source != req.Source {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(e.Message), strings.ToLower(keyword)) {
			continue
		}
		matched = append(matched, e)
	}
	truncated := len(matched) > limit
	if truncated {
		matched = matched[:limit]
	}
	if matched == nil {
		matched = []LogEntry{}
	}
	return &QueryResult{
		Source: req.Source, Keyword: keyword, Limit: limit,
		Count: len(matched), Truncated: truncated, Entries: matched,
	}, nil
}

// validateQuery 校验显式入参：source 必须在枚举内、limit∈[0,1000]、keyword 长度上限。
func validateQuery(req QueryRequest) *FaultError {
	if !validSource(req.Source) {
		return &FaultError{Code: "E_INVALID", Message: fmt.Sprintf("source 非法：%q", req.Source)}
	}
	if req.Limit < 0 {
		return &FaultError{Code: "E_INVALID", Message: "limit 不能为负数"}
	}
	if req.Limit > MaxLimit {
		return &FaultError{Code: "E_INVALID", Message: fmt.Sprintf("limit=%d 超过上限 %d（防 DoS）", req.Limit, MaxLimit)}
	}
	if len(req.Keyword) > 256 {
		return &FaultError{Code: "E_INVALID", Message: "keyword 超过 256 字符"}
	}
	return nil
}
