package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// invokeWith 用给定运行期配置发一个请求并读回响应（每个用例独立连接，结束后发 shutdown）。
func invokeWith(t *testing.T, cfg config, method string, params json.RawMessage) *pluginapi.Response {
	t.Helper()
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
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

// invoke 用空来源注册表发请求。
func invoke(t *testing.T, method string, params json.RawMessage) *pluginapi.Response {
	t.Helper()
	return invokeWith(t, testConfig(), method, params)
}

func noError(t *testing.T, r *pluginapi.Response) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("意外错误: code=%s message=%s", r.Error.Code, r.Error.Message)
	}
	return string(r.Result)
}

// logConfig 构造一个指向临时日志文件的运行期配置，同时返回文件绝对路径。
func logConfig(t *testing.T, content string) (config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写测试日志失败: %v", err)
	}
	cfg := testConfig()
	cfg.registry = logic.NewRegistry([]logic.SourceSpec{
		{Name: "app", Path: path, Type: logic.SourceTypeFile},
	})
	return cfg, path
}

// TestLogsSourcesMethods 覆盖 sources 裸名/全名、来源探测与路径脱敏。
func TestLogsSourcesMethods(t *testing.T) {
	cfg, path := logConfig(t, "hello\nworld\n")
	for _, method := range []string{"sources", "logs.sources"} {
		t.Run(method, func(t *testing.T) {
			got := noError(t, invokeWith(t, cfg, method, nil))
			for _, want := range []string{`"name":"app"`, `"file":"app.log"`, `"readable":true`, `"exists":true`, `"count":1`} {
				if !strings.Contains(got, want) {
					t.Fatalf("result=%s 缺少 %s", got, want)
				}
			}
			if strings.Contains(got, filepath.Dir(path)) {
				t.Fatalf("result=%s 泄露宿主绝对路径", got)
			}
		})
	}
}

// TestLogsTailProtocol 覆盖 tail 的分块尾部读取与 keyword 过滤。
func TestLogsTailProtocol(t *testing.T) {
	var lines strings.Builder
	for i := 1; i <= 10; i++ {
		lines.WriteString("line-")
		lines.WriteString(strings.Repeat("x", i))
		lines.WriteString("\n")
	}
	cfg, _ := logConfig(t, lines.String())
	got := noError(t, invokeWith(t, cfg, "logs.tail", json.RawMessage(`{"source":"app","lines":2}`)))
	if !strings.Contains(got, `"count":2`) || !strings.Contains(got, "line-xxxxxxxxxx") || !strings.Contains(got, "line-xxxxxxxxx") {
		t.Fatalf("result=%s，期望最后两行", got)
	}
	cfgFiltered, _ := logConfig(t, "ok\nERROR boom\nok\nERROR boom2\n")
	got = noError(t, invokeWith(t, cfgFiltered, "logs.tail", json.RawMessage(`{"source":"app","lines":2,"keyword":"error"}`)))
	if !strings.Contains(got, `"count":2`) || !strings.Contains(got, "boom2") || !strings.Contains(got, "boom\"") {
		t.Fatalf("result=%s，keyword 过滤期望从尾部凑够 2 行 ERROR", got)
	}
}

// TestLogsSearchProtocol 覆盖 search 的行号与命中内容。
func TestLogsSearchProtocol(t *testing.T) {
	cfg, _ := logConfig(t, "alpha\nERROR first\nbeta\nERROR second\n")
	got := noError(t, invokeWith(t, cfg, "logs.search", json.RawMessage(`{"source":"app","keyword":"error"}`)))
	for _, want := range []string{`"line_no":2`, `"line_no":4`, `"count":2`, `"scanned_bytes":`} {
		if !strings.Contains(got, want) {
			t.Fatalf("result=%s 缺少 %s", got, want)
		}
	}
}

// TestLogsStatsProtocol 覆盖 stats 的大小、行数与可读性。
func TestLogsStatsProtocol(t *testing.T) {
	cfg, _ := logConfig(t, "a\nb\nc\n")
	got := noError(t, invokeWith(t, cfg, "logs.stats", json.RawMessage(`{"source":"app"}`)))
	for _, want := range []string{`"path":"app/app.log"`, `"size_bytes":6`, `"lines":3`, `"readable":true`} {
		if !strings.Contains(got, want) {
			t.Fatalf("result=%s 缺少 %s", got, want)
		}
	}
}

// TestLogsInvalidParams 覆盖非法入参返回 E_INVALID。
func TestLogsInvalidParams(t *testing.T) {
	cfg, _ := logConfig(t, "x\n")
	cases := []struct {
		name, method, params, want string
	}{
		{"tail 非对象", "logs.tail", `"x"`, "params 必须是 JSON 对象"},
		{"tail 未登记来源", "logs.tail", `{"source":"bogus"}`, "来源未登记"},
		{"tail lines 负数", "logs.tail", `{"source":"app","lines":-1}`, "lines 必须在"},
		{"tail lines 超限", "logs.tail", `{"source":"app","lines":2001}`, "lines 必须在"},
		{"tail since 非法", "logs.tail", `{"source":"app","since":"yesterday"}`, "since 必须是 RFC3339"},
		{"search 缺 keyword", "logs.search", `{"source":"app"}`, "keyword 必填"},
		{"search limit 超限", "logs.search", `{"source":"app","keyword":"x","limit":1001}`, "limit 必须在"},
		{"stats 缺 source", "logs.stats", `{}`, "source 必填"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invokeWith(t, cfg, tc.method, json.RawMessage(tc.params))
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
			cfg := testConfig()
			cfg.fault = tc.fault
			r := invokeWith(t, cfg, "logs.sources", nil)
			if r.Error == nil || r.Error.Code != tc.code {
				t.Fatalf("fault=%s error=%v，期望 %s", tc.fault, r.Error, tc.code)
			}
		})
	}
}
