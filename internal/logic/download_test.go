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

// TestDownloadChunkRoundTrip 覆盖分块读取边界：按 id + offset/limit 逐块取，
// 拼接结果必须与整份导出逐字节相同，next_offset 单调前进、最后一块 eof=true。
func TestDownloadChunkRoundTrip(t *testing.T) {
	content := makeLines(200)
	registry := fileRegistry(t, writeTempFile(t, content))
	stateDir := t.TempDir()
	full := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: MaxDownloadLines, StateDir: stateDir})
	if full.ID == "" || !full.EOF || full.NextOffset != full.SizeBytes {
		t.Fatalf("full=%+v，整份模式应 eof=true 且 next_offset=size", full)
	}
	var assembled []byte
	offset := int64(0)
	for i := 0; ; i++ {
		if i > 10000 {
			t.Fatalf("分块循环未收敛")
		}
		piece := mustDownload(t, registry, DownloadRequest{ID: full.ID, StateDir: stateDir, Offset: offset, Limit: 97, Chunked: true})
		if piece.Offset != offset || piece.ChunkBytes > 97 {
			t.Fatalf("piece=%+v，期望 offset=%d 且 chunk_bytes≤97", piece, offset)
		}
		data, err := base64.StdEncoding.DecodeString(piece.DataBase64)
		if err != nil || len(data) != piece.ChunkBytes {
			t.Fatalf("data_base64 与 chunk_bytes 不一致: err=%v len=%d", err, len(data))
		}
		assembled = append(assembled, data...)
		if piece.EOF {
			if piece.NextOffset != piece.SizeBytes {
				t.Fatalf("eof 时 next_offset=%d 应等于 size=%d", piece.NextOffset, piece.SizeBytes)
			}
			break
		}
		if piece.NextOffset <= offset {
			t.Fatalf("next_offset 未前进: %d → %d", offset, piece.NextOffset)
		}
		offset = piece.NextOffset
	}
	if string(assembled) != content {
		t.Fatalf("分块拼接内容与源不一致（len=%d vs %d）", len(assembled), len(content))
	}
}

// TestDownloadLineRange 覆盖行区间导出：1 起含两端、超出文件尾只回可用行。
func TestDownloadLineRange(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "l1\nl2\nl3\nl4\n"))
	stateDir := t.TempDir()
	result := mustDownload(t, registry, DownloadRequest{Source: "app", FromLine: 2, ToLine: 3, StateDir: stateDir})
	data, err := os.ReadFile(result.Path)
	if err != nil || string(data) != "l2\nl3\n" {
		t.Fatalf("range 内容=%q err=%v，期望 l2/l3", data, err)
	}
	if result.Lines != 2 {
		t.Fatalf("lines=%d，期望 2", result.Lines)
	}
	overflow := mustDownload(t, registry, DownloadRequest{Source: "app", FromLine: 3, ToLine: 99, StateDir: stateDir})
	data, _ = os.ReadFile(overflow.Path)
	if string(data) != "l3\nl4\n" || overflow.Lines != 2 {
		t.Fatalf("越界区间内容=%q lines=%d，期望 l3/l4", data, overflow.Lines)
	}
}

// TestDownloadChunkRejects 覆盖分块/区间的非法入参与越界 id（路径穿越必须拒绝）。
func TestDownloadChunkRejects(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "a\nb\n"))
	stateDir := t.TempDir()
	full := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: 10, StateDir: stateDir})
	if _, ferr := registry.Download(DownloadRequest{ID: full.ID, StateDir: stateDir, Offset: -1, Chunked: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("offset=-1 应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{ID: full.ID, StateDir: stateDir, Offset: full.SizeBytes + 1, Chunked: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("offset 越界应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{ID: full.ID, StateDir: stateDir, Limit: MaxDownloadChunk + 1, Chunked: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("limit 超限应 E_INVALID，got %+v", ferr)
	}
	for _, id := range []string{"../etc/passwd", "/etc/passwd", "..", ".", "a/b", "a\\b", "x..y"} {
		if _, ferr := registry.Download(DownloadRequest{ID: id, StateDir: stateDir, Chunked: true}); ferr == nil || ferr.Code != CodeInvalid {
			t.Fatalf("id=%q 应 E_INVALID（路径穿越），got %+v", id, ferr)
		}
	}
	if _, ferr := registry.Download(DownloadRequest{ID: "logs-19700101T000000Z.log", StateDir: stateDir, Chunked: true}); ferr == nil || ferr.Code != CodeNotFound {
		t.Fatalf("不存在的 id 应 E_NOTFOUND，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: stateDir, FromLine: 0, ToLine: 3}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("只给 to_line 应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: stateDir, FromLine: 3, ToLine: 1}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("to<from 应 E_INVALID，got %+v", ferr)
	}
	if _, ferr := registry.Download(DownloadRequest{Source: "app", StateDir: stateDir, FromLine: -1, ToLine: 2}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("from_line=-1 应 E_INVALID，got %+v", ferr)
	}
}

// TestDownloadChunkRejectsSymlink 覆盖导出目录内的符号链接：即使 id 合法也必须拒绝。
func TestDownloadChunkRejectsSymlink(t *testing.T) {
	registry := fileRegistry(t, writeTempFile(t, "a\n"))
	stateDir := t.TempDir()
	full := mustDownload(t, registry, DownloadRequest{Source: "app", Lines: 10, StateDir: stateDir})
	dir := filepath.Join(stateDir, "exports")
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("写目标失败: %v", err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	if _, ferr := registry.Download(DownloadRequest{ID: "link.log", StateDir: stateDir, Chunked: true}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("符号链接应 E_INVALID，got %+v（full=%s）", ferr, full.ID)
	}
}
