// Package logic 实现 logs 插件的方法级业务逻辑：来源白名单、文件尾部读取、
// 全文件分块搜索、统计与 systemd journal 读取。
//
// 安全口径：所有读取只按配置登记（KORE_LOGS_SOURCES）的来源名解析，不接受外部路径；
// 路径必须是 Clean 后的绝对路径，父目录在装载时固定解析结果、读取时重新校验，
// 最终文件不得是符号链接。单次扫描上限 64 MiB，单行上限 8 KiB，不整文件读入内存。
package logic

import (
	"strconv"
	"time"
)

// SourceType 是来源类型：常规日志文件或 systemd journal。
type SourceType string

const (
	SourceTypeFile    SourceType = "file"
	SourceTypeJournal SourceType = "journal"
)

// SourceSpec 是配置登记的一条日志来源（KORE_LOGS_SOURCES 数组元素）。
type SourceSpec struct {
	Name string     `json:"name"`
	Path string     `json:"path"`
	Type SourceType `json:"type"`
}

// SourceStatus 是 logs.sources 返回的单条探测结果；只含 name + 文件名，不含宿主绝对路径。
type SourceStatus struct {
	Name       string     `json:"name"`
	File       string     `json:"file"`
	Type       SourceType `json:"type"`
	Exists     bool       `json:"exists"`
	Readable   bool       `json:"readable"`
	SizeBytes  int64      `json:"size_bytes"`
	ModifiedAt string     `json:"modified_at,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// SourcesResult 是 logs.sources 的结构化结果。
type SourcesResult struct {
	Sources []SourceStatus `json:"sources"`
	Count   int            `json:"count"`
}

// Debugf 是可选调试日志钩子（完整路径只写 DEBUG 日志，不进响应）。
type Debugf func(format string, args ...any)

// TailRequest 是 logs.tail 的显式入参。Fault/Debugf 只由运行期注入。
// Position 是上一次响应回传的文件字节偏移：> 0 时走增量前向读取（只回新行），
// 仅对 file 来源生效；journal 无稳定偏移，忽略该字段。
type TailRequest struct {
	Source   string `json:"source"`
	Lines    int    `json:"lines"`
	Keyword  string `json:"keyword"`
	Since    string `json:"since"`
	Position int64  `json:"position"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

// TailLine 是尾部读取的单行；超 8 KiB 的行截断并置 Truncated。
// Time 是行首时间戳的 RFC3339 规范形（解析失败为空串）；Level 是行首区域解析的级别（error/warn/info/debug，未知为空串）。
type TailLine struct {
	Text      string `json:"text"`
	Time      string `json:"time,omitempty"`
	Level     string `json:"level,omitempty"`
	Truncated bool   `json:"truncated"`
}

// TailResult 是 logs.tail 的结构化结果。Position 是下次增量拉取的字节偏移
// （file 来源为当前文件内「最后一条完整行」之后的偏移；journal 恒为 0）。
type TailResult struct {
	Source        string     `json:"source"`
	File          string     `json:"file"`
	Requested     int        `json:"requested"`
	Count         int        `json:"count"`
	Entries       []TailLine `json:"entries"`
	Position      int64      `json:"position"`
	Keyword       string     `json:"keyword,omitempty"`
	Since         string     `json:"since,omitempty"`
	ScannedBytes  int64      `json:"scanned_bytes"`
	Truncated     bool       `json:"truncated"`
	LineTruncated bool       `json:"line_truncated"`
}

// SearchRequest 是 logs.search 的显式入参。Query 是 0.3.0 起的首选字段名；
// 0.2.x 调用方传的 Keyword 继续兼容（Query 优先）。keyword/query 语义为
// ASCII 大小写不敏感的**字面子串**（正则元字符按字面处理，等价 grep -iF）。
type SearchRequest struct {
	Source  string `json:"source"`
	Query   string `json:"query"`
	Keyword string `json:"keyword"`
	Limit   int    `json:"limit"`
	Fault   Fault  `json:"-"`
	Debugf  Debugf `json:"-"`
}

// SearchMatch 是一条命中：LineNo 为 1 起行号，Text 为行内容（超长截断）。
// Time 是行首时间戳的 RFC3339 规范形（解析失败为空串）；Level 是行首区域解析的级别。
type SearchMatch struct {
	LineNo    int    `json:"line_no"`
	Text      string `json:"text"`
	Time      string `json:"time,omitempty"`
	Level     string `json:"level,omitempty"`
	Truncated bool   `json:"truncated"`
}

// SearchResult 是 logs.search 的结构化结果。Total 是全文命中总行数（可能大于
// len(Matches)，此时 Truncated=true）；Matches 只保留前 Limit 条。
type SearchResult struct {
	Source       string        `json:"source"`
	File         string        `json:"file"`
	Keyword      string        `json:"keyword"`
	Limit        int           `json:"limit"`
	Count        int           `json:"count"`
	Total        int           `json:"total"`
	Matches      []SearchMatch `json:"matches"`
	ScannedBytes int64         `json:"scanned_bytes"`
	Truncated    bool          `json:"truncated"`
	Note         string        `json:"note,omitempty"`
}

// DownloadRequest 是 logs.download 的显式入参。StateDir 由运行期注入（KORE_STATE_DIR）。
type DownloadRequest struct {
	Source   string `json:"source"`
	Lines    int    `json:"lines"`
	StateDir string `json:"-"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

// DownloadResult 是 logs.download 的结果：文件落在 <state>/exports/<source>-<ts>.log，
// Path 为落盘绝对路径；DataBase64 与 Filename 供前端直接触发浏览器下载
// （沿 files 插件 file.download 的 base64 通道，无需内核新增静态路由）。
type DownloadResult struct {
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"size_bytes"`
	Lines      int    `json:"lines"`
	DataBase64 string `json:"data_base64"`
	Truncated  bool   `json:"truncated"`
	Note       string `json:"note,omitempty"`
}

// StatsRequest 是 logs.stats 的显式入参。
type StatsRequest struct {
	Source string `json:"source"`
	Fault  Fault  `json:"-"`
	Debugf Debugf `json:"-"`
}

// StatsResult 是 logs.stats 的结构化结果；Lines 为 nil 表示未统计（JSON null）。
// Path 是展示用路径（"<source>/<文件名>"），宿主绝对路径只写 DEBUG 日志。
type StatsResult struct {
	Name       string     `json:"name"`
	File       string     `json:"file"`
	Path       string     `json:"path"`
	Type       SourceType `json:"type"`
	SizeBytes  int64      `json:"size_bytes"`
	Lines      *int64     `json:"lines"`
	ModifiedAt string     `json:"modified_at,omitempty"`
	Readable   bool       `json:"readable"`
	Note       string     `json:"note,omitempty"`
}

// 方法级上限与默认值。
const (
	DefaultTailLines   = 100
	MaxTailLines       = 500
	DefaultSearchLimit = 100
	MaxSearchLimit     = 1000
	MaxKeywordLen      = 256

	// DefaultDownloadLines / MaxDownloadLines 是 logs.download 的行数默认与上限。
	DefaultDownloadLines = 1000
	MaxDownloadLines     = 10000
	// MaxDownloadBytes 是导出文件的体积硬上限（5 MiB），达到即停止写入并标记截断。
	MaxDownloadBytes int64 = 5 << 20
	// MaxIncrementalBytes 是单次增量 tail 的前向读取窗口（1 MiB）：
	// 新增内容超过窗口时跳到最近 1 MiB，跳过部分置 truncated。
	MaxIncrementalBytes int64 = 1 << 20

	// MaxScanBytes 是单次 tail/search 反向或正向扫描的硬上限（64 MiB）。
	MaxScanBytes int64 = 64 << 20
	// StatsLineCountMaxBytes 是 stats 统计行数的文件大小上限（64 MiB）。
	StatsLineCountMaxBytes int64 = 64 << 20
	// ReadBlockSize 是分块读取的块大小（64 KiB）。
	ReadBlockSize = 64 << 10
	// MaxLineBytes 是单行内容上限（8 KiB），超出截断并标记。
	MaxLineBytes = 8 << 10
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

// FaultError 是故障/业务错误的统一载体，由主包映射为 pluginapi.Error。
type FaultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *FaultError) Error() string { return e.Code + ": " + e.Message }

// 协议错误码（与 pluginapi.E_* 一一对应，避免 logic 反向依赖主包）。
const (
	CodeInvalid     = "E_INVALID"
	CodeNotFound    = "E_NOTFOUND"
	CodeInternal    = "E_INTERNAL"
	CodeUnsupported = "E_UNSUPPORTED"
	CodeUpstream    = "E_UPSTREAM"
)

// methodErr 构造统一的结构化方法错误。
func methodErr(code, msg string) *FaultError {
	return &FaultError{Code: code, Message: msg}
}

// effectiveTailLines 校验 lines：0 取默认 100，合法区间 1..500。
func effectiveTailLines(lines int) (int, *FaultError) {
	if lines == 0 {
		return DefaultTailLines, nil
	}
	if lines < 1 || lines > MaxTailLines {
		return 0, methodErr(CodeInvalid, "lines 必须在 1 到 "+strconv.Itoa(MaxTailLines)+" 之间")
	}
	return lines, nil
}

// effectiveSearchLimit 校验 limit：0 取默认 100，合法区间 1..1000。
func effectiveSearchLimit(limit int) (int, *FaultError) {
	if limit == 0 {
		return DefaultSearchLimit, nil
	}
	if limit < 1 || limit > MaxSearchLimit {
		return 0, methodErr(CodeInvalid, "limit 必须在 1 到 1000 之间")
	}
	return limit, nil
}

// effectiveDownloadLines 校验导出行数：0 取默认 1000，合法区间 1..10000。
func effectiveDownloadLines(lines int) (int, *FaultError) {
	if lines == 0 {
		return DefaultDownloadLines, nil
	}
	if lines < 1 || lines > MaxDownloadLines {
		return 0, methodErr(CodeInvalid, "lines 必须在 1 到 "+strconv.Itoa(MaxDownloadLines)+" 之间")
	}
	return lines, nil
}

// checkSourceParam 校验 source 名称非空。
func checkSourceParam(source string) *FaultError {
	if source == "" {
		return methodErr(CodeInvalid, "source 必填")
	}
	return nil
}

// checkKeywordLen 校验 keyword 长度上限。
func checkKeywordLen(keyword string) *FaultError {
	if len(keyword) > MaxKeywordLen {
		return methodErr(CodeInvalid, "keyword 超过 256 字节上限")
	}
	return nil
}

// parseSince 校验 RFC3339 时间；空串返回 (零值,false,nil)。
func parseSince(raw string) (time.Time, bool, *FaultError) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, methodErr(CodeInvalid, "since 必须是 RFC3339 时间，例如 2026-11-22T10:00:00Z")
	}
	return ts, true, nil
}

// debugf 在钩子存在时输出调试日志。
func debugf(hook Debugf, format string, args ...any) {
	if hook != nil {
		hook(format, args...)
	}
}
