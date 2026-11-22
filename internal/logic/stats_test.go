package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatsBasic 覆盖大小、行数、可读性与修改时间。
func TestStatsBasic(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "a\nb\nc\n"))
	result := mustStats(t, registry, "app")
	if result.SizeBytes != 6 || result.Lines == nil || *result.Lines != 3 {
		t.Fatalf("result=%+v，期望 size=6 lines=3", result)
	}
	if !result.Readable || result.Path != "app/app.log" || result.Name != "app" || result.File != "app.log" {
		t.Fatalf("result=%+v", result)
	}
	if result.ModifiedAt == "" {
		t.Fatal("modified_at 不应为空")
	}
}

// TestStatsNoTrailingNewline 覆盖末行无换行时的行数。
func TestStatsNoTrailingNewline(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "a\nb"))
	result := mustStats(t, registry, "app")
	if result.Lines == nil || *result.Lines != 2 {
		t.Fatalf("lines=%v，期望 2", result.Lines)
	}
}

// TestStatsEmptyFile 覆盖空文件：0 字节 0 行且可读。
func TestStatsEmptyFile(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, ""))
	result := mustStats(t, registry, "app")
	if result.SizeBytes != 0 || result.Lines == nil || *result.Lines != 0 || !result.Readable {
		t.Fatalf("result=%+v，期望 size=0 lines=0 readable", result)
	}
}

// TestStatsMissingFile 覆盖文件不存在：返回成功 + readable=false + note。
func TestStatsMissingFile(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry([]SourceSpec{{Name: "ghost", Path: filepath.Join(dir, "ghost.log"), Type: SourceTypeFile}})
	result := mustStats(t, registry, "ghost")
	if result.Readable || result.Lines != nil || !strings.Contains(result.Note, "不存在") {
		t.Fatalf("result=%+v，期望 readable=false + 不存在", result)
	}
}

// TestStatsSkipsLineCountForLargeFile 覆盖 >64 MiB 文件不统计行数。
func TestStatsSkipsLineCountForLargeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建大文件失败: %v", err)
	}
	if err := file.Truncate(StatsLineCountMaxBytes + 1); err != nil {
		t.Fatalf("创建稀疏文件失败: %v", err)
	}
	_ = file.Close()
	registry := fileRegistry(t, path)
	result := mustStats(t, registry, "app")
	if result.Lines != nil {
		t.Fatalf("lines=%v，期望 null", *result.Lines)
	}
	if !result.Readable || !strings.Contains(result.Note, "64 MiB") {
		t.Fatalf("result=%+v，期望 readable + 跳过统计说明", result)
	}
}

// TestStatsSymlinkRefused 覆盖符号链接来源：不跟随读取。
func TestStatsSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}
	link := filepath.Join(dir, "app.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	result := mustStats(t, fileRegistry(t, link), "app")
	if result.Readable || !strings.Contains(result.Note, "符号链接") {
		t.Fatalf("result=%+v，期望 readable=false + 符号链接", result)
	}
}

// TestStatsUnregisteredSource 覆盖未登记来源返回 E_INVALID。
func TestStatsUnregisteredSource(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "x\n"))
	if _, ferr := registry.Stats(StatsRequest{Source: "nope"}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("ferr=%v，期望 E_INVALID", ferr)
	}
}
