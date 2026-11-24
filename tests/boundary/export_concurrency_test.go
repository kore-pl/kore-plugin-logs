package boundary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestBoundaryConcurrentExportSameSecond 是「同秒并发导出同名目标」用例：
// 16 个来源都叫 app（目标文件名都形如 app-<UTC秒>.log），但各自源文件内容不同。
// barrier 放行后同时落盘，断言每份导出都拿到唯一文件名、内容谁也没覆盖谁。
//
// 修复前：uniqueExportPath 先 Lstat 判空再返回共享目标名，各写唯一 tmp 后 rename
// 到同一路径，后写的覆盖前写的 → 存活文件数 < 16、内容集合缺失。
// 修复后：目标名用 O_EXCL 原子占名，冲突换后缀 → 16 份全部保留。
// 本包在 -race 下运行，Registry 被多 goroutine 并发复用不得报数据竞争。
func TestBoundaryConcurrentExportSameSecond(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()

	const exporters = 16
	registries := make([]*logic.Registry, exporters)
	wantContents := make(map[string]bool, exporters)
	for i := 0; i < exporters; i++ {
		content := fmt.Sprintf("export-%02d unique line\n", i)
		wantContents[content] = true
		path := filepath.Join(t.TempDir(), "app.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写来源文件 %d: %v", i, err)
		}
		registries[i] = logic.NewRegistry([]logic.SourceSpec{{Name: "app", Path: path, Type: logic.SourceTypeFile}})
	}

	start := make(chan struct{})
	filenames := make(chan string, exporters)
	errCh := make(chan error, exporters)
	var wg sync.WaitGroup
	for i := 0; i < exporters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			res, ferr := registries[idx].Download(logic.DownloadRequest{
				Source: "app", Lines: 10, StateDir: stateDir,
			})
			if ferr != nil {
				errCh <- fmt.Errorf("导出者 %d 失败: %v", idx, ferr)
				return
			}
			filenames <- res.Filename
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	close(filenames)
	for err := range errCh {
		t.Fatal(err)
	}

	// 断言 1：每个并发导出都拿到互不相同的目标文件名。
	seen := make(map[string]bool, exporters)
	for name := range filenames {
		if seen[name] {
			t.Fatalf("文件名 %q 被两个导出者同时拿到：共享目标名必然互相覆盖", name)
		}
		seen[name] = true
	}

	// 断言 2：导出目录里 16 份全在，且内容集合完整——谁也没覆盖谁。
	dir := filepath.Join(stateDir, "exports")
	logs := make([]string, 0, exporters)
	for _, name := range listDirNames(t, dir) {
		if strings.HasSuffix(name, ".log") {
			logs = append(logs, name)
		}
	}
	if len(logs) != exporters {
		t.Fatalf("导出文件数=%d 期望=%d：同秒并发导出互相覆盖（后写覆盖前写）", len(logs), exporters)
	}
	gotContents := make(map[string]bool, exporters)
	for _, name := range logs {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读导出文件 %s: %v", name, err)
		}
		gotContents[string(data)] = true
	}
	for content := range wantContents {
		if !gotContents[content] {
			t.Fatalf("导出内容被覆盖丢失: %q", content)
		}
	}
}
