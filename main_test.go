package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

func testConfig() config {
	return config{
		id: "logs", version: "0.1.0",
		requiresKore: ">=0.1.0 <0.2.0",
		capabilities: []string{"system:read"},
		startedAt:    time.Now(),
		registry:     logic.NewRegistry(nil),
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readFrame(t *testing.T, conn net.Conn) any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码帧失败: %v", err)
	}
	return env
}

func readRequest(t *testing.T, conn net.Conn) *pluginapi.Request {
	t.Helper()
	req, ok := readFrame(t, conn).(*pluginapi.Request)
	if !ok {
		t.Fatal("帧不是 request")
	}
	return req
}

func readResponse(t *testing.T, conn net.Conn) *pluginapi.Response {
	t.Helper()
	resp, ok := readFrame(t, conn).(*pluginapi.Response)
	if !ok {
		t.Fatal("帧不是 response")
	}
	return resp
}

func writeResponse(t *testing.T, conn net.Conn, id, reqID string, result json.RawMessage) {
	t.Helper()
	resp := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: id, ReqID: reqID, Result: result}
	if err := pluginapi.WriteJSON(conn, resp); err != nil {
		t.Fatalf("写响应失败: %v", err)
	}
}

// TestHandshake 校验 hello 首帧字段与握手成功路径。
func TestHandshake(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	go func() { done <- handshake(pluginConn, testConfig()); _ = pluginConn.Close() }()

	req := readRequest(t, kernelConn)
	if req.Method != helloMethod {
		t.Fatalf("首帧方法=%q，期望 %q", req.Method, helloMethod)
	}
	var params helloParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("解析 hello 参数失败: %v", err)
	}
	if params.ID != "logs" || params.Protocol != pluginapi.Protocol {
		t.Fatalf("hello 参数 id=%s protocol=%s", params.ID, params.Protocol)
	}
	if params.PID != os.Getpid() {
		t.Fatalf("pid=%d，期望 %d", params.PID, os.Getpid())
	}
	result, _ := json.Marshal(helloResult{OK: true, Protocol: pluginapi.Protocol})
	writeResponse(t, kernelConn, req.ID, req.ReqID, result)
	if err := <-done; err != nil {
		t.Fatalf("握手返回错误: %v", err)
	}
}
