package fault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// 本文件覆盖 G-20 P0：会删文件的方法（clean / clean.restore）与
// retention.get / retention.set / usage 的故障面。
// 断言的是可观察外部行为（磁盘上文件是否还在、返回码、结构体字段），
// 不只断言「没 panic」。

// faultFileSource 在 dir 下造活跃文件与轮转文件，返回来源注册表与活跃文件路径。
func faultFileSource(t *testing.T, dir, name string, rotated int) (*logic.Registry, string) {
	t.Helper()
	active := filepath.Join(dir, name+".log")
	if err := os.WriteFile(active, []byte("active\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	for i := 0; i < rotated; i++ {
		path := filepath.Join(dir, name+".log.2026010"+string(rune('1'+i))+"T000000Z")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
			t.Fatalf("写轮转文件失败: %v", err)
		}
		ts := time.Now().Add(-time.Duration(10-i) * 24 * time.Hour)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("设置 mtime 失败: %v", err)
		}
	}
	return logic.NewRegistry([]logic.SourceSpec{{Name: name, Path: active, Type: logic.SourceTypeFile}}), active
}

// faultSetMaxFiles 把来源保留份数设为 n（其余维度取默认值）。
func faultSetMaxFiles(t *testing.T, r *logic.Registry, source string, stateDir string, n int) {
	t.Helper()
	if _, ferr := r.RetentionSet(logic.RetentionSetRequest{Source: source, MaxFiles: &n, StateDir: stateDir}); ferr != nil {
		t.Fatalf("设置保留策略失败: %v", ferr)
	}
}

// faultSetMaxAge 把来源保留天数设为 n，使既有轮转文件成为候选（0 表示不限制）。
func faultSetMaxAge(t *testing.T, r *logic.Registry, source string, stateDir string, n int) {
	t.Helper()
	if _, ferr := r.RetentionSet(logic.RetentionSetRequest{Source: source, MaxAgeDays: &n, StateDir: stateDir}); ferr != nil {
		t.Fatalf("设置保留策略失败: %v", ferr)
	}
}

// faultChmod 改权限并在用例结束（或调用方恢复）时还原，保证 t.TempDir 能清干净。
func faultChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	old, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, old.Mode().Perm()) })
}

// TestFaultCleanPartialSuccess 是 P0 主用例：一个来源的文件不可移动（只读目录），
// 另一个来源正常——必须已移动的如实报告、未移动的保持原位、返回结构标出 partial。
func TestFaultCleanPartialSuccess(t *testing.T) {
	appDir := t.TempDir()
	webDir := t.TempDir()
	faultFileSource(t, appDir, "app", 2)
	faultFileSource(t, webDir, "web", 2)
	registry := logic.NewRegistry([]logic.SourceSpec{
		{Name: "app", Path: filepath.Join(appDir, "app.log"), Type: logic.SourceTypeFile},
		{Name: "web", Path: filepath.Join(webDir, "web.log"), Type: logic.SourceTypeFile},
	})
	stateDir := t.TempDir()
	faultSetMaxFiles(t, registry, "app", stateDir, 1)
	faultSetMaxFiles(t, registry, "web", stateDir, 1)

	// app 目录变只读：rename 需要目录写权限，该来源的候选文件全部移不走。
	faultChmod(t, appDir, 0o500)

	result, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	if ferr != nil {
		t.Fatalf("部分成功不应整体报错，ferr=%v", ferr)
	}
	if result == nil || !result.Partial {
		t.Fatalf("result=%+v，期望 partial=true", result)
	}
	if len(result.Moved) != 1 || result.Moved[0] != "web.log.20260101T000000Z" {
		t.Fatalf("moved=%v，期望仅 web 的旧文件被移动", result.Moved)
	}
	if len(result.Failed) != 1 || result.Failed[0].Source != "app" || result.Failed[0].Reason == "" {
		t.Fatalf("failed=%+v，期望 app 来源带原因", result.Failed)
	}
	if !strings.Contains(result.Note, "部分成功") {
		t.Fatalf("note=%q，期望明示部分成功", result.Note)
	}

	// 可观察行为：app 的两个轮转文件都还在原位；web 的旧文件已离开来源目录进入回收区。
	for _, name := range []string{"app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if _, err := os.Stat(filepath.Join(appDir, name)); err != nil {
			t.Fatalf("%s 应保持原位: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(webDir, "web.log.20260101T000000Z")); !os.IsNotExist(err) {
		t.Fatalf("web 旧文件应已移出，err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine", "web", "web.log.20260101T000000Z")); err != nil {
		t.Fatalf("web 旧文件应已进入回收区: %v", err)
	}
	// 活跃文件永不清除。
	if _, err := os.Stat(filepath.Join(webDir, "web.log")); err != nil {
		t.Fatalf("活跃文件不应被清理: %v", err)
	}
}

// TestFaultCleanNothingMovedIsError 覆盖「一个都没移成」：纯失败必须返回错误，
// 且所有候选文件都还在原位（不丢文件）。
func TestFaultCleanNothingMovedIsError(t *testing.T) {
	dir := t.TempDir()
	registry, active := faultFileSource(t, dir, "app", 2)
	stateDir := t.TempDir()
	faultSetMaxAge(t, registry, "app", stateDir, 1)
	faultChmod(t, dir, 0o500)

	result, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	if result != nil {
		t.Fatalf("全部失败时不应返回结果，result=%+v", result)
	}
	if ferr == nil || ferr.Code != logic.CodeInternal || !strings.Contains(ferr.Message, "清理未完成") {
		t.Fatalf("ferr=%v，期望 E_INTERNAL/清理未完成", ferr)
	}
	for _, name := range []string{"app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 未移动则必须保留: %v", name, err)
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("活跃文件不应被清理: %v", err)
	}
}

// TestFaultCleanEmptySourceIdempotent 覆盖无轮转文件（等价空目录/无匹配）：
// 幂等成功、不报错、不动任何文件。
func TestFaultCleanEmptySourceIdempotent(t *testing.T) {
	dir := t.TempDir()
	registry, active := faultFileSource(t, dir, "app", 0)
	stateDir := t.TempDir()
	for i := 0; i < 2; i++ {
		result, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
		if ferr != nil {
			t.Fatalf("第 %d 次清理无匹配应成功: %v", i+1, ferr)
		}
		if result.Partial || len(result.Moved) != 0 || len(result.Failed) != 0 || len(result.Candidates) != 0 {
			t.Fatalf("第 %d 次 result=%+v，期望空清单且非 partial", i+1, result)
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("活跃文件不应被清理: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine")); !os.IsNotExist(err) {
		t.Fatalf("无候选时不应创建回收区，err=%v", err)
	}
}

// TestFaultCleanDiskFull 用既有故障注入点模拟写盘失败：报错语义 E_INTERNAL/ENOSPC，
// 且不留下半个文件、不动任何日志。
func TestFaultCleanDiskFull(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 2)
	stateDir := t.TempDir()
	faultSetMaxAge(t, registry, "app", stateDir, 1)

	result, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true, Fault: logic.FaultDiskFull})
	if result != nil {
		t.Fatalf("磁盘满不应有结果，result=%+v", result)
	}
	if ferr == nil || ferr.Code != logic.CodeInternal || !strings.Contains(ferr.Message, "ENOSPC") {
		t.Fatalf("ferr=%v，期望 E_INTERNAL/ENOSPC", ferr)
	}
	for _, name := range []string{"app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("磁盘满时不得动文件 %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine")); !os.IsNotExist(err) {
		t.Fatalf("磁盘满时不应创建回收区，err=%v", err)
	}
}

// TestFaultCleanUnwritableStateDir 覆盖回收区不可创建：纯失败返回错误，
// 候选文件全部保持原位（不删一半）。
func TestFaultCleanUnwritableStateDir(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 2)
	stateDir := t.TempDir()
	faultSetMaxAge(t, registry, "app", stateDir, 1)
	faultChmod(t, stateDir, 0o500)

	result, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	if result != nil || ferr == nil || ferr.Code != logic.CodeInternal {
		t.Fatalf("result=%+v ferr=%v，期望纯失败 E_INTERNAL", result, ferr)
	}
	for _, name := range []string{"app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 应保持原位: %v", name, err)
		}
	}
}

// TestFaultRestoreTargetExistsNoOverwrite 是 restore 的核心断言：
// 目标已存在时不许静默覆盖——跳过并在结果里列出，源文件（含内容）保持不变，回收区文件仍在。
func TestFaultRestoreTargetExistsNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 1)
	stateDir := t.TempDir()
	faultSetMaxAge(t, registry, "app", stateDir, 1)
	if _, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true}); ferr != nil {
		t.Fatalf("准备回收区失败: %v", ferr)
	}
	target := filepath.Join(dir, "app.log.20260101T000000Z")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("准备阶段目标应已被清理走，err=%v", err)
	}
	// 恢复前目标位置被新文件占用（模拟轮转又写回同名文件）。
	if err := os.WriteFile(target, []byte("newer\n"), 0o600); err != nil {
		t.Fatalf("写占用文件失败: %v", err)
	}

	result, ferr := registry.Restore(logic.RestoreRequest{Source: "app", StateDir: stateDir})
	if ferr != nil {
		t.Fatalf("目标已存在应跳过而非报错: %v", ferr)
	}
	if len(result.Restored) != 0 || len(result.Skipped) != 1 || result.Skipped[0] != "app.log.20260101T000000Z" {
		t.Fatalf("restored=%v skipped=%v，期望只跳过同名文件", result.Restored, result.Skipped)
	}
	if !strings.Contains(result.Note, "不覆盖") {
		t.Fatalf("note=%q，期望明示不覆盖", result.Note)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "newer\n" {
		t.Fatalf("目标文件被覆盖了：content=%q err=%v", string(raw), err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine", "app", "app.log.20260101T000000Z")); err != nil {
		t.Fatalf("被跳过的回收区文件不应丢失: %v", err)
	}
}

// TestFaultRestoreUnwritableSourceDir 覆盖恢复时来源目录不可写：
// 报错语义正确，回收区文件保持完整（不出现「恢复一半」的丢失）。
func TestFaultRestoreUnwritableSourceDir(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 1)
	stateDir := t.TempDir()
	faultSetMaxAge(t, registry, "app", stateDir, 1)
	if _, ferr := registry.Clean(logic.CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true}); ferr != nil {
		t.Fatalf("准备回收区失败: %v", ferr)
	}
	faultChmod(t, dir, 0o500)

	result, ferr := registry.Restore(logic.RestoreRequest{Source: "app", StateDir: stateDir})
	if result != nil || ferr == nil || ferr.Code != logic.CodeInternal || !strings.Contains(ferr.Message, "恢复") {
		t.Fatalf("result=%+v ferr=%v，期望 E_INTERNAL/恢复失败", result, ferr)
	}
	qfile := filepath.Join(stateDir, "quarantine", "app", "app.log.20260101T000000Z")
	if raw, err := os.ReadFile(qfile); err != nil || len(raw) == 0 {
		t.Fatalf("回收区文件必须保持完整: err=%v len=%d", err, len(raw))
	}
}

// TestFaultRetentionGetCorruptState 覆盖保留策略文件损坏 / 未知字段：
// 必须 E_INTERNAL 而不是静默按空策略继续（否则会误清日志）。
func TestFaultRetentionGetCorruptState(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 0)
	for _, body := range []string{"{not json", `{"policies":{"app":{"max_age_days":1}},"extra":1}`} {
		stateDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateDir, "retention.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("写损坏策略失败: %v", err)
		}
		if _, ferr := registry.RetentionGet(logic.RetentionGetRequest{StateDir: stateDir}); ferr == nil || ferr.Code != logic.CodeInternal {
			t.Fatalf("body=%q ferr=%v，期望 E_INTERNAL", body, ferr)
		}
		if _, ferr := registry.Usage(logic.UsageRequest{StateDir: stateDir}); ferr == nil || ferr.Code != logic.CodeInternal {
			t.Fatalf("usage body=%q ferr=%v，期望 E_INTERNAL", body, ferr)
		}
	}
}

// TestFaultRetentionSetUnwritableState 覆盖策略落盘失败：
// E_INTERNAL 且不留半个文件（无 retention.json、无 .tmp 残留）。
func TestFaultRetentionSetUnwritableState(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 0)
	stateDir := t.TempDir()
	faultChmod(t, stateDir, 0o500)
	n := 3
	if _, ferr := registry.RetentionSet(logic.RetentionSetRequest{Source: "app", MaxFiles: &n, StateDir: stateDir}); ferr == nil || ferr.Code != logic.CodeInternal {
		t.Fatalf("ferr=%v，期望 E_INTERNAL", ferr)
	}
	for _, name := range []string{"retention.json", "retention.json.tmp"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s 不应落盘，err=%v", name, err)
		}
	}
}

// TestFaultUsageUnreadableSourceDir 覆盖来源目录不可读：usage 不整体失败，
// 该来源带 note、字节为 0，其余来源仍可用（外部可观察）。
func TestFaultUsageUnreadableSourceDir(t *testing.T) {
	badDir := t.TempDir()
	okDir := t.TempDir()
	faultFileSource(t, badDir, "app", 1)
	faultFileSource(t, okDir, "web", 1)
	registry := logic.NewRegistry([]logic.SourceSpec{
		{Name: "app", Path: filepath.Join(badDir, "app.log"), Type: logic.SourceTypeFile},
		{Name: "web", Path: filepath.Join(okDir, "web.log"), Type: logic.SourceTypeFile},
	})
	faultChmod(t, badDir, 0o000)

	usage, ferr := registry.Usage(logic.UsageRequest{StateDir: t.TempDir()})
	if ferr != nil {
		t.Fatalf("单来源不可读不应拖垮 usage: %v", ferr)
	}
	if len(usage.Sources) != 2 {
		t.Fatalf("sources=%d，期望两个来源都在", len(usage.Sources))
	}
	if usage.Sources[0].Note == "" || usage.Sources[0].TotalBytes != 0 {
		t.Fatalf("不可读来源应带 note 且字节为 0: %+v", usage.Sources[0])
	}
	if usage.Sources[1].TotalBytes == 0 {
		t.Fatalf("正常来源应仍统计到字节: %+v", usage.Sources[1])
	}
}

// TestFaultNewMethodsDiskFull 覆盖新方法在「磁盘满」注入下统一返回 E_INTERNAL/ENOSPC。
func TestFaultNewMethodsDiskFull(t *testing.T) {
	dir := t.TempDir()
	registry, _ := faultFileSource(t, dir, "app", 1)
	stateDir := t.TempDir()
	n := 1
	cases := []struct {
		name string
		call func() *logic.FaultError
	}{
		{"retention.get", func() *logic.FaultError {
			_, ferr := registry.RetentionGet(logic.RetentionGetRequest{StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"retention.set", func() *logic.FaultError {
			_, ferr := registry.RetentionSet(logic.RetentionSetRequest{Source: "app", MaxFiles: &n, StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"usage", func() *logic.FaultError {
			_, ferr := registry.Usage(logic.UsageRequest{StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
		{"clean.restore", func() *logic.FaultError {
			_, ferr := registry.Restore(logic.RestoreRequest{StateDir: stateDir, Fault: logic.FaultDiskFull})
			return ferr
		}},
	}
	for _, tc := range cases {
		ferr := tc.call()
		if ferr == nil || ferr.Code != logic.CodeInternal || !strings.Contains(ferr.Message, "ENOSPC") {
			t.Fatalf("%s ferr=%v，期望 E_INTERNAL/ENOSPC", tc.name, ferr)
		}
	}
}
