package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
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
func dispatch(request *pluginapi.Request, cfg config, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，优雅退出")
		return response, true
	case "kore.ping":
		response.Result = pingResult(cfg)
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
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
func handleLogsVerb(verb string, params json.RawMessage, cfg config, logger *slog.Logger) (json.RawMessage, *pluginapi.Error) {
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
	default:
		return nil, notFound(verb)
	}
}

// tailParams 用指针区分“未传 lines”和“显式 lines=0”。
type tailParams struct {
	Source  string `json:"source"`
	Lines   *int   `json:"lines"`
	Keyword string `json:"keyword"`
	Since   string `json:"since"`
}

type searchParams struct {
	Source  string `json:"source"`
	Keyword string `json:"keyword"`
	Limit   *int   `json:"limit"`
}

type statsParams struct {
	Source string `json:"source"`
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
	return req, nil
}

// parseSearchParams 校验入参为 JSON 对象并映射为 logic.SearchRequest。
func parseSearchParams(raw json.RawMessage, fault logic.Fault) (logic.SearchRequest, *pluginapi.Error) {
	var p searchParams
	if perr := decodeObject(raw, &p); perr != nil {
		return logic.SearchRequest{}, perr
	}
	req := logic.SearchRequest{Source: p.Source, Keyword: p.Keyword, Fault: fault}
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

// businessVerb 把 <id>.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method, id string) (string, bool) {
	verb := strings.TrimPrefix(method, id+".")
	switch verb {
	case "sources", "tail", "search", "stats":
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
