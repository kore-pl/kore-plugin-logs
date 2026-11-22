package logic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSourcesValid 覆盖合法配置解析与字段保真。
func TestParseSourcesValid(t *testing.T) {
	raw := `[
		{"name":"syslog","path":"/var/log/syslog","type":"file"},
		{"name":"journal","path":"","type":"journal"}
	]`
	specs, err := ParseSources(raw)
	if err != nil {
		t.Fatalf("ParseSources 失败: %v", err)
	}
	if len(specs) != 2 || specs[0].Name != "syslog" || specs[1].Type != SourceTypeJournal {
		t.Fatalf("specs=%+v", specs)
	}
	empty, err := ParseSources("  ")
	if err != nil || len(empty) != 0 {
		t.Fatalf("空配置应返回空列表: specs=%+v err=%v", empty, err)
	}
}

// TestParseSourcesRejects 覆盖非法配置：类型、路径、重名、未知字段。
func TestParseSourcesRejects(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"非数组", `{}`, "必须是 JSON 数组"},
		{"尾部垃圾", `[{"name":"a","path":"/tmp/a.log","type":"file"}] x`, "多余内容"},
		{"type 非法", `[{"name":"a","path":"/tmp/a.log","type":"pipe"}]`, "type="},
		{"相对路径", `[{"name":"a","path":"var/a.log","type":"file"}]`, "绝对路径"},
		{"未 Clean", `[{"name":"a","path":"/tmp//a.log","type":"file"}]`, "filepath.Clean"},
		{"含反斜杠", `[{"name":"a","path":"/tmp\\a.log","type":"file"}]`, "反斜杠"},
		{"file 缺 path", `[{"name":"a","path":"","type":"file"}]`, "path 不能为空"},
		{"name 非法", `[{"name":"a/b","path":"/tmp/a.log","type":"file"}]`, "name="},
		{"重名", `[{"name":"a","path":"/tmp/a.log","type":"file"},{"name":"a","path":"/tmp/b.log","type":"file"}]`, "重复"},
		{"未知字段", `[{"name":"a","path":"/tmp/a.log","type":"file","extra":1}]`, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSources(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v，期望包含 %q", err, tc.want)
			}
		})
	}
}

// TestSourcesProbe 覆盖 file 来源的 exists/size/readable 与路径脱敏。
func TestSourcesProbe(t *testing.T) {
	path := writeTempFile(t, "hello\n")
	registry := fileRegistry(t, path)
	result, ferr := registry.Sources(FaultNone)
	if ferr != nil {
		t.Fatalf("Sources 意外错误: %v", ferr)
	}
	if result.Count != 1 || len(result.Sources) != 1 {
		t.Fatalf("sources=%+v", result)
	}
	status := result.Sources[0]
	if status.Name != "app" || status.File != "app.log" || !status.Exists || !status.Readable {
		t.Fatalf("status=%+v", status)
	}
	if status.SizeBytes != 6 || status.ModifiedAt == "" {
		t.Fatalf("size=%d modified=%q", status.SizeBytes, status.ModifiedAt)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), filepath.Dir(path)) {
		t.Fatalf("status JSON 泄露宿主路径: %s", encoded)
	}
}

// TestSourcesMissingFile 覆盖文件不存在时 exists=false、不报错。
func TestSourcesMissingFile(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry([]SourceSpec{{Name: "ghost", Path: filepath.Join(dir, "ghost.log"), Type: SourceTypeFile}})
	result, ferr := registry.Sources(FaultNone)
	if ferr != nil {
		t.Fatalf("Sources 意外错误: %v", ferr)
	}
	if result.Sources[0].Exists || result.Sources[0].Readable || result.Sources[0].Note == "" {
		t.Fatalf("status=%+v，期望 exists=false 且有 note", result.Sources[0])
	}
}

// TestSourcesLoadError 覆盖装载期父目录不可访问：来源保留但标记错误。
func TestSourcesLoadError(t *testing.T) {
	dir := t.TempDir()
	missingDir := filepath.Join(dir, "gone")
	registry := NewRegistry([]SourceSpec{{Name: "gone", Path: filepath.Join(missingDir, "a.log"), Type: SourceTypeFile}})
	result, ferr := registry.Sources(FaultNone)
	if ferr != nil {
		t.Fatalf("Sources 意外错误: %v", ferr)
	}
	status := result.Sources[0]
	if status.Exists || status.Readable || !strings.Contains(status.Note, "目录不可访问") {
		t.Fatalf("status=%+v", status)
	}
	if _, terr := registry.Tail(TailRequest{Source: "gone"}); terr == nil || terr.Code != CodeNotFound {
		t.Fatalf("tail err=%v，期望 E_NOTFOUND", terr)
	}
}

// TestRegistryDuplicateNames 覆盖直接构造时同名来源去重（保留最后一条）。
func TestRegistryDuplicateNames(t *testing.T) {
	first := writeTempFile(t, "a\n")
	second := writeTempFile(t, "b\n")
	registry := NewRegistry([]SourceSpec{
		{Name: "dup", Path: first, Type: SourceTypeFile},
		{Name: "dup", Path: second, Type: SourceTypeFile},
	})
	result, _ := registry.Sources(FaultNone)
	if result.Count != 1 || result.Sources[0].SizeBytes != 2 {
		t.Fatalf("sources=%+v，期望只保留最后一条", result)
	}
}

// TestUnregisteredSourceRejected 覆盖未登记来源被拒绝，且不会退回路径读取。
func TestUnregisteredSourceRejected(t *testing.T) {
	path := writeTempFile(t, "secret\n")
	registry := fileRegistry(t, path)
	if _, ferr := registry.Tail(TailRequest{Source: "other"}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("tail ferr=%v，期望 E_INVALID", ferr)
	}
	if _, ferr := registry.Search(SearchRequest{Source: "/etc/passwd", Keyword: "root"}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("search ferr=%v，期望 E_INVALID", ferr)
	}
	if _, ferr := registry.Stats(StatsRequest{Source: ""}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("stats ferr=%v，期望 E_INVALID", ferr)
	}
}

// TestSymlinkFileRejected 覆盖最终文件是符号链接（指向配置外）时被拒绝。
func TestSymlinkFileRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("root:x:0:0\n"), 0o600); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}
	link := filepath.Join(dir, "app.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	registry := fileRegistry(t, link)
	if _, ferr := registry.Tail(TailRequest{Source: "app"}); ferr == nil || ferr.Code != CodeInvalid || !strings.Contains(ferr.Message, "符号链接") {
		t.Fatalf("tail ferr=%v，期望 E_INVALID/符号链接", ferr)
	}
	if _, ferr := registry.Search(SearchRequest{Source: "app", Keyword: "root"}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("search ferr=%v，期望 E_INVALID", ferr)
	}
	if result, _ := registry.Sources(FaultNone); result.Sources[0].Readable || !strings.Contains(result.Sources[0].Note, "符号链接") {
		t.Fatalf("sources=%+v，期望 readable=false + 符号链接 note", result.Sources[0])
	}
}

// TestSymlinkDirSwapRejected 覆盖装载后父目录被替换为符号链接（防替换攻击）。
func TestSymlinkDirSwapRejected(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	path := filepath.Join(realDir, "app.log")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
	registry := fileRegistry(t, path)

	attackerDir := filepath.Join(root, "attacker")
	if err := os.Mkdir(attackerDir, 0o700); err != nil {
		t.Fatalf("建攻击目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(attackerDir, "app.log"), []byte("forged\n"), 0o600); err != nil {
		t.Fatalf("写伪造日志失败: %v", err)
	}
	if err := os.Rename(realDir, filepath.Join(root, "real-moved")); err != nil {
		t.Fatalf("移动真实目录失败: %v", err)
	}
	if err := os.Symlink(attackerDir, realDir); err != nil {
		t.Fatalf("建目录符号链接失败: %v", err)
	}
	if _, ferr := registry.Tail(TailRequest{Source: "app"}); ferr == nil || ferr.Code != CodeInvalid {
		t.Fatalf("tail ferr=%v，期望 E_INVALID（父目录被替换）", ferr)
	}
	if _, ferr := registry.Stats(StatsRequest{Source: "app"}); ferr != nil {
		t.Fatalf("stats 意外错误: %v", ferr)
	}
}
