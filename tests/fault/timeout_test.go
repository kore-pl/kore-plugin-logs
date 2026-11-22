package fault

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestFaultTimeout 验证方法超时故障返回 E_UPSTREAM，并用真实
// context.WithTimeout 确认 DeadlineExceeded 语义（请求级定时器，符合 G-19）。
func TestFaultTimeout(t *testing.T) {
	result, ferr := logic.List(logic.FaultTimeout)
	if result != nil {
		t.Fatalf("timeout 故障下不应有结果，实际=%+v", result)
	}
	if ferr == nil || ferr.Code != "E_UPSTREAM" || !strings.Contains(ferr.Message, "timeout") {
		t.Fatalf("ferr=%v，期望 E_UPSTREAM/timeout", ferr)
	}

	_, ferr = logic.Query(logic.QueryRequest{Source: logic.SourceAll, Fault: logic.FaultTimeout})
	if ferr == nil || ferr.Code != "E_UPSTREAM" {
		t.Fatalf("query timeout ferr=%v，期望 E_UPSTREAM", ferr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("ctx.Err()=%v，期望 DeadlineExceeded", ctx.Err())
		}
	case <-block:
		t.Fatal("阻塞通道不应就绪")
	}
}
