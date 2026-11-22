package main

import (
	"encoding/json"
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
func dispatch(request *pluginapi.Request, cfg config) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
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
	result, perr := handleLogsVerb(verb, request.Params, cfg)
	if perr != nil {
		response.Error = perr
		return response, false
	}
	response.Result = result
	return response, false
}

// handleLogsVerb 执行 logs 方法：返回结构化结果 JSON 或协议错误体。
// logic 层的 FaultError 在此统一映射为 pluginapi.Error。
func handleLogsVerb(verb string, params json.RawMessage, cfg config) (json.RawMessage, *pluginapi.Error) {
	f := logic.ParseFault(cfg.fault)
	switch verb {
	case "list":
		result, ferr := logic.List(f)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.list")
	case "query":
		req, perr := parseQueryParams(params)
		if perr != nil {
			return nil, perr
		}
		req.Fault = f
		result, ferr := logic.Query(req)
		if ferr != nil {
			return nil, fromFault(ferr)
		}
		return encodeResult(result, "logs.query")
	default:
		return nil, notFound(verb)
	}
}

// encodeResult 序列化 logic 层结果为响应载荷。
func encodeResult(v any, method string) (json.RawMessage, *pluginapi.Error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化 " + method + " 结果失败: " + err.Error())
	}
	return b, nil
}

// parseQueryParams 校验 logs.query 入参为 JSON 对象并解析显式字段。
func parseQueryParams(raw json.RawMessage) (logic.QueryRequest, *pluginapi.Error) {
	var req logic.QueryRequest
	if len(raw) == 0 || string(raw) == "null" {
		return req, nil
	}
	if raw[0] != '{' {
		return req, invalidParam("params 必须是 JSON 对象")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, invalidParam("params 解析失败: " + err.Error())
	}
	return req, nil
}

// businessVerb 把 <id>.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method, id string) (string, bool) {
	verb := strings.TrimPrefix(method, id+".")
	switch verb {
	case "list", "query":
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
