package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestLogsProcessE2E 起真实插件二进制，经 UDS 握手后走完 sources/tail/search/stats，
// 并验证未登记来源被拒；所有读取都落在临时日志目录。
func TestLogsProcessE2E(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "app.log")
	var content strings.Builder
	for i := 1; i <= 50; i++ {
		content.WriteString(fmt.Sprintf("2026-11-22T10:00:%02dZ INFO line-%03d\n", i%60, i))
		if i%10 == 0 {
			content.WriteString(fmt.Sprintf("2026-11-22T10:00:%02dZ ERROR boom-%03d\n", i%60, i))
		}
	}
	if err := os.WriteFile(logPath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("写临时日志失败: %v", err)
	}
	sources, err := json.Marshal([]map[string]string{
		{"name": "app", "path": logPath, "type": "file"},
	})
	if err != nil {
		t.Fatalf("序列化来源配置失败: %v", err)
	}

	binary := buildPlugin(t, tmp)
	socket := filepath.Join(tmp, "logs.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 UDS 失败: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(15 * time.Second))

	process, stderr := startPlugin(t, binary, socket, string(sources), "KORE_STATE_DIR="+tmp)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("接受插件连接失败: %v (stderr=%s)", err, stderr.String())
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	handshake(t, conn)

	steps := []struct {
		method  string
		params  string
		wantErr bool
		check   func(t *testing.T, payload string)
	}{
		{"logs.sources", `{}`, false, func(t *testing.T, payload string) {
			for _, want := range []string{`"name":"app"`, `"file":"app.log"`, `"readable":true`, `"exists":true`} {
				if !strings.Contains(payload, want) {
					t.Fatalf("sources 缺少 %s: %s", want, payload)
				}
			}
			if strings.Contains(payload, tmp) {
				t.Fatalf("sources 泄露宿主路径: %s", payload)
			}
		}},
		{"logs.tail", `{"source":"app","lines":3}`, false, func(t *testing.T, payload string) {
			if !strings.Contains(payload, `"count":3`) || !strings.Contains(payload, "line-050") {
				t.Fatalf("tail 未返回真实末尾行: %s", payload)
			}
		}},
		{"logs.tail", `{"source":"app","lines":2,"keyword":"error"}`, false, func(t *testing.T, payload string) {
			if !strings.Contains(payload, `"count":2`) || !strings.Contains(payload, "boom-050") || !strings.Contains(payload, "boom-040") {
				t.Fatalf("tail keyword 过滤不符: %s", payload)
			}
		}},
		{"logs.search", `{"source":"app","keyword":"boom","limit":3}`, false, func(t *testing.T, payload string) {
			for _, want := range []string{`"line_no":11`, `"line_no":22`, `"line_no":33`, `"count":3`, `"truncated":true`} {
				if !strings.Contains(payload, want) {
					t.Fatalf("search 缺少 %s: %s", want, payload)
				}
			}
		}},
		{"logs.search", `{"source":"app","query":"boom","limit":2}`, false, func(t *testing.T, payload string) {
			for _, want := range []string{`"count":2`, `"total":5`, `"level":"error"`, `"time":"`} {
				if !strings.Contains(payload, want) {
					t.Fatalf("search(query) 缺少 %s: %s", want, payload)
				}
			}
		}},
		{"logs.download", `{"source":"app","lines":5}`, false, func(t *testing.T, payload string) {
			for _, want := range []string{`"path":"`, `"size_bytes":`, `"lines":5`, `"data_base64":"`, `"filename":"app-`} {
				if !strings.Contains(payload, want) {
					t.Fatalf("download 缺少 %s: %s", want, payload)
				}
			}
		}},
		{"logs.stats", `{"source":"app"}`, false, func(t *testing.T, payload string) {
			for _, want := range []string{`"path":"app/app.log"`, `"readable":true`, `"size_bytes":`, `"lines":`} {
				if !strings.Contains(payload, want) {
					t.Fatalf("stats 缺少 %s: %s", want, payload)
				}
			}
		}},
		{"logs.tail", `{"source":"/etc/passwd"}`, true, nil},
		{"logs.search", `{"source":"app","keyword":""}`, true, nil},
		{"logs.download", `{"source":"bogus"}`, true, nil},
	}
	for i, step := range steps {
		response := request(t, conn, fmt.Sprintf("e2e-%d", i), step.method, step.params)
		payload := string(response.Result)
		if response.Error != nil {
			payload = fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, response.Error.Code, response.Error.Message)
		}
		t.Logf("E2E_PROCESS_JSON %s %s", step.method, payload)
		if step.wantErr {
			if response.Error == nil || response.Error.Code != pluginapi.E_INVALID {
				t.Fatalf("%s 应返回 E_INVALID: %+v", step.method, response)
			}
			continue
		}
		if response.Error != nil {
			t.Fatalf("%s 失败: %s", step.method, response.Error.Message)
		}
		step.check(t, payload)
	}

	// 增量 tail 端到端：首次 tail 取 position，追加 3 行后带 position 再拉，
	// 断言只回新行、position 前进到新文件尾（真实进程 + 真实文件偏移）。
	first := request(t, conn, "e2e-tail-pos-1", "logs.tail", `{"source":"app","lines":2}`)
	var firstTail struct {
		Position int64 `json:"position"`
		Count    int   `json:"count"`
	}
	if err := json.Unmarshal(first.Result, &firstTail); err != nil {
		t.Fatalf("解析 tail 响应失败: %v", err)
	}
	if firstTail.Position == 0 || firstTail.Count != 2 {
		t.Fatalf("首次 tail=%+v，期望 2 行与 position", firstTail)
	}
	appendFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开追加句柄失败: %v", err)
	}
	if _, err := appendFile.WriteString("2026-11-22T10:00:59Z ERROR boom-appended\nplain appended\n"); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	_ = appendFile.Close()
	second := request(t, conn, "e2e-tail-pos-2", "logs.tail",
		fmt.Sprintf(`{"source":"app","lines":100,"position":%d}`, firstTail.Position))
	var secondTail struct {
		Count    int   `json:"count"`
		Position int64 `json:"position"`
		Entries  []struct {
			Text  string `json:"text"`
			Level string `json:"level"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(second.Result, &secondTail); err != nil {
		t.Fatalf("解析增量 tail 响应失败: %v", err)
	}
	if secondTail.Count != 2 || len(secondTail.Entries) != 2 {
		t.Fatalf("增量 tail count=%d，期望只回 2 条新行: %s", secondTail.Count, second.Result)
	}
	if secondTail.Entries[0].Text != "2026-11-22T10:00:59Z ERROR boom-appended" || secondTail.Entries[0].Level != "error" {
		t.Fatalf("增量首行=%+v，期望 boom-appended(error)", secondTail.Entries[0])
	}
	if secondTail.Position != firstTail.Position+int64(len("2026-11-22T10:00:59Z ERROR boom-appended\nplain appended\n")) {
		t.Fatalf("增量 position=%d，期望前进到新文件尾", secondTail.Position)
	}

	response := request(t, conn, "e2e-shutdown", "kore.shutdown", `{}`)
	if response.Error != nil {
		t.Fatalf("shutdown 失败: %s", response.Error.Message)
	}
	waitExit(t, process)
}
