package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
	"github.com/pengtang1024/kore-plugin-logs/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// statusResult 是 kore.ping 的结果（PLUGIN-PROTOCOL §7.1）。
type statusResult struct {
	Status     string `json:"status"`
	UptimeS    int64  `json:"uptime_s"`
	Goroutines int    `json:"goroutines"`
	RSSKB      int64  `json:"rss_kb"`
}

// dispatch 把请求分发为响应帧；第二个返回值表示收到 kore.shutdown。
func dispatch(request *pluginapi.Request, cfg *config, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，优雅退出")
		return response, true
	case "kore.ping":
		response.Result = pingResult(*cfg)
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
		return response, false
	case cfg.id + ".config.get", "config.get":
		result, perr := handleConfigGet(cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	case cfg.id + ".config.set", "config.set":
		result, perr := handleConfigSet(request.Params, cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	}
	verb, ok := businessVerb(request.Method, cfg.id)
	if !ok {
		response.Error = notFound(request.Method)
		return response, false
	}
	result, perr := handleLogsVerb(verb, request.Params, cfg, logger)
	if perr != nil {
		response.Error = perr
		response.Result = nil
		return response, false
	}
	response.Result = result
	return response, false
}

// handleLogsVerb 执行 logs 方法：返回结构化结果 JSON 或协议错误体。
// logic 层的 FaultError 在此统一映射为 pluginapi.Error。
func handleLogsVerb(verb string, params json.RawMessage, cfg *config, logger *slog.Logger) (json.RawMessage, *pluginapi.Error) {
	f := logic.ParseFault(cfg.fault)
	registry := cfg.registry
	if registry == nil {
		registry = logic.NewRegistry(nil)
	}
	switch verb {
	case "sources":
		result, ferr := registry.Sources(f)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.sources")
	case "tail":
		req, perr := parseTailParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Tail(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.tail")
	case "search":
		req, perr := parseSearchParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Search(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.search")
	case "stats":
		req, perr := parseStatsParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Stats(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.stats")
	case "download":
		req, perr := parseDownloadParams(params, f, cfg.stateDir)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Download(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.download")
	case "follow":
		req, perr := parseFollowParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Follow(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.follow")
	case "rotate":
		req, perr := parseRotateParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Rotate(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.rotate")
	case "aggregate":
		req, perr := parseAggregateParams(params, f)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Aggregate(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.aggregate")
	case "retention.get":
		result, ferr := registry.RetentionGet(logic.RetentionGetRequest{StateDir: cfg.stateDir, Fault: f})
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.retention.get")
	case "retention.set":
		req, perr := parseRetentionSetParams(params, f, cfg.stateDir)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.RetentionSet(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.retention.set")
	case "usage":
		result, ferr := registry.Usage(logic.UsageRequest{StateDir: cfg.stateDir, Fault: f})
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.usage")
	case "clean":
		req, perr := parseCleanParams(params, f, cfg.stateDir)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Clean(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.clean")
	case "clean.restore":
		req, perr := parseRestoreParams(params, f, cfg.stateDir)
		if perr != nil {
			return nil, perr
		}
		req.Debugf = debugLogger(logger)
		result, ferr := registry.Restore(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.clean.restore")
	default:
		return nil, notFound(verb)
	}
}

// tailParams 用指针区分“未传 lines”和“显式 lines=0”。
type tailParams struct {
	Source   string `json:"source"`
	Lines    *int   `json:"lines"`
	Keyword  string `json:"keyword"`
	Since    string `json:"since"`
	Position *int64 `json:"position"`
}

type searchParams struct {
	Source  string `json:"source"`
	Query   string `json:"query"`
	Keyword string `json:"keyword"`
	Limit   *int   `json:"limit"`
}

type statsParams struct {
	Source string `json:"source"`
}

type downloadParams struct {
	Source   string `json:"source"`
	Lines    *int   `json:"lines"`
	FromLine *int64 `json:"from_line"`
	ToLine   *int64 `json:"to_line"`
	ID       string `json:"id"`
	Offset   *int64 `json:"offset"`
	Limit    *int64 `json:"limit"`
}

type followParams struct {
	Source   string `json:"source"`
	Cursor   string `json:"cursor"`
	MaxLines *int   `json:"max_lines"`
	MaxBytes *int64 `json:"max_bytes"`
	Keyword  string `json:"keyword"`
}

type rotateParams struct {
	Source  string `json:"source"`
	Confirm *bool  `json:"confirm"`
	Keep    *int   `json:"keep"`
}

type retentionSetParams struct {
	Source     string `json:"source"`
	MaxAgeDays *int   `json:"max_age_days"`
	MaxSizeMB  *int   `json:"max_size_mb"`
	MaxFiles   *int   `json:"max_files"`
}

type cleanParams struct {
	Source  string `json:"source"`
	DryRun  *bool  `json:"dry_run"`
	Confirm *bool  `json:"confirm"`
}

type restoreParams struct {
	Source string `json:"source"`
}

// aggregateParams 是 logs.aggregate 的入参：sources 为空表示全部来源（按配置顺序）。
type aggregateParams struct {
	Sources []string `json:"sources"`
	Keyword string   `json:"keyword"`
	Since   string   `json:"since"`
	Limit   *int     `json:"limit"`
}

// parseTailParams 校验入参为 JSON 对象并映射为 logic.TailRequest。
func parseTailParams(raw json.RawMessage, fault logic.Fault) (logic.TailRequest, *pluginapi.Error) {
	var p tailParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.TailRequest{}, perr
	}
	req := logic.TailRequest{Source: p.Source, Keyword: p.Keyword, Since: p.Since, Fault: fault}
	if p.Lines != nil {
		req.Lines = *p.Lines
	}
	if p.Position != nil {
		req.Position = *p.Position
	}
	return req, nil
}

// parseSearchParams 校验入参为 JSON 对象并映射为 logic.SearchRequest。
// query（0.3.0 起）与 keyword（0.2.x 兼容）都接受，query 优先。
func parseSearchParams(raw json.RawMessage, fault logic.Fault) (logic.SearchRequest, *pluginapi.Error) {
	var p searchParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.SearchRequest{}, perr
	}
	req := logic.SearchRequest{Source: p.Source, Query: p.Query, Keyword: p.Keyword, Fault: fault}
	if p.Limit != nil {
		req.Limit = *p.Limit
	}
	return req, nil
}

// parseStatsParams 校验入参为 JSON 对象并映射为 logic.StatsRequest。
func parseStatsParams(raw json.RawMessage, fault logic.Fault) (logic.StatsRequest, *pluginapi.Error) {
	var p statsParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.StatsRequest{}, perr
	}
	return logic.StatsRequest{Source: p.Source, Fault: fault}, nil
}

// parseDownloadParams 校验入参为 JSON 对象并映射为 logic.DownloadRequest（stateDir 运行期注入）。
// 显式给了 id / offset / limit 时进入分块模式；否则保持 0.3.0 的「整份 inline」默认语义。
func parseDownloadParams(raw json.RawMessage, fault logic.Fault, stateDir string) (logic.DownloadRequest, *pluginapi.Error) {
	var p downloadParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.DownloadRequest{}, perr
	}
	req := logic.DownloadRequest{Source: p.Source, ID: p.ID, StateDir: stateDir, Fault: fault}
	if p.Lines != nil {
		req.Lines = *p.Lines
	}
	if p.FromLine != nil {
		req.FromLine = *p.FromLine
	}
	if p.ToLine != nil {
		req.ToLine = *p.ToLine
	}
	if p.Offset != nil {
		req.Offset = *p.Offset
	}
	if p.Limit != nil {
		req.Limit = *p.Limit
	}
	req.Chunked = p.ID != "" || p.Offset != nil || p.Limit != nil
	return req, nil
}

// parseFollowParams 校验入参为 JSON 对象并映射为 logic.FollowRequest。
func parseFollowParams(raw json.RawMessage, fault logic.Fault) (logic.FollowRequest, *pluginapi.Error) {
	var p followParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.FollowRequest{}, perr
	}
	req := logic.FollowRequest{Source: p.Source, Cursor: p.Cursor, Keyword: p.Keyword, Fault: fault}
	if p.MaxLines != nil {
		req.MaxLines = *p.MaxLines
	}
	if p.MaxBytes != nil {
		req.MaxBytes = *p.MaxBytes
	}
	return req, nil
}

// parseRotateParams 校验入参为 JSON 对象并映射为 logic.RotateRequest。
// confirm 默认 false：未显式确认的切割请求一律拒绝。
// parseRetentionSetParams 校验入参并映射为 logic.RetentionSetRequest。
func parseRetentionSetParams(raw json.RawMessage, fault logic.Fault, stateDir string) (logic.RetentionSetRequest, *pluginapi.Error) {
	var p retentionSetParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.RetentionSetRequest{}, perr
	}
	return logic.RetentionSetRequest{
		Source: p.Source, MaxAgeDays: p.MaxAgeDays, MaxSizeMB: p.MaxSizeMB, MaxFiles: p.MaxFiles,
		StateDir: stateDir, Fault: fault,
	}, nil
}

// parseCleanParams 校验入参并映射为 logic.CleanRequest；dry_run 缺省为 true（安全优先）。
func parseCleanParams(raw json.RawMessage, fault logic.Fault, stateDir string) (logic.CleanRequest, *pluginapi.Error) {
	var p cleanParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.CleanRequest{}, perr
	}
	req := logic.CleanRequest{Source: p.Source, DryRun: true, StateDir: stateDir, Fault: fault}
	if p.DryRun != nil {
		req.DryRun = *p.DryRun
	}
	if p.Confirm != nil {
		req.Confirm = *p.Confirm
	}
	return req, nil
}

// parseRestoreParams 校验入参并映射为 logic.RestoreRequest。
func parseRestoreParams(raw json.RawMessage, fault logic.Fault, stateDir string) (logic.RestoreRequest, *pluginapi.Error) {
	var p restoreParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.RestoreRequest{}, perr
	}
	return logic.RestoreRequest{Source: p.Source, StateDir: stateDir, Fault: fault}, nil
}

func parseRotateParams(raw json.RawMessage, fault logic.Fault) (logic.RotateRequest, *pluginapi.Error) {
	var p rotateParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.RotateRequest{}, perr
	}
	req := logic.RotateRequest{Source: p.Source, Fault: fault}
	if p.Confirm != nil {
		req.Confirm = *p.Confirm
	}
	if p.Keep != nil {
		req.Keep = *p.Keep
	}
	return req, nil
}

// parseAggregateParams 校验入参为 JSON 对象并映射为 logic.AggregateRequest。
func parseAggregateParams(raw json.RawMessage, fault logic.Fault) (logic.AggregateRequest, *pluginapi.Error) {
	var p aggregateParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.AggregateRequest{}, perr
	}
	req := logic.AggregateRequest{Sources: p.Sources, Keyword: p.Keyword, Since: p.Since, Fault: fault}
	if p.Limit != nil {
		req.Limit = *p.Limit
	}
	return req, nil
}

// debugLogger 把 slog 包装成 logic.Debugf 的格式化签名（slog 不会对首参做格式化）。
func debugLogger(logger *slog.Logger) logic.Debugf {
	return func(format string, args ...any) {
		logger.Debug(fmt.Sprintf(format, args...))
	}
}

// decodeObject 要求 params 是 JSON 对象；缺省/null 留给方法层做必填校验。
func decodeObject(raw json.RawMessage, target any) *pluginapi.Error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] != '{' {
		return invalidParam("params 必须是 JSON 对象")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return invalidParam("params 解析失败: " + err.Error())
	}
	return nil
}

// encodeResult 序列化 logic 层结果为响应载荷。
func encodeResult(v any, method string) (json.RawMessage, *pluginapi.Error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化 " + method + " 结果失败: " + err.Error())
	}
	return b, nil
}

// handleConfigGet 返回表单 schema、当前值与来源；敏感字段经 plugincfg 掩码后不回传原值。
func handleConfigGet(cfg *config) (json.RawMessage, *pluginapi.Error) {
	schema := logSourcesSchema()
	values, err := json.Marshal(cfg.values)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	masked, err := plugincfg.MaskValues(schema, values)
	if err != nil {
		return nil, internalErr("掩码配置值失败: " + err.Error())
	}
	return encodeResult(plugincfg.GetResult{Schema: schema, Values: masked, Source: cfg.source}, "logs.config.get")
}

// handleConfigSet 执行「校验 → 原子写 → 更新内存」：写盘成功后才替换 registry，
// 保证失败时旧配置继续可用，且下一次 logs.* 调用立即使用新来源（无需重启）。
func handleConfigSet(params json.RawMessage, cfg *config) (json.RawMessage, *pluginapi.Error) {
	var p plugincfg.SetParams
	if perr := decodeObject(params, &p); perr != nil {
		return nil, perr
	}
	if len(p.Values) == 0 || string(p.Values) == "null" {
		return nil, invalidParam("values 必须是 JSON 对象")
	}
	if p.Values[0] != '{' {
		return nil, invalidParam("values 必须是 JSON 对象")
	}
	merged := cfg.values
	if err := strictDecode(p.Values, &merged); err != nil {
		return nil, invalidParam("配置值非法: " + err.Error())
	}
	specs, err := parseSourceList(merged.Sources)
	if err != nil {
		return nil, invalidParam("日志来源非法: " + err.Error())
	}
	if cfg.store == nil || cfg.store.Path() == "" {
		return nil, internalErr("KORE_STATE_DIR 未设置，无法保存插件配置")
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	if err := cfg.store.Write(raw); err != nil {
		return nil, internalErr("保存插件配置失败: " + err.Error())
	}
	cfg.values = merged
	cfg.registry = logic.NewRegistry(specs)
	cfg.source = plugincfg.SourceFile
	return encodeResult(plugincfg.SetResult{Saved: true, Applied: true, RestartRequired: false, Warnings: []string{}},
		"logs.config.set")
}

// businessVerb 把 <id>.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method, id string) (string, bool) {
	verb := strings.TrimPrefix(method, id+".")
	switch verb {
	case "sources", "tail", "search", "stats", "download", "follow", "rotate", "aggregate",
		"retention.get", "retention.set", "usage", "clean", "clean.restore":
		return verb, true
	default:
		return "", false
	}
}

// invalidParam / notFound / internalErr 构造协议错误体（§9）。
func invalidParam(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

func notFound(method string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "未知方法：" + method}
}

func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}

// fromFault 把 logic.FaultError 映射为 pluginapi.Error。
func fromFault(e *logic.FaultError) *pluginapi.Error {
	return &pluginapi.Error{Code: e.Code, Message: e.Message}
}

// pingResult 组装 kore.ping 结果。
func pingResult(cfg config) json.RawMessage {
	b, _ := json.Marshal(statusResult{
		Status: "ready", UptimeS: int64(time.Since(cfg.startedAt).Seconds()),
		Goroutines: runtime.NumGoroutine(), RSSKB: rssKB(),
	})
	return b
}
