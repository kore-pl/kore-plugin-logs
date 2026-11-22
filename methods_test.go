package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// invoke 发一个请求并读回响应（每个用例独立连接，结束后发 shutdown）。
func invoke(t *testing.T, method string, params json.RawMessage) *pluginapi.Response {
	t.Helper()
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	cfg := testConfig()
	go func() { done <- serve(pluginConn, cfg, testLogger()); _ = pluginConn.Close() }()

	req := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-1", ReqID: "r-1", Method: method, Params: params,
	}
	if err := pluginapi.WriteJSON(kernelConn, req); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	response := readResponse(t, kernelConn)
	if response.ReqID != "r-1" {
		t.Fatalf("req_id=%q，期望回显 r-1", response.ReqID)
	}
	shutdown := &pluginapi.Request{Type: pluginapi.TypeRequest, ID: "id-s", ReqID: "r-s", Method: "kore.shutdown"}
	if err := pluginapi.WriteJSON(kernelConn, shutdown); err != nil {
		t.Fatalf("写 shutdown 失败: %v", err)
	}
	// net.Pipe 无缓冲：必须读走 shutdown 响应，否则 serve 卡在 WriteFrame。
	_ = readResponse(t, kernelConn)
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
	return response
}

func noError(t *testing.T, r *pluginapi.Response) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("意外错误: code=%s message=%s", r.Error.Code, r.Error.Message)
	}
	return string(r.Result)
}

// TestLogsListMethods 覆盖 list 裸名/全名与结构化来源分流结果。
func TestLogsListMethods(t *testing.T) {
	for _, method := range []string{"list", "logs.list"} {
		t.Run(method, func(t *testing.T) {
			got := noError(t, invoke(t, method, nil))
			for _, s := range []string{`"sources"`, `"count"`, `"note"`, `"skeleton: sample entries`} {
				if !strings.Contains(got, s) {
					t.Fatalf("result=%s 缺少 %s", got, s)
				}
			}
			if !strings.Contains(got, `"source":"kernel"`) {
				t.Fatalf("result=%s 未包含 kernel 来源分流", got)
			}
		})
	}
}

// TestLogsQueryRoutingAndFilter 覆盖 query 的分流与关键字过滤。
func TestLogsQueryRoutingAndFilter(t *testing.T) {
	got := noError(t, invoke(t, "logs.query", json.RawMessage(`{"source":"kernel"}`)))
	if !strings.Contains(got, `"source":"kernel"`) || !strings.Contains(got, `"count":2`) {
		t.Fatalf("result=%s，期望 kernel/count=2", got)
	}
	got = noError(t, invoke(t, "logs.query", json.RawMessage(`{"keyword":"OOM"}`)))
	if !strings.Contains(got, `"count":1`) {
		t.Fatalf("result=%s，期望 OOM/count=1", got)
	}
}

// TestLogsInvalidParams 覆盖非法入参返回 E_INVALID。
func TestLogsInvalidParams(t *testing.T) {
	cases := []struct {
		name, params, want string
	}{
		{"非对象", `"x"`, "params 必须是 JSON 对象"},
		{"source 非法", `{"source":"bogus"}`, "source 非法"},
		{"limit 超限", `{"limit":1001}`, "超过上限"},
		{"limit 负数", `{"limit":-1}`, "不能为负数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invoke(t, "logs.query", json.RawMessage(tc.params))
			if r.Error == nil || r.Error.Code != pluginapi.E_INVALID {
				t.Fatalf("error=%v，期望 E_INVALID", r.Error)
			}
			if !strings.Contains(r.Error.Message, tc.want) {
				t.Fatalf("message=%q，期望包含 %q", r.Error.Message, tc.want)
			}
		})
	}
}

// TestUnknownMethod 未知方法回 E_NOTFOUND。
func TestUnknownMethod(t *testing.T) {
	r := invoke(t, "logs.nope", nil)
	if r.Error == nil || r.Error.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("error=%v，期望 E_NOTFOUND", r.Error)
	}
}

// TestFaultMapping 验证四类故障在协议层映射为正确错误码（端到端）。
func TestFaultMapping(t *testing.T) {
	cases := []struct{ fault, code string }{
		{"kill", pluginapi.E_UPSTREAM},
		{"timeout", pluginapi.E_UPSTREAM},
		{"disk-full", pluginapi.E_INTERNAL},
		{"cgroup-oom", pluginapi.E_INTERNAL},
	}
	for _, tc := range cases {
		t.Run(tc.fault, func(t *testing.T) {
			pluginConn, kernelConn := net.Pipe()
			defer func() { _ = kernelConn.Close() }()
			cfg := testConfig()
			cfg.fault = tc.fault
			done := make(chan error, 1)
			go func() { done <- serve(pluginConn, cfg, testLogger()); _ = pluginConn.Close() }()
			_ = pluginapi.WriteJSON(kernelConn, &pluginapi.Request{
				Type: pluginapi.TypeRequest, ID: "id-f", ReqID: "r-f", Method: "logs.list",
			})
			r := readResponse(t, kernelConn)
			if r.Error == nil || r.Error.Code != tc.code {
				t.Fatalf("fault=%s error=%v，期望 %s", tc.fault, r.Error, tc.code)
			}
			_ = pluginapi.WriteJSON(kernelConn, &pluginapi.Request{
				Type: pluginapi.TypeRequest, ID: "id-s", ReqID: "r-s", Method: "kore.shutdown",
			})
			_ = readResponse(t, kernelConn)
			if err := <-done; err != nil {
				t.Fatalf("serve 返回错误: %v", err)
			}
		})
	}
}
