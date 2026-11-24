package boundary

import (
	"os"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestBoundaryPermissionDenied 是「权限不足」用例：真实 chmod 000 制造 EACCES，
// 断言读取路径返回结构化 E_INTERNAL + 权限语义，usage 单来源降级不拖垮全局，
// 破坏性清理不静默成功、配置落盘失败不产生半成品。
//
// 与 tests/fault/clean_test.go 的 chmod 用例是独立版本：那边测「部分成功」，
// 这里测「整目录不可读 + 配置落盘失败」，不复用其 helper、不改原件。
func TestBoundaryPermissionDenied(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 忽略权限位，chmod 000 无法制造 EACCES")
	}
	dir := t.TempDir()
	registry, _ := seedActive(t, dir, "app", []byte("2026-11-23T00:00:00Z INFO line\n"))
	seedRotated(t, dir, "app.log", 3, 128)
	beforeNames := listDirNames(t, dir)
	mustChmod(t, dir, 0o000)

	// 1) tail 读活跃文件：目录不可搜 → 权限不足，结构化 E_INTERNAL。
	_, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: 10})
	requireCode(t, "tail 目录 000", ferr, logic.CodeInternal)
	requireMessageContains(t, "tail 目录 000", ferr, "权限不足")

	// 2) 符号链接解析失败/权限不足时 sources 单条降级，不整体崩。
	status, ferr := registry.Sources(logic.FaultNone)
	if ferr != nil {
		t.Fatalf("sources 不应整体失败: %v", ferr)
	}
	if len(status.Sources) != 1 || status.Sources[0].Readable {
		t.Fatalf("目录不可读时来源应不可读: %+v", status.Sources)
	}
	if status.Sources[0].Note == "" {
		t.Fatalf("不可读来源必须带 note，实际=%+v", status.Sources[0])
	}

	// 3) usage 单来源降级：不返回错误、该来源字节 0 且带 note。
	usage, ferr := registry.Usage(logic.UsageRequest{StateDir: t.TempDir()})
	if ferr != nil {
		t.Fatalf("单来源不可读不应拖垮 usage: %v", ferr)
	}
	if len(usage.Sources) != 1 || usage.Sources[0].TotalBytes != 0 || usage.Sources[0].Note == "" {
		t.Fatalf("不可读来源应 byte=0 且带 note: %+v", usage.Sources)
	}

	// 4) 真实清理（破坏性）：一个文件都移不动 → E_INTERNAL，不静默成功。
	_, ferr = registry.Clean(logic.CleanRequest{Source: "app", Confirm: true, StateDir: t.TempDir()})
	requireCode(t, "clean 目录 000", ferr, logic.CodeInternal)
	// 目录 000 时无法直接列目录，恢复权限后核对「一个文件都没被移走」。
	mustChmod(t, dir, 0o700)
	afterNames := listDirNames(t, dir)
	if len(afterNames) != len(beforeNames) {
		t.Fatalf("清理失败后来源目录应原样保留 %d 个文件，实际=%v", len(beforeNames), afterNames)
	}

	// 5) 配置落盘：state 目录只读 → E_INTERNAL，且不留 retention.json / .tmp。
	stateDir := t.TempDir()
	mustChmod(t, stateDir, 0o000)
	n := 3
	_, ferr = registry.RetentionSet(logic.RetentionSetRequest{Source: "app", MaxFiles: &n, StateDir: stateDir})
	requireCode(t, "retention.set 只读 state", ferr, logic.CodeInternal)
	mustChmod(t, stateDir, 0o700)
	if names := listDirNames(t, stateDir); len(names) != 0 {
		t.Fatalf("落盘失败时不应留下半成品: %v", names)
	}
}

// TestBoundaryPermissionDeniedFile 覆盖「文件本身 000、目录可搜」：打开阶段 EACCES
// 与目录不可读是两条不同代码路径，都必须给出权限语义而非 panic/静默空结果。
func TestBoundaryPermissionDeniedFile(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 忽略权限位，chmod 000 无法制造 EACCES")
	}
	dir := t.TempDir()
	registry, active := seedActive(t, dir, "app", []byte("2026-11-23T00:00:00Z INFO line\n"))
	mustChmod(t, active, 0o000)

	_, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: 10})
	requireCode(t, "tail 文件 000", ferr, logic.CodeInternal)
	requireMessageContains(t, "tail 文件 000", ferr, "权限不足")

	// stats 是只读探测，不得因为不可读而 panic；要么结构标明不可读，要么结构化错误。
	stats, ferr := registry.Stats(logic.StatsRequest{Source: "app"})
	if ferr != nil {
		requireCode(t, "stats 文件 000", ferr, logic.CodeInternal)
		return
	}
	if stats.Readable {
		t.Fatalf("文件 000 不应报告 readable=true: %+v", stats)
	}
	if !strings.Contains(stats.Note, "权限") {
		t.Fatalf("不可读文件 note 必须说明权限原因: %+v", stats)
	}
}
