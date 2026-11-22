// Package logic 实现 logs 插件的方法级业务逻辑：按来源分流 + 关键字过滤。
//
// 骨架口径：本包只操作内存中的固定样例条目，绝不读取宿主日志文件、
// 不打开任何文件路径；真实日志源接入属于后续版本，不在本仓范围。
package logic

// Source 是日志来源的显式枚举，不用 object/any 逃逸（G-24）。
type Source string

const (
	SourceAll    Source = "all"
	SourceSystem Source = "system"
	SourceKernel Source = "kernel"
	SourceApp    Source = "app"
	SourceAccess Source = "access"
	SourceError  Source = "error"
)

// Fault 是故障注入模式，取环境变量 KORE_FAULT；空值表示不注入。
type Fault string

const (
	FaultNone     Fault = ""
	FaultKill     Fault = "kill"       // kill -9：进程被强杀
	FaultTimeout  Fault = "timeout"    // 方法执行超时
	FaultDiskFull Fault = "disk-full"  // 磁盘满 ENOSPC
	FaultCgroup   Fault = "cgroup-oom" // cgroup memory.max 超限 OOM
)

// FaultError 是故障下的结构化方法级错误，由主包映射为 pluginapi.Error。
type FaultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *FaultError) Error() string { return e.Code + ": " + e.Message }

// SourceInfo 是 logs.list 返回的单一来源概况。
type SourceInfo struct {
	Source Source `json:"source"`
	Count  int    `json:"count"`
}

// ListResult 是 logs.list 的结构化结果。
type ListResult struct {
	Sources []SourceInfo `json:"sources"`
	Count   int          `json:"count"`
	Note    string       `json:"note"`
}

// LogEntry 是单条样例日志（骨架口径，不来自宿主文件）。
type LogEntry struct {
	Source  Source `json:"source"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// QueryRequest 是 logs.query 的显式入参。Fault 不来自外部 JSON，
// 仅由运行期环境变量注入，故 json 标签为 "-"。
type QueryRequest struct {
	Source  Source `json:"source"`
	Keyword string `json:"keyword"`
	Limit   int    `json:"limit"`
	Fault   Fault  `json:"-"`
}

// QueryResult 是 logs.query 的结构化结果。
type QueryResult struct {
	Source    Source     `json:"source"`
	Keyword   string     `json:"keyword"`
	Limit     int        `json:"limit"`
	Count     int        `json:"count"`
	Truncated bool       `json:"truncated"`
	Entries   []LogEntry `json:"entries"`
}

// DefaultLimit 是 limit 未指定（0）时的默认值。
const DefaultLimit = 100

// MaxLimit 是 limit 硬上限，防止一次查询返回过多条目造成 DoS。
const MaxLimit = 1000
