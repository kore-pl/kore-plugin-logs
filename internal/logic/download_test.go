package logic

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustDownload 调用 download 并在无错误时返回结果。
func mustDownload(t *testing.T, registry *Registry, req DownloadRequest) *DownloadResult {
	t.Helper()
	result, ferr := registry.Download(req)
	if ferr != nil {
		t.Fatalf("Download 意外错误: %v", ferr)
	}
	return result
}

// TestDownloadWritesFile 覆盖导出落盘：文件存在、内容为最近 N 行、size/lines/base64 一致。
func TestDownloadWritesFile(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "line-1\nline-2\nERROR line-3\n"))
	stateDir := t.TempDir()
	result := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: 2, StateDir: stateDir})
	if result.Lines != 2 || result.SizeBytes != int64(len("line-2\nERROR line-3\n")) {
		t.Fatalf("result=%+v，期望最近 2 行", result)
	}
	if filepath.Dir(result.Path) != filepath.Join(stateDir, "exports") {
		t.Fatalf("path=%q，期望落在 <state>/exports", result.Path)
	}
	if !strings.HasSuffix(result.Filename, ".log") || !strings.HasPrefix(result.Filename, "app-") {
		t.Fatalf("filename=%q，期望 app-<ts>.log", result.Filename)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("读导出文件失败: %v", err)
	}
	if string(data) != "line-2\nERROR line-3\n" {
		t.Fatalf("内容=%q，期望最近 2 行", data)
	}
	if info, err := os.Stat(result.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("导出文件权限=%v，期望 0600", info.Mode().Perm())
	}
	if got, err := base64.StdEncoding.DecodeString(result.DataBase64); err != nil || string(got) != string(data) {
		t.Fatalf("data_base64 与落盘内容不一致")
	}
}

// TestDownloadSizeCap 覆盖 5 MiB 导出上限：达到即停止写入并标记 truncated。
func TestDownloadSizeCap(t *testing.T) {
	line := strings.Repeat("a", 1023) + "\n" // 1024B/行；10000 行 = 10 MiB > 5 MiB
	var builder strings.Builder
	for i := 0; i < 60000; i++ {
		builder.WriteString(line)
	}
	registry := fileRegistry(t, writeTempFile(t, builder.String()))
	result := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: MaxDownloadLines, StateDir: t.TempDir()})
	if result.SizeBytes > MaxDownloadBytes {
		t.Fatalf("size=%d，超过 5 MiB 上限", result.SizeBytes)
	}
	if !result.Truncated {
		t.Fatalf("超限应置 truncated")
	}
	if result.Note == "" {
		t.Fatalf("截断应带说明")
	}
	if result.Lines != int(MaxDownloadBytes/1024) {
		t.Fatalf("lines=%d，期望 5 MiB 内的 %d 行", result.Lines, MaxDownloadBytes/1024)
	}
	info, err := os.Stat(result.Path)
	if err != nil || info.Size() != result.SizeBytes {
		t.Fatalf("落盘大小与返回不一致: %v", err)
	}
}

// TestSafeExportName 覆盖文件名清洗：非法字符替换、空与点回退、超长截断。
func TestSafeExportName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"syslog", "syslog"},
		{"sys log", "sys_log"},
		{"../etc/passwd", ".._etc_passwd"},
		{"a/b\\c:d", "a_b_c_d"},
		{"", "export"},
		{".", "export"},
		{"..", "export"},
		{"名称", "__"},
		{strings.Repeat("x", 100), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		if got := safeExportName(tc.in); got != tc.want {
			t.Fatalf("safeExportName(%q)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
	// 清洗结果再拼接后必须仍在导出目录内（纵深防御验证）。
	joined := filepath.Join("/state/exports", safeExportName("../etc/passwd")+"-20261123T000000Z.log")
	if !strings.HasPrefix(joined, "/state/exports/") {
		t.Fatalf("拼接结果 %q 越出导出目录", joined)
	}
}

// TestDownloadValidation 覆盖缺 state_dir、lines 越限与未登记来源。
func TestDownloadValidation(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "x\n"))
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: ""}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("缺 state_dir 应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: t.TempDir(), Lines: -1}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("lines=-1 应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: t.TempDir(), Lines: MaxDownloadLines + 1}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("lines 超限应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "bogus", StateDir: t.TempDir()}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("未登记来源应 E_INVALID，got %+v", ferr)
	}
	// fault 注入在方法入口生效。
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: t.TempDir(), Fault: FaultTimeout}); ferr == nil || ferr.Code != CodeUpstream {
		t.Fatalf("fault=timeout 应 E_UPSTREAM，got %+v", ferr)
	}
}

// TestDownloadLargeFileTruncation 覆盖大文件导出只读尾部：100 MiB 稀疏文件导出 500 行可完成。
func TestDownloadLargeFileTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(100 << 20); err != nil {
		t.Fatalf("稀疏截断失败: %v", err)
	}
	// 尾部 1 MiB 写满真实短行（稀疏 NUL 区无换行，模拟大日志的真实行密度）。
	var tailData strings.Builder
	for i := 0; i < 8190; i++ {
		tailData.WriteString("padding-line-0000\n")
	}
	tailData.WriteString("tail-error-line\n")
	if _, err := f.WriteAt([]byte(tailData.String()), 99<<20); err != nil {
		t.Fatalf("写尾部失败: %v", err)
	}
	registry := NewRegistry([]SourceSpec{{Name: "app", Path: path, Type: SourceTypeFile}})
	result := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: 500, StateDir: t.TempDir()})
	if result.Lines != 500 || result.SizeBytes == 0 {
		t.Fatalf("result=%+v，期望读出尾部 500 行", result)
	}
	if strings.Contains(result.DataBase64, "") && result.DataBase64 == "" {
		t.Fatalf("base64 内容不应为空")
	}
}
