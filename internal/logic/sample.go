package logic

// samples 是骨架口径下的固定样例条目集合；按 Source 分流、按 Message 过滤。
// 顺序固定，保证查询结果确定性与可复现。
var samples = []LogEntry{
	{Source: SourceSystem, Time: "2026-11-22T10:00:00Z", Level: "INFO", Message: "kored started"},
	{Source: SourceSystem, Time: "2026-11-22T10:00:01Z", Level: "WARN", Message: "config reload slow"},
	{Source: SourceKernel, Time: "2026-11-22T10:00:02Z", Level: "INFO", Message: "cgroup v2 mounted"},
	{Source: SourceKernel, Time: "2026-11-22T10:00:03Z", Level: "ERROR", Message: "oom killer invoked"},
	{Source: SourceApp, Time: "2026-11-22T10:00:04Z", Level: "INFO", Message: "plugin logs handshake ok"},
	{Source: SourceApp, Time: "2026-11-22T10:00:05Z", Level: "ERROR", Message: "upstream timeout on cache"},
	{Source: SourceAccess, Time: "2026-11-22T10:00:06Z", Level: "INFO", Message: "GET /api/v1/health 200"},
	{Source: SourceAccess, Time: "2026-11-22T10:00:07Z", Level: "WARN", Message: "GET /api/v1/logs 401"},
	{Source: SourceError, Time: "2026-11-22T10:00:08Z", Level: "ERROR", Message: "disk ENOSPC on /var/log"},
	{Source: SourceError, Time: "2026-11-22T10:00:09Z", Level: "ERROR", Message: "kill signal received"},
}

// allSources 是合法的来源枚举顺序（all 不参与分流统计）。
var allSources = []Source{SourceSystem, SourceKernel, SourceApp, SourceAccess, SourceError}

// validSource 判断来源是否在枚举内（含 all）。
func validSource(s Source) bool {
	if s == SourceAll {
		return true
	}
	for _, v := range allSources {
		if s == v {
			return true
		}
	}
	return false
}
