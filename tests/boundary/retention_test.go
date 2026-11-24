package boundary

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// retentionSources 是本用例的 4 个来源：各自独立活跃文件，共用同一个 state 目录。
var retentionSources = []string{"app", "auth", "nginx", "worker"}

// retentionRegistry 造 4 来源注册表（每个来源一个真实活跃文件）。
func retentionRegistry(t *testing.T, dir string) *logic.Registry {
	t.Helper()
	specs := make([]logic.SourceSpec, 0, len(retentionSources))
	for _, name := range retentionSources {
		active := filepath.Join(dir, name+".log")
		if err := os.WriteFile(active, []byte("2026-11-23T00:00:00Z INFO seed\n"), 0o600); err != nil {
			t.Fatalf("写活跃文件 %s: %v", active, err)
		}
		specs = append(specs, logic.SourceSpec{Name: name, Path: active, Type: logic.SourceTypeFile})
	}
	return logic.NewRegistry(specs)
}

// TestBoundaryConcurrentRetentionSet 是「4 来源并发写保留策略」用例。
//
// 设计前提：策略落盘是「读-改-写」共享文件，并发调用必须串行化。
// 固定临时名（retention.json.tmp）会让并发写互相踩踏——A 的 rename 先把 tmp 挪走，
// B 的 rename 随即 ENOENT；即便侥幸都成功，后写者也会覆盖前写者的来源而丢更新。
//
// 断言（可观察外部行为，不依赖实现细节）：
//  1. 4 来源并发写后，retention.json 里恰好 4 条策略（一条不丢）；
//  2. 每条策略内容与它自己的来源严格对应，无串扰；
//  3. state 目录不残留任何 retention.json.tmp* 临时文件。
//
// 该包在 -race 下运行：Registry 被多 goroutine 并发复用不得报数据竞争。
func TestBoundaryConcurrentRetentionSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stateDir := t.TempDir()
	registry := retentionRegistry(t, dir)

	// 多轮并发：单轮窗口窄，重复放大「读-改-写」竞争的概率。
	const rounds = 6
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		errCh := make(chan error, len(retentionSources))
		var wg sync.WaitGroup
		for i, source := range retentionSources {
			// 每个来源写入可区分且互不相同的策略值，串扰会被下面的逐条断言抓住。
			days := 10 + i + round
			size := 100 + i + round
			files := 3 + i + round
			wg.Add(1)
			go func(source string, days, size, files int) {
				defer wg.Done()
				<-start
				res, ferr := registry.RetentionSet(logic.RetentionSetRequest{
					Source:     source,
					MaxAgeDays: &days,
					MaxSizeMB:  &size,
					MaxFiles:   &files,
					StateDir:   stateDir,
				})
				if ferr != nil {
					errCh <- fmt.Errorf("来源 %s 第 %d 轮 RetentionSet 失败: %s: %s", source, round, ferr.Code, ferr.Message)
					return
				}
				if !res.Saved || res.Source != source || res.Policy.MaxAgeDays != days ||
					res.Policy.MaxSizeMB != size || res.Policy.MaxFiles != files {
					errCh <- fmt.Errorf("来源 %s 第 %d 轮返回异常: %+v", source, round, res)
				}
			}(source, days, size, files)
		}
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}

		got, ferr := registry.RetentionGet(logic.RetentionGetRequest{StateDir: stateDir})
		if ferr != nil {
			t.Fatalf("第 %d 轮读取策略失败: %s: %s", round, ferr.Code, ferr.Message)
		}
		if len(got.Policies) != len(retentionSources) {
			t.Fatalf("第 %d 轮并发写后策略条数=%d 期望=%d（并发丢更新），policies=%+v",
				round, len(got.Policies), len(retentionSources), got.Policies)
		}
		for i, source := range retentionSources {
			want := logic.RetentionPolicy{MaxAgeDays: 10 + i + round, MaxSizeMB: 100 + i + round, MaxFiles: 3 + i + round}
			if got.Policies[source] != want {
				t.Fatalf("第 %d 轮来源 %s 策略=%+v 期望=%+v（内容串扰/丢失）",
					round, source, got.Policies[source], want)
			}
		}
	}

	// 原子写不得把临时文件留在 state 目录。
	tmpLeft, err := filepath.Glob(filepath.Join(stateDir, "retention.json.tmp*"))
	if err != nil {
		t.Fatalf("检查临时文件残留: %v", err)
	}
	if len(tmpLeft) != 0 {
		t.Fatalf("state 目录残留临时文件: %v", tmpLeft)
	}
}
