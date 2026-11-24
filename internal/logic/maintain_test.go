package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRetentionDefaultsArePreset 断言"装完不配就生效"：全新 state 目录、没有任何
// retention.json、不调用 retention.set，retention.get 必须直接返回出厂预设，
// 且预设包含自动切割阈值（否则只清理不切割，单文件照样涨到磁盘满）。
func TestRetentionDefaultsArePreset(t *testing.T) {
	path := writeTempFile(t, "x\n")
	registry := fileRegistry(t, path)
	stateDir := t.TempDir()

	got := mustRetentionGet(t, registry, RetentionGetRequest{StateDir: stateDir})
	if len(got.Policies) != 0 {
		t.Fatalf("未配置时不应存在按来源覆盖：%+v", got.Policies)
	}
	want := RetentionPolicy{
		MaxAgeDays: DefaultRetentionDays, MaxSizeMB: DefaultRetentionSizeMB,
		MaxFiles: DefaultRetentionFiles, RotateSizeMB: DefaultRotateSizeMB,
	}
	if got.Defaults != want {
		t.Fatalf("默认策略=%+v，期望 %+v", got.Defaults, want)
	}
	// 产品口径：自动维护必须是默认行为，默认值里切割阈值不能是 0。
	if got.Defaults.RotateSizeMB <= 0 || got.Defaults.MaxAgeDays <= 0 || got.Defaults.MaxSizeMB <= 0 {
		t.Fatalf("默认值必须让切割+清理默认生效：%+v", got.Defaults)
	}
	// 未按来源覆盖时 clean / usage / maintain 用的是同一套默认（不是"未配置"）。
	if p := policyFor(got.Policies, "app"); p != want {
		t.Fatalf("policyFor 默认=%+v，期望 %+v", p, want)
	}
}

// TestDefaultRotateThresholdIs100MiB 断言默认单文件切割阈值是 100 MiB（不是 1 GiB 也不是 0）。
func TestDefaultRotateThresholdIs100MiB(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 当天已切过，隔离"按天"分支，只测大小分支。
	if err := os.WriteFile(filepath.Join(dir, "app.log."+now.UTC().Format("20060102")+"T000000Z"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备当天切割文件失败: %v", err)
	}
	policy := defaultRetentionPolicy()
	if reason := rotateReason(dir, "app.log", int64(100<<20)-1, policy, now); reason != "" {
		t.Fatalf("99 MiB 不应按大小切割，得到 %q", reason)
	}
	if reason := rotateReason(dir, "app.log", int64(100<<20), policy, now); reason != "size" {
		t.Fatalf("100 MiB 应按大小切割，得到 %q", reason)
	}
}

// TestMaintainRotatesBySizeAndTruncates 断言"超大小 → 自动切割"：
// 活跃文件超过配置阈值后，maintain 生成切割文件并把活跃文件清空（copytruncate，写方无需重载）。
func TestMaintainRotatesBySizeAndTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	payload := strings.Repeat("a", 2<<20) // 2 MiB
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	registry := fileRegistry(t, path)
	stateDir := t.TempDir()
	limit := 1 // 1 MiB，测试用；生产默认 100 MiB
	if _, ferr := registry.RetentionSet(RetentionSetRequest{Source: "app", RotateSizeMB: &limit, StateDir: stateDir}); ferr != nil {
		t.Fatalf("设置切割阈值失败: %v", ferr)
	}

	result, ferr := registry.Maintain(MaintainRequest{StateDir: stateDir, Now: time.Now()})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if result.Rotated != 1 || len(result.Sources) != 1 || result.Sources[0].RotatedBy != "size" {
		t.Fatalf("应按大小切割一次：%+v", result)
	}
	rotated := filepath.Join(dir, result.Sources[0].RotatedTo)
	info, err := os.Stat(rotated)
	if err != nil || info.Size() != int64(len(payload)) {
		t.Fatalf("切割文件不存在或大小不对: err=%v info=%v", err, info)
	}
	active, err := os.Stat(path)
	if err != nil || active.Size() != 0 {
		t.Fatalf("活跃文件应被清空: err=%v size=%v", err, active)
	}
}

// TestMaintainRotatesDailyOncePerDay 断言"按时间切割"默认按天：当天切一次，重复调用不再切。
func TestMaintainRotatesDailyOncePerDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	registry := fileRegistry(t, path)
	stateDir := t.TempDir()
	now := time.Now()

	first, ferr := registry.Maintain(MaintainRequest{StateDir: stateDir, Now: now})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if first.Rotated != 1 || first.Sources[0].RotatedBy != "daily" {
		t.Fatalf("首次应按天切割：%+v", first)
	}
	// 当天再写内容再维护：同一天不应再切（否则 6h 一次的维护会切出 4 份/天）。
	if err := os.WriteFile(path, []byte("more\n"), 0o600); err != nil {
		t.Fatalf("追加内容失败: %v", err)
	}
	second, ferr := registry.Maintain(MaintainRequest{StateDir: stateDir, Now: now})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if second.Rotated != 0 {
		t.Fatalf("同一天不应二次切割：%+v", second)
	}
	// 次日同一份活跃文件应再切一份。
	tomorrow := now.Add(24 * time.Hour)
	third, ferr := registry.Maintain(MaintainRequest{StateDir: stateDir, Now: tomorrow})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if third.Rotated != 1 {
		t.Fatalf("次日应再切一份：%+v", third)
	}
}

// TestMaintainCleansExpiredRotated 断言"按时间清理"默认生效：超过保留天数的切割文件
// 被移入回收区（可 restore），活跃文件不受影响。
func TestMaintainCleansExpiredRotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("live\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	// 当天已切过一份：隔离"按天切割"分支，本用例只验证"按时间清理"。
	marker := filepath.Join(dir, "app.log."+time.Now().UTC().Format("20060102")+"T000000Z")
	if err := os.WriteFile(marker, []byte("live\n"), 0o600); err != nil {
		t.Fatalf("写当天切割文件失败: %v", err)
	}
	old := filepath.Join(dir, "app.log.20250101T000000Z")
	if err := os.WriteFile(old, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("写旧切割文件失败: %v", err)
	}
	stamp := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatalf("设置旧文件 mtime 失败: %v", err)
	}
	registry := fileRegistry(t, path)
	stateDir := t.TempDir()

	result, ferr := registry.Maintain(MaintainRequest{StateDir: stateDir, Now: time.Now()})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if result.Cleaned != 1 || len(result.Sources[0].Trimmed) != 1 {
		t.Fatalf("应清理 1 个超龄切割文件：%+v", result)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("超龄切割文件应已移出原目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "quarantine", "app", "app.log.20250101T000000Z")); err != nil {
		t.Fatalf("超龄文件应可在回收区恢复: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len("live\n")) {
		t.Fatalf("活跃文件不应被清理: err=%v", err)
	}
}

// TestMaintainDryRunMakesNoChange 断言预演不改动磁盘。
func TestMaintainDryRunMakesNoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("写活跃文件失败: %v", err)
	}
	registry := fileRegistry(t, path)
	result, ferr := registry.Maintain(MaintainRequest{StateDir: t.TempDir(), DryRun: true, Now: time.Now()})
	if ferr != nil {
		t.Fatalf("Maintain 意外错误: %v", ferr)
	}
	if result.Rotated != 0 {
		t.Fatalf("dry_run 不应切割：%+v", result)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len("hello\n")) {
		t.Fatalf("dry_run 不应改动活跃文件: err=%v", err)
	}
}
