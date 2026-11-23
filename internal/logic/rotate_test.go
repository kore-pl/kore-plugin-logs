package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustRotate 调用 rotate 并在无错误时返回结果。
func mustRotate(t *testing.T, registry *Registry, req RotateRequest) *RotateResult {
	t.Helper()
	result, ferr := registry.Rotate(req)
	if ferr != nil {
		t.Fatalf("Rotate 意外错误: %v", ferr)
	}
	return result
}

// TestRotateRenamesAndRetains 覆盖切割 + 保留策略：只留最新 keep 份，旧切割文件被删除。
func TestRotateRenamesAndRetains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("line-1\nline-2\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	// 预置两份历史切割文件（时间戳名称，按字典序即时间序）。
	for _, name := range []string{"app.log.20260101T000000Z", "app.log.20260102T000000Z"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old\n"), 0o600); err != nil {
			t.Fatalf("写历史切割文件失败: %v", err)
		}
	}
	registry := fileRegistry(t, path)
	result := mustRotate(t, registry, RotateRequest{Source: "app", Confirm: true, Keep: 2})
	if result.SizeBytes != int64(len("line-1\nline-2\n")) || result.Kept != 2 {
		t.Fatalf("result=%+v，期望 size=14、kept=2", result)
	}
	if filepath.Dir(result.RotatedTo) != dir || !strings.HasPrefix(filepath.Base(result.RotatedTo), "app.log.") {
		t.Fatalf("rotated_to=%q，期望同目录 app.log.<时间戳>", result.RotatedTo)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("原文件应已被 rename 移走: %v", err)
	}
	data, err := os.ReadFile(result.RotatedTo)
	if err != nil || string(data) != "line-1\nline-2\n" {
		t.Fatalf("切割文件内容=%q err=%v", data, err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "app.log.20260101T000000Z" {
		t.Fatalf("removed=%v，期望删除最旧的 20260101 一份", result.Removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.log.20260101T000000Z")); !os.IsNotExist(err) {
		t.Fatalf("最旧切割文件应已删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "app.log.20260102T000000Z")); err != nil {
		t.Fatalf("保留份数内文件不应被删除: %v", err)
	}
}

// TestRotateRequiresConfirm 覆盖 confirm 门禁：未显式确认一律拒绝，且不动文件。
func TestRotateRequiresConfirm(t *testing.T) {
	path := writeTempFile(t, "x\n")
	registry := fileRegistry(t, path)
	_, ferr := registry.Rotate(RotateRequest{Source: "app"})
	if ferr == nil || ferr.Code != CodeInvalid || !strings.Contains(ferr.Message, "confirm") {
		t.Fatalf("ferr=%+v，期望 E_INVALID 且提示 confirm", ferr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("未确认时不得移动文件: %v", err)
	}
}

// TestRotateRejectsUnsafeSources 覆盖未登记来源 / journal / 符号链接 / 不存在。
func TestRotateRejectsUnsafeSources(t *testing.T) {
	path := writeTempFile(t, "x\n")
	registry := fileRegistry(t, path)
	if _, ferr := registry.Rotate(RotateRequest{Source: "bogus", Confirm: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("未登记来源应 E_INVALID，got %+v", ferr)
	}
	journalRegistry := NewRegistry([]SourceSpec{{Name: "journal", Type: SourceTypeJournal}})
	if _, ferr := journalRegistry.Rotate(RotateRequest{Source: "journal", Confirm: true}); ferr == nil || ferr.Code != CodeUnsupported {
		t.Fatalf("journal 应 E_UNSUPPORTED，got %+v", ferr)
	}

	// 符号链接来源必须拒绝（防借白名单名指向任意文件）。
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写目标失败: %v", err)
	}
	link := filepath.Join(dir, "app.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	linkRegistry := NewRegistry([]SourceSpec{{Name: "app", Path: link, Type: SourceTypeFile}})
	if _, ferr := linkRegistry.Rotate(RotateRequest{Source: "app", Confirm: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("符号链接应 E_INVALID，got %+v", ferr)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ferr := registry.Rotate(RotateRequest{Source: "app", Confirm: true}); ferr == nil || ferr.Code != CodeNotFound {
		t.Fatalf("缺失文件应 E_NOTFOUND，got %+v", ferr)
	}
}

// TestRotateKeepBounds 覆盖 keep 取值区间。
func TestRotateKeepBounds(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "x\n"))
	for _, keep := range []int{-1, MaxRotateKeep + 1} {
		if _, ferr := registry.Rotate(RotateRequest{Source: "app", Confirm: true, Keep: keep}); ferr == nil || ferr.Code != CodeInvalid {
			t.Fatalf("keep=%d 应 E_INVALID，got %+v", keep, ferr)
		}
	}
}

// TestRotateOnlyTouchesMatchingFiles 覆盖清理边界：保留策略只删匹配 <base>.<时间戳> 的常规文件，
// 同目录其它文件（含相似名、其它来源的切割文件）一律不碰。
func TestRotateOnlyTouchesMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	unrelated := []string{"app.log.20260101T000000Z.bak", "other.log.20260101T000000Z", "app.log.notes", "notes.txt"}
	for _, name := range unrelated {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep\n"), 0o600); err != nil {
			t.Fatalf("写无关文件失败: %v", err)
		}
	}
	old := "app.log.20260101T000000Z"
	if err := os.WriteFile(filepath.Join(dir, old), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("写旧切割失败: %v", err)
	}
	registry := fileRegistry(t, path)
	result := mustRotate(t, registry, RotateRequest{Source: "app", Confirm: true, Keep: 1})
	if len(result.Removed) != 1 || result.Removed[0] != old {
		t.Fatalf("removed=%v，期望只删旧切割文件 %s", result.Removed, old)
	}
	for _, name := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("无关文件 %s 不应被删除: %v", name, err)
		}
	}
}

// TestRotateSameSecondCollision 覆盖同一秒内多次切割：文件名追加 -n 后缀，不互相覆盖。
func TestRotateSameSecondCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	registry := fileRegistry(t, path)
	seen := make(map[string]bool, 3)
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(path, []byte("run\n"), 0o600); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
		result := mustRotate(t, registry, RotateRequest{Source: "app", Confirm: true, Keep: MaxRotateKeep})
		if seen[result.RotatedTo] {
			t.Fatalf("同秒切割路径重复: %s", result.RotatedTo)
		}
		seen[result.RotatedTo] = true
	}
	if len(seen) != 3 {
		t.Fatalf("期望 3 个不同的切割路径，got %v", seen)
	}
}
