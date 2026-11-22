package logic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// sourceNamePattern 是来源名的合法字符集：首字符字母或数字，其后允许 . _ -。
var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// fileSource 是一条已装载的文件来源：配置目录 + 固定解析后的真实目录。
// realDir 在装载时求值并在每次读取时复核，防止目录被符号链接替换。
type fileSource struct {
	name    string
	dir     string
	base    string
	realDir string
	loadErr *FaultError
}

// sourceEntry 是注册表里的一条来源：file 来源带 fileSource，journal 来源只留 spec。
type sourceEntry struct {
	spec SourceSpec
	file *fileSource
}

// Registry 是不可变的来源白名单；创建后只读，可被多个请求并发复用。
type Registry struct {
	order   []string
	entries map[string]*sourceEntry
}

// ParseSources 解析并校验 KORE_LOGS_SOURCES：
// 必须是 [{name,path,type}] 的 JSON 数组，name 唯一、type ∈ file|journal、
// path 为 Clean 后的绝对路径（journal 允许空路径）。空串表示未配置任何来源。
func ParseSources(raw string) ([]SourceSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var specs []SourceSpec
	if err := decoder.Decode(&specs); err != nil {
		return nil, fmt.Errorf("KORE_LOGS_SOURCES 必须是 JSON 数组：%w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("KORE_LOGS_SOURCES 在 JSON 数组之后存在多余内容")
	}
	if err := ValidateSources(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// ValidateSources 校验一组已构造的来源配置；配置契约在写盘前复用同一套规则，
// 保证「环境变量解析」与「界面保存」不会出现两套校验口径。
func ValidateSources(specs []SourceSpec) error {
	seen := make(map[string]bool, len(specs))
	for i, spec := range specs {
		if err := validateSourceSpec(i, spec, seen); err != nil {
			return err
		}
	}
	return nil
}

// validateSourceSpec 校验单条来源配置，错误信息带下标便于定位。
func validateSourceSpec(index int, spec SourceSpec, seen map[string]bool) error {
	if !sourceNamePattern.MatchString(spec.Name) {
		return fmt.Errorf("第 %d 条来源 name=%q 非法：1-64 位字母数字或 . _ -，且首字符为字母或数字", index, spec.Name)
	}
	if seen[spec.Name] {
		return fmt.Errorf("第 %d 条来源 name=%q 重复", index, spec.Name)
	}
	seen[spec.Name] = true
	switch spec.Type {
	case SourceTypeFile, SourceTypeJournal:
	default:
		return fmt.Errorf("第 %d 条来源 %q 的 type=%q 非法：只允许 file 或 journal", index, spec.Name, spec.Type)
	}
	if strings.ContainsRune(spec.Path, 0) {
		return fmt.Errorf("第 %d 条来源 %q 的 path 含 NUL 字节", index, spec.Name)
	}
	if strings.Contains(spec.Path, "\\") {
		return fmt.Errorf("第 %d 条来源 %q 的 path 含反斜杠", index, spec.Name)
	}
	if spec.Type == SourceTypeFile && spec.Path == "" {
		return fmt.Errorf("第 %d 条来源 %q 是 file 类型，path 不能为空", index, spec.Name)
	}
	if spec.Path != "" {
		if !filepath.IsAbs(spec.Path) {
			return fmt.Errorf("第 %d 条来源 %q 的 path 必须是绝对路径", index, spec.Name)
		}
		if filepath.Clean(spec.Path) != spec.Path {
			return fmt.Errorf("第 %d 条来源 %q 的 path 必须是 filepath.Clean 后的路径", index, spec.Name)
		}
	}
	return nil
}

// NewRegistry 由已校验的配置构建来源白名单，并在装载时固定每个文件来源的父目录解析结果。
// 父目录不可访问的来源不会被静默跳过：装载错误随来源保存，方法调用时返回结构化错误。
func NewRegistry(specs []SourceSpec) *Registry {
	registry := &Registry{order: make([]string, 0, len(specs)), entries: make(map[string]*sourceEntry, len(specs))}
	for _, spec := range specs {
		entry := &sourceEntry{spec: spec}
		if spec.Type == SourceTypeFile {
			fs := &fileSource{name: spec.Name, dir: filepath.Dir(spec.Path), base: filepath.Base(spec.Path)}
			if real, err := filepath.EvalSymlinks(fs.dir); err != nil {
				fs.loadErr = methodErr(CodeNotFound, "来源目录不可访问")
			} else {
				fs.realDir = real
			}
			entry.file = fs
		}
		if _, exists := registry.entries[spec.Name]; !exists {
			registry.order = append(registry.order, spec.Name)
		}
		registry.entries[spec.Name] = entry
	}
	return registry
}

// lookup 按来源名取注册项；未登记的名称一律拒绝，绝不回退到路径。
func (r *Registry) lookup(name string) (*sourceEntry, *FaultError) {
	if ferr := checkSourceParam(name); ferr != nil {
		return nil, ferr
	}
	entry, ok := r.entries[name]
	if !ok {
		return nil, methodErr(CodeInvalid, "来源未登记："+name+"（只允许 KORE_LOGS_SOURCES 中登记的名称）")
	}
	return entry, nil
}

// displayPath 返回展示用路径 "<来源名>/<文件名>"，不暴露宿主绝对路径。
func displayPath(name, base string) string { return name + "/" + base }

// Sources 实现 logs.sources：按配置顺序探测每个来源的 exists/size/readable。
func (r *Registry) Sources(f Fault) (*SourcesResult, *FaultError) {
	if ferr := checkFault(f); ferr != nil {
		return nil, ferr
	}
	sources := make([]SourceStatus, 0, len(r.order))
	for _, name := range r.order {
		entry := r.entries[name]
		if entry.spec.Type == SourceTypeJournal {
			sources = append(sources, journalStatus(name))
			continue
		}
		sources = append(sources, entry.file.status())
	}
	return &SourcesResult{Sources: sources, Count: len(sources)}, nil
}

// status 只读探测一条文件来源；安全拒绝（符号链接/目录解析不一致）写入 note，不返回错误。
func (f *fileSource) status() SourceStatus {
	status := SourceStatus{Name: f.name, File: f.base, Type: SourceTypeFile}
	if f.loadErr != nil {
		status.Note = f.loadErr.Message
		return status
	}
	realDir, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		status.Note = "来源目录不可访问"
		return status
	}
	if realDir != f.realDir {
		status.Note = "来源目录经符号链接解析后与配置不一致，拒绝读取"
		return status
	}
	target := filepath.Join(realDir, f.base)
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			status.Note = "来源文件不存在"
		} else if errors.Is(err, iofs.ErrPermission) {
			status.Note = "权限不足，无法读取来源元信息"
		} else {
			status.Note = "无法读取来源元信息"
		}
		return status
	}
	status.Exists = true
	if info.Mode()&os.ModeSymlink != 0 {
		status.Note = "来源文件是符号链接，与配置路径不一致，拒绝读取"
		return status
	}
	if !info.Mode().IsRegular() {
		status.Note = "来源不是常规文件"
		return status
	}
	status.SizeBytes = info.Size()
	status.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
	file, err := os.Open(target)
	if err != nil {
		status.Note = "权限不足，无法读取来源"
		return status
	}
	opened, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil || !os.SameFile(info, opened) {
		status.Note = "来源文件在探测期间被替换，拒绝读取"
		return status
	}
	status.Readable = true
	return status
}

// openFile 按安全口径解析并打开一条文件来源：
// 父目录解析结果必须与装载时一致、最终文件必须存在且不是符号链接、打开后 inode 必须一致。
func (r *Registry) openFile(entry *sourceEntry) (*os.File, *FaultError) {
	fs := entry.file
	if fs == nil {
		return nil, methodErr(CodeInvalid, "来源类型不是文件")
	}
	if fs.loadErr != nil {
		return nil, fs.loadErr
	}
	realDir, err := filepath.EvalSymlinks(fs.dir)
	if err != nil {
		return nil, methodErr(CodeNotFound, "来源目录不可访问")
	}
	if realDir != fs.realDir {
		return nil, methodErr(CodeInvalid, "来源目录经符号链接解析后与配置不一致，已拒绝读取")
	}
	target := filepath.Join(realDir, fs.base)
	before, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, methodErr(CodeNotFound, "来源文件不存在")
		}
		if errors.Is(err, iofs.ErrPermission) {
			return nil, methodErr(CodeInternal, "权限不足，无法读取来源")
		}
		return nil, methodErr(CodeInternal, "无法读取来源元信息")
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, methodErr(CodeInvalid, "来源文件是符号链接，与配置路径不一致，已拒绝读取")
	}
	if !before.Mode().IsRegular() {
		return nil, methodErr(CodeInvalid, "来源不是常规文件")
	}
	file, err := os.Open(target)
	if err != nil {
		if errors.Is(err, iofs.ErrPermission) {
			return nil, methodErr(CodeInternal, "权限不足，无法读取来源")
		}
		return nil, methodErr(CodeInternal, "打开来源文件失败")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, methodErr(CodeInternal, "读取来源元信息失败")
	}
	if !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, methodErr(CodeInvalid, "来源文件在读取前被替换，已拒绝读取")
	}
	return file, nil
}
