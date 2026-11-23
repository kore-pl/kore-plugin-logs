package logic

// AggregateRequest 是 logs.aggregate 的显式入参：跨多个已登记来源做同一条件的
// 尾部读取并按来源分组。Sources 为空表示全部来源（按配置顺序）；Keyword/Since
// 与 tail 同口径（字面子串 + RFC3339 时间过滤）；Limit 是**全局**命中上限
// （默认 100，1..500），同时也作为单来源的读取行数上限。
type AggregateRequest struct {
	Sources []string
	Keyword string
	Since   string
	Limit   int
	Fault   Fault
	Debugf  Debugf
}

// AggregateEntry 是一条聚合命中；Source 标明归属来源（跨来源混合列表时可直接定位）。
type AggregateEntry struct {
	Source    string `json:"source"`
	Text      string `json:"text"`
	Time      string `json:"time,omitempty"`
	Level     string `json:"level,omitempty"`
	Truncated bool   `json:"truncated"`
}

// AggregateGroup 是按来源分组的命中列表；Error 非空表示该来源读取失败（不影响其它来源）。
type AggregateGroup struct {
	Source    string           `json:"source"`
	Type      SourceType       `json:"type"`
	File      string           `json:"file"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
	Error     string           `json:"error,omitempty"`
	Entries   []AggregateEntry `json:"entries"`
}

// AggregateResult 是 logs.aggregate 的结构化结果。
// Count 是实际返回的命中总数（≤ Limit）；Truncated 表示命中可能被 Limit 截断。
type AggregateResult struct {
	Sources   []string         `json:"sources"`
	Keyword   string           `json:"keyword,omitempty"`
	Since     string           `json:"since,omitempty"`
	Limit     int              `json:"limit"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
	Errors    int              `json:"errors"`
	Groups    []AggregateGroup `json:"groups"`
}

// Aggregate 实现 logs.aggregate：复用 tail 的分块读取与 keyword/since 过滤，
// 逐来源读取后按配置顺序分组；全局 Limit 用完后其余来源只保留空分组（标记截断）。
// 只做调用时读取，不起 ticker / 后台 goroutine（G-19）。
func (r *Registry) Aggregate(req AggregateRequest) (*AggregateResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	limit, perr := effectiveTailLines(req.Limit)
	if perr != nil {
		return nil, perr
	}
	if perr := checkKeywordLen(req.Keyword); perr != nil {
		return nil, perr
	}
	if _, _, perr := parseSince(req.Since); perr != nil {
		return nil, perr
	}
	names, perr := r.aggregateNames(req.Sources)
	if perr != nil {
		return nil, perr
	}
	result := &AggregateResult{
		Sources: names, Keyword: req.Keyword, Since: req.Since, Limit: limit,
		Groups: make([]AggregateGroup, 0, len(names)),
	}
	remaining := limit
	for _, name := range names {
		group := r.aggregateOne(name, remaining, req)
		remaining -= len(group.Entries)
		if group.Error != "" {
			result.Errors++
		}
		if len(group.Entries) > 0 {
			result.Count += len(group.Entries)
		}
		if remaining <= 0 {
			result.Truncated = true
		}
		result.Groups = append(result.Groups, group)
	}
	return result, nil
}

// aggregateNames 解析目标来源列表：空表示全部；显式列表去重保序，未登记来源回 E_INVALID。
func (r *Registry) aggregateNames(requested []string) ([]string, *FaultError) {
	if len(requested) == 0 {
		names := make([]string, len(r.order))
		copy(names, r.order)
		return names, nil
	}
	seen := make(map[string]struct{}, len(requested))
	names := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, dup := seen[name]; dup {
			continue
		}
		if _, perr := r.lookup(name); perr != nil {
			return nil, perr
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

// aggregateOne 读取单个来源并按剩余全局额度截取；读取失败写入 group.Error。
func (r *Registry) aggregateOne(name string, remaining int, req AggregateRequest) AggregateGroup {
	entry, perr := r.lookup(name)
	if perr != nil {
		return AggregateGroup{Source: name, Error: perr.Message, Entries: []AggregateEntry{}}
	}
	group := AggregateGroup{Source: name, Type: entry.spec.Type, Entries: []AggregateEntry{}}
	if remaining <= 0 {
		group.Truncated = true
		return group
	}
	tail, ferr := r.Tail(TailRequest{
		Source: name, Lines: remaining, Keyword: req.Keyword, Since: req.Since,
		Fault: req.Fault, Debugf: req.Debugf,
	})
	if ferr != nil {
		group.Error = ferr.Message
		return group
	}
	group.File = tail.File
	group.Truncated = tail.Truncated
	for _, line := range tail.Entries {
		group.Entries = append(group.Entries, AggregateEntry{
			Source: name, Text: line.Text, Time: line.Time, Level: line.Level, Truncated: line.Truncated,
		})
	}
	group.Count = len(group.Entries)
	return group
}
