// Package e2e 用真实插件进程跑 logs 生命周期：进程经 UDS 完成 kore.hello 握手后，
// sources/tail/search/stats 全部读取临时日志文件，不触碰宿主真实日志。
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// buildPlugin 构建真实插件二进制。
func buildPlugin(t *testing.T, dir string) string {
	t.Helper()
	binary := filepath.Join(dir, "logs-plugin")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建插件失败: %v\n%s", err, out)
	}
	return binary
}

// startPlugin 以给定来源配置启动插件进程；extraEnv 追加运行期环境变量
// （如 KORE_STATE_DIR，供 download 导出落盘）。
func startPlugin(t *testing.T, binary, socket, sources string, extraEnv ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	process := exec.Command(binary)
	process.Env = append(os.Environ(),
		"KORE_PLUGIN_SOCK="+socket,
		"KORE_PLUGIN_ID=logs",
		"KORE_PLUGIN_TOKEN=e2e",
		"KORE_LOGS_SOURCES="+sources,
	)
	process.Env = append(process.Env, extraEnv...)
	process.Stdout = io.Discard
	process.Stderr = stderr
	if err := process.Start(); err != nil {
		t.Fatalf("启动插件失败: %v", err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
		}
	})
	return process, stderr
}

// handshake 读首帧 kore.hello 并回写成功响应。
func handshake(t *testing.T, conn net.Conn) {
	t.Helper()
	request := readRequest(t, conn)
	if request.Method != "kore.hello" {
		t.Fatalf("首帧应为 kore.hello，实际 %s", request.Method)
	}
	response := &pluginapi.Response{
		Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID,
		Result: json.RawMessage(`{"ok":true,"protocol":"kore.plugin.v1"}`),
	}
	if err := pluginapi.WriteJSON(conn, response); err != nil {
		t.Fatalf("回写 hello 失败: %v", err)
	}
}

// request 发一个请求并读回响应。
func request(t *testing.T, conn net.Conn, id, method, params string) *pluginapi.Response {
	t.Helper()
	req := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: id, Method: method, Params: json.RawMessage(params),
	}
	if err := pluginapi.WriteJSON(conn, req); err != nil {
		t.Fatalf("发送 %s 失败: %v", method, err)
	}
	return readResponse(t, conn)
}

func readRequest(t *testing.T, conn net.Conn) *pluginapi.Request {
	t.Helper()
	env, err := pluginapi.DecodeFrame(readPayload(t, conn))
	if err != nil {
		t.Fatalf("解码请求失败: %v", err)
	}
	request, ok := env.(*pluginapi.Request)
	if !ok {
		t.Fatalf("期望 request，实际 %T", env)
	}
	return request
}

func readResponse(t *testing.T, conn net.Conn) *pluginapi.Response {
	t.Helper()
	env, err := pluginapi.DecodeFrame(readPayload(t, conn))
	if err != nil {
		t.Fatalf("解码响应失败: %v", err)
	}
	response, ok := env.(*pluginapi.Response)
	if !ok {
		t.Fatalf("期望 response，实际 %T", env)
	}
	return response
}

func readPayload(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	return payload
}

// waitExit 等待插件在 kore.shutdown 后优雅退出。
func waitExit(t *testing.T, process *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("插件退出异常: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("插件未在 5s 内退出")
	}
}
