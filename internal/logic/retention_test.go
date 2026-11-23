package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeRotated 在 dir 下写一个轮转文件并指定修改时间（相对 now 的天数，负值=更早）。
func makeRotated(t *testing.T, dir, name string, size int, ageDays int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatalf("写轮转文件失败: %v", err)
	}
	ts := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("设置 mtime 失败: %v", err)
	}
	return path
}

// TestRetentionSetGet 覆盖策略保存 / 读取 / 边界值与未登记来源拒绝。
func TestRetentionSetGet(t *testing.T) {
	path := writeTempFile(t, "x\n")
	registry := fileRegistry(t, path)
	stateDir := t.TempDir()

	days, size, files := 7, 10, 2
	set := mustRetentionSet(t, registry, RetentionSetRequest{
		Source: "app", MaxAgeDays: &days, MaxSizeMB: &size, MaxFiles: &files, StateDir: stateDir,
	})
	if !set.Saved || set.Policy.MaxAgeDays != 7 || set.Policy.MaxSizeMB != 10 || set.Policy.MaxFiles != 2 {
		t.Fatalf("set=%+v", set)
	}
	got := mustRetentionGet(t, registry, RetentionGetRequest{StateDir: stateDir})
	if got.Defaults.MaxAgeDays != DefaultRetentionDays || got.Policies["app"].MaxFiles != 2 {
		t.Fatalf("get=%+v", got)
	}
	info, err := os.Stat(filepath.Join(stateDir, retentionFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retention.json 权限=%v err=%v", info.Mode().Perm(), err)
	}

	bad := 4000
	for _, tc := range []RetentionSetRequest{
		{Source: "app", MaxAgeDays: &bad, StateDir: stateDir},
		{Source: "app", MaxSizeMB: intPtr(-1), StateDir: stateDir},
		{Source: "app", MaxFiles: intPtr(MaxRetentionFiles + 1), StateDir: stateDir},
		{Source: "bogus", MaxFiles: intPtr(1), StateDir: stateDir},
		{Source: "app", StateDir: ""},
	} {
		if _, ferr := registry.RetentionSet(tc); ferr == nil || ferr.Code != CodeInvalid {
			t.Fatalf("非法策略应 E_INVALID：%+v → %+v", tc, ferr)
		}
	}
	// 显式 0 = 不限制，允许保存。
	if _, ferr := registry.RetentionSet(RetentionSetRequest{Source: "app", MaxAgeDays: intPtr(0), StateDir: stateDir}); ferr != nil {
		t.Fatalf("显式 0 应允许: %+v", ferr)
	}
}

// TestCleanDryRunNoMove 覆盖 dry_run 默认只列清单、不动任何文件。
func TestCleanDryRunNoMove(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	if err := os.WriteFile(active, []byte("active\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	makeRotated(t, dir, "app.log.20260101T000000Z", 100, 10)
	makeRotated(t, dir, "app.log.20260102T000000Z", 100, 5)
	registry := fileRegistry(t, active)
	stateDir := t.TempDir()
	mustRetentionSet(t, registry, RetentionSetRequest{Source: "app", MaxFiles: intPtr(1), StateDir: stateDir})

	result := mustClean(t, registry, CleanRequest{StateDir: stateDir, DryRun: true})
	if len(result.Candidates) != 1 || result.TotalBytes == 0 {
		t.Fatalf("dry_run=%+v，期望 1 个候选且可释放字节>0", result)
	}
	if !result.DryRun || len(result.Moved) != 0 {
		t.Fatalf("dry_run 不应移动文件: %+v", result)
	}
	for _, name := range []string{"app.log", "app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("dry_run 不得删除 %s: %v", name, err)
		}
	}
}

// TestCleanConfirmMoveRestore 覆盖真实清理移入回收区 + 恢复；活跃文件永不被清理。
func TestCleanConfirmMoveRestore(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	if err := os.WriteFile(active, []byte("active\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	old := makeRotated(t, dir, "app.log.20260101T000000Z", 100, 10)
	newer := makeRotated(t, dir, "app.log.20260102T000000Z", 100, 1)
	registry := fileRegistry(t, active)
	stateDir := t.TempDir()
	mustRetentionSet(t, registry, RetentionSetRequest{Source: "app", MaxFiles: intPtr(1), StateDir: stateDir})

	result := mustClean(t, registry, CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	if len(result.Moved) != 1 || result.Moved[0] != filepath.Base(old) {
		t.Fatalf("moved=%v，期望只清理最旧的 1 份", result.Moved)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("活跃文件不得被清理: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("保留份数内文件不得被清理: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine", "app", filepath.Base(old))); err != nil {
		t.Fatalf("待清理文件应在回收区: %v", err)
	}
	restored := mustRestore(t, registry, RestoreRequest{Source: "app", StateDir: stateDir})
	if len(restored.Restored) != 1 {
		t.Fatalf("restored=%+v", restored)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("恢复后文件应回到原目录: %v", err)
	}
}

// TestCleanRequiresConfirm 覆盖真实清理必须 confirm；否则 E_INVALID。
func TestCleanRequiresConfirm(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "x\n"))
	if _, ferr := registry.Clean(CleanRequest{StateDir: t.TempDir(), DryRun: false}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("缺 confirm 应 E_INVALID，got %+v", ferr)
	}
}

// TestCleanSecurityBoundaries 覆盖白名单外来源拒绝与符号链接不进入候选。
func TestCleanSecurityBoundaries(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	if err := os.WriteFile(active, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	registry := fileRegistry(t, active)
	stateDir := t.TempDir()
	if _, ferr := registry.Clean(CleanRequest{Source: "bogus", StateDir: stateDir, DryRun: false, Confirm: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("未登记来源应 E_INVALID，got %+v", ferr)
	}
	journal := NewRegistry([]SourceSpec{{Name: "journal", Type: SourceTypeJournal}})
	if _, ferr := journal.Clean(CleanRequest{Source: "journal", StateDir: stateDir, DryRun: false, Confirm: true}); ferr == nil || ferr.Code != CodeUnsupported {
		t.Fatalf("journal 应 E_UNSUPPORTED，got %+v", ferr)
	}

	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("写白名单外文件失败: %v", err)
	}
	link := filepath.Join(dir, "app.log.link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	_ = mustRetentionSet(t, registry, RetentionSetRequest{Source: "app", MaxFiles: intPtr(0), MaxAgeDays: intPtr(0), MaxSizeMB: intPtr(0), StateDir: stateDir})
	result := mustClean(t, registry, CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	for _, c := range result.Candidates {
		if c.File == filepath.Base(link) {
			t.Fatalf("符号链接不得进入候选: %+v", c)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("符号链接不应被删除/移动: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
		t.Fatalf("白名单外文件被改动: %v %q", err, data)
	}
}

// TestQuarantineQuotaEviction 覆盖回收区超限淘汰最旧（配额调小以便测试）。
func TestQuarantineQuotaEviction(t *testing.T) {
	oldQuota := quarantineQuotaBytes
	quarantineQuotaBytes = 10
	t.Cleanup(func() { quarantineQuotaBytes = oldQuota })

	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	if err := os.WriteFile(active, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	makeRotated(t, dir, "app.log.20260101T000000Z", 100, 10)
	makeRotated(t, dir, "app.log.20260102T000000Z", 100, 5)
	registry := fileRegistry(t, active)
	stateDir := t.TempDir()
	mustRetentionSet(t, registry, RetentionSetRequest{Source: "app", MaxFiles: intPtr(0), MaxAgeDays: intPtr(1), StateDir: stateDir})
	result := mustClean(t, registry, CleanRequest{StateDir: stateDir, DryRun: false, Confirm: true})
	if len(result.Moved) != 2 || len(result.Evicted) == 0 {
		t.Fatalf("moved=%v evicted=%v，期望 2 个移入且触发淘汰", result.Moved, result.Evicted)
	}
	if result.QuarantineBytes > 10 {
		t.Fatalf("quarantine_bytes=%d，期望 ≤10（配额）", result.QuarantineBytes)
	}
}

// TestUsageOverPolicy 覆盖占用统计与失控判读（份数超限）。
func TestUsageOverPolicy(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	if err := os.WriteFile(active, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	makeRotated(t, dir, "app.log.20260101T000000Z", 100, 10)
	makeRotated(t, dir, "app.log.20260102T000000Z", 100, 5)
	registry := fileRegistry(t, active)
	stateDir := t.TempDir()
	mustRetentionSet(t, registry, RetentionSetRequest{Source: "app", MaxFiles: intPtr(1), StateDir: stateDir})
	usage := mustUsage(t, registry, UsageRequest{StateDir: stateDir})
	if len(usage.Sources) != 1 || usage.Sources[0].Files != 2 || usage.Sources[0].RotatedBytes != 200 {
		t.Fatalf("usage=%+v", usage.Sources)
	}
	if !usage.Sources[0].OverPolicy || usage.OverCount != 1 || usage.Sources[0].OldestAt == "" {
		t.Fatalf("期望判读为超策略: %+v", usage.Sources[0])
	}
	if !strings.Contains(strings.Join(usage.Sources[0].Reasons, "；"), "份数") {
		t.Fatalf("reasons=%v，期望包含份数超限", usage.Sources[0].Reasons)
	}
}

// intPtr 返回 int 指针（区分「未传」与「显式 0」）。
func intPtr(v int) *int { return &v }

func mustRetentionSet(t *testing.T, r *Registry, req RetentionSetRequest) *RetentionSetResult {
	t.Helper()
	result, ferr := r.RetentionSet(req)
	if ferr != nil {
		t.Fatalf("RetentionSet 意外错误: %v", ferr)
	}
	return result
}

func mustRetentionGet(t *testing.T, r *Registry, req RetentionGetRequest) *RetentionGetResult {
	t.Helper()
	result, ferr := r.RetentionGet(req)
	if ferr != nil {
		t.Fatalf("RetentionGet 意外错误: %v", ferr)
	}
	return result
}

func mustClean(t *testing.T, r *Registry, req CleanRequest) *CleanResult {
	t.Helper()
	result, ferr := r.Clean(req)
	if ferr != nil {
		t.Fatalf("Clean 意外错误: %v", ferr)
	}
	return result
}

func mustRestore(t *testing.T, r *Registry, req RestoreRequest) *RestoreResult {
	t.Helper()
	result, ferr := r.Restore(req)
	if ferr != nil {
		t.Fatalf("Restore 意外错误: %v", ferr)
	}
	return result
}

func mustUsage(t *testing.T, r *Registry, req UsageRequest) *UsageResult {
	t.Helper()
	result, ferr := r.Usage(req)
	if ferr != nil {
		t.Fatalf("Usage 意外错误: %v", ferr)
	}
	return result
}
