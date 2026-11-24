package boundary

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestBoundaryConcurrentWriteAndRotate 是「并发写日志 + 并发轮转」用例：
// 4 个写入者各自持有 O_APPEND fd 持续追加，主 goroutine 连续 3 轮切割。
// 并发安全的设计前提是「切割 = 原子 rename」+「Registry 创建后只读」——
// 这里断言可观察不变量：所有写入的记录跨活跃/轮转文件守恒，一条都不能丢。
// 同时该包在 -race 下运行，Registry 被多 goroutine 并发复用不得报数据竞争。
func TestBoundaryConcurrentWriteAndRotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := "app.log"
	active := filepath.Join(dir, base)

	// 预置约 3 MiB 基线内容：放大切割窗口，使「非原子切割」的丢写可稳定复现。
	seedRecord := []byte("seed 2026-11-23T00:00:00Z INFO baseline record\n")
	const seedRecords = 70000
	var seed bytes.Buffer
	seed.Grow(len(seedRecord) * seedRecords)
	for i := 0; i < seedRecords; i++ {
		seed.Write(seedRecord)
	}
	if err := os.WriteFile(active, seed.Bytes(), 0o600); err != nil {
		t.Fatalf("写基线活跃文件: %v", err)
	}
	registry := logic.NewRegistry([]logic.SourceSpec{{Name: "app", Path: active, Type: logic.SourceTypeFile}})

	const writers = 4
	files := make([]*os.File, writers)
	for i := 0; i < writers; i++ {
		f, err := os.OpenFile(active, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("打开写入 fd %d: %v", i, err)
		}
		files[i] = f
	}

	stop := make(chan struct{})
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	var written atomic.Int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int, f *os.File) {
			defer wg.Done()
			record := []byte(fmt.Sprintf("writer-%d 2026-11-23T00:00:00Z INFO concurrent append record\n", idx))
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := f.Write(record); err != nil {
					errCh <- fmt.Errorf("写入者 %d 追加失败: %w", idx, err)
					return
				}
				written.Add(1)
			}
		}(i, files[i])
	}

	time.Sleep(10 * time.Millisecond)
	const rounds = 3
	rotations := 0
	for round := 0; round < rounds; round++ {
		if _, ferr := registry.Rotate(logic.RotateRequest{Source: "app", Confirm: true, Keep: 100}); ferr != nil {
			t.Fatalf("第 %d 轮切割失败: %v", round+1, ferr)
		}
		rotations++
		// 模拟被切割服务重开日志文件：新建空的活跃文件，供下一轮切割。
		if _, err := os.Lstat(active); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(active, nil, 0o600); err != nil {
				t.Fatalf("重建活跃文件: %v", err)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	for _, f := range files {
		_ = f.Close()
	}
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if rotations != rounds {
		t.Fatalf("切割成功 %d 轮，期望 %d 轮", rotations, rounds)
	}

	// 守恒断言：活跃文件 + 全部轮转文件里的完整记录数 == 基线 + 并发写入数。
	var total int64
	detail := make([]string, 0, rounds+1)
	for _, name := range listDirNames(t, dir) {
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		n := int64(bytes.Count(data, []byte("\n")))
		total += n
		detail = append(detail, fmt.Sprintf("%s=%d", name, n))
	}
	want := int64(seedRecords) + written.Load()
	if total != want {
		t.Fatalf("并发写+轮转丢记录：实际=%d 期望=%d（丢失 %d），明细 %v\n"+
			"切割必须是原子 rename：改成 copy+delete 会把竞争窗口内的追加写入丢在不可达 inode 上。",
			total, want, want-total, detail)
	}
}

// TestBoundaryConcurrentRotateSingleSource 是「同一来源被并发切割」用例：
// 多个调用同时切同一条来源，只能有一个成功（原子 rename 只允许一个赢家），
// 其余返回结构化错误而不是 panic 或静默成功；磁盘上只允许产生一个轮转文件。
func TestBoundaryConcurrentRotateSingleSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := "app.log"
	registry, active := seedActive(t, dir, "app", []byte("2026-11-23T00:00:00Z INFO line\n"))

	const callers = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *logic.FaultError, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ferr := registry.Rotate(logic.RotateRequest{Source: "app", Confirm: true, Keep: 100})
			results <- ferr
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	success, failed := 0, 0
	for ferr := range results {
		if ferr == nil {
			success++
			continue
		}
		failed++
		if ferr.Code != logic.CodeNotFound && ferr.Code != logic.CodeInternal {
			t.Fatalf("并发切割失败者错误码=%s，期望 E_NOTFOUND 或 E_INTERNAL（message=%q）", ferr.Code, ferr.Message)
		}
	}
	if success != 1 {
		t.Fatalf("并发切割成功 %d 次，期望恰好 1 次（failed=%d）", success, failed)
	}
	rotated := 0
	for _, name := range listDirNames(t, dir) {
		if strings.HasPrefix(name, base+".") {
			rotated++
		}
	}
	if rotated != 1 {
		t.Fatalf("并发切割产生 %d 个轮转文件，期望 1 个", rotated)
	}
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		t.Fatalf("切割后活跃文件应不存在，Lstat err=%v", err)
	}
}

// TestBoundaryConcurrentReadAndClean 是「并发读 + 并发清理」用例：
// 读取者持续 tail/search/stats/usage/sources，清理者并发把旧轮转文件移入回收区。
// 断言：读取永不被清理打断（活跃文件不在清理范围）、活跃内容逐字节不变、
// 轮转文件收敛到保留份数、回收区确有文件；整包在 -race 下运行。
func TestBoundaryConcurrentReadAndClean(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := "app.log"
	var content strings.Builder
	const activeLines = 300
	for i := 0; i < activeLines; i++ {
		fmt.Fprintf(&content, "2026-11-23T00:00:%02dZ INFO needle-%04d payload\n", i%60, i)
	}
	wantActive := content.String()
	registry, active := seedActive(t, dir, "app", []byte(wantActive))
	seedRotated(t, dir, base, 24, 256)
	stateDir := t.TempDir()
	maxFiles := 2
	if _, ferr := registry.RetentionSet(logic.RetentionSetRequest{Source: "app", MaxFiles: &maxFiles, StateDir: stateDir}); ferr != nil {
		t.Fatalf("设置保留份数: %v", ferr)
	}

	errCh := make(chan error, 64)
	var wg sync.WaitGroup
	const readers = 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				tail, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: 50})
				if ferr != nil {
					errCh <- fmt.Errorf("读取者 %d tail: %v", idx, ferr)
					return
				}
				if tail.Count == 0 || len(tail.Entries) == 0 {
					errCh <- fmt.Errorf("读取者 %d tail 活跃文件返回空", idx)
					return
				}
				search, ferr := registry.Search(logic.SearchRequest{Source: "app", Query: "needle", Limit: 10})
				if ferr != nil {
					errCh <- fmt.Errorf("读取者 %d search: %v", idx, ferr)
					return
				}
				if search.Total != activeLines {
					errCh <- fmt.Errorf("读取者 %d search total=%d 期望 %d", idx, search.Total, activeLines)
					return
				}
				stats, ferr := registry.Stats(logic.StatsRequest{Source: "app"})
				if ferr != nil || !stats.Readable {
					errCh <- fmt.Errorf("读取者 %d stats: ferr=%v stats=%+v", idx, ferr, stats)
					return
				}
				if _, ferr := registry.Usage(logic.UsageRequest{StateDir: stateDir}); ferr != nil {
					errCh <- fmt.Errorf("读取者 %d usage: %v", idx, ferr)
					return
				}
				if _, ferr := registry.Sources(logic.FaultNone); ferr != nil {
					errCh <- fmt.Errorf("读取者 %d sources: %v", idx, ferr)
					return
				}
			}
		}(i)
	}
	const cleaners = 2
	for i := 0; i < cleaners; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				res, ferr := registry.Clean(logic.CleanRequest{Source: "app", Confirm: true, StateDir: stateDir})
				if ferr != nil {
					// 两个清理者竞争同一批候选：输家一个都没移动 → 纯失败 E_INTERNAL 是合法结果。
					if ferr.Code != logic.CodeInternal {
						errCh <- fmt.Errorf("清理者 %d: %v", idx, ferr)
						return
					}
					continue
				}
				if res == nil || res.DryRun {
					errCh <- fmt.Errorf("清理者 %d 返回异常结果: %+v", idx, res)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	got, err := os.ReadFile(active)
	if err != nil {
		t.Fatalf("清理后活跃文件不可读: %v", err)
	}
	if string(got) != wantActive {
		t.Fatalf("并发清理改动了活跃文件：%d 字节 vs 期望 %d 字节", len(got), len(wantActive))
	}
	remaining := 0
	for _, name := range listDirNames(t, dir) {
		if strings.HasPrefix(name, base+".") {
			remaining++
		}
	}
	if remaining > maxFiles {
		t.Fatalf("轮转文件剩余 %d 个，超过保留份数 %d", remaining, maxFiles)
	}
	quarantine := filepath.Join(stateDir, "quarantine", "app")
	entries, err := os.ReadDir(quarantine)
	if err != nil || len(entries) == 0 {
		t.Fatalf("回收区应有被清理文件: dir=%s err=%v", quarantine, err)
	}
}
