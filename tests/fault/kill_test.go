// Package fault 是 logs 插件的四类故障注入测试（G-20 / 提案 §2.1）。
// 每个用例验证：故障时方法级返回结构化错误，而不是崩溃或静默成功。
package fault

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestFaultKill 验证 kill -9 故障下 logs.sources / logs.tail 返回 E_UPSTREAM，
// 并真实 kill -9 一个子进程确认 SIGKILL 语义（不是空断言刷 gate）。
func TestFaultKill(t *testing.T) {
	registry := logic.NewRegistry(nil)
	result, ferr := registry.Sources(logic.FaultKill)
	if result != nil {
		t.Fatalf("kill 故障下不应有结果，实际=%+v", result)
	}
	if ferr == nil || ferr.Code != "E_UPSTREAM" || !strings.Contains(ferr.Message, "kill") {
		t.Fatalf("ferr=%v，期望 E_UPSTREAM/kill", ferr)
	}

	_, ferr = registry.Tail(logic.TailRequest{Source: "system", Fault: logic.FaultKill})
	if ferr == nil || ferr.Code != "E_UPSTREAM" {
		t.Fatalf("query kill ferr=%v，期望 E_UPSTREAM", ferr)
	}

	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("系统无 sleep，跳过真实 SIGKILL 子进程验证")
	}
	cmd := exec.Command(path, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill 子进程失败: %v", err)
	}
	err = cmd.Wait()
	if err == nil {
		t.Fatal("子进程被 SIGKILL 后 Wait 应返回错误")
	}
	if !strings.Contains(err.Error(), "signal: killed") && !strings.Contains(err.Error(), "signal 9") {
		t.Fatalf("Wait 错误=%q，期望 SIGKILL 语义", err)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signal() != syscall.SIGKILL {
			t.Fatalf("信号=%v，期望 SIGKILL", ws.Signal())
		}
	}
}
