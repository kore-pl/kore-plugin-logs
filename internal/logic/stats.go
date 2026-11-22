package logic

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"time"
)

// Stats 实现 logs.stats：返回来源大小、修改时间、可读性与行数。
// 文件超过 64 MiB 时跳过行数统计（lines=null）并说明原因；不存在/不可读返回成功 + note。
func (r *Registry) Stats(req StatsRequest) (*StatsResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	if entry.spec.Type == SourceTypeJournal {
		return journalStats(entry.spec.Name), nil
	}
	fs := entry.file
	result := &StatsResult{
		Name: fs.name, File: fs.base, Path: displayPath(fs.name, fs.base), Type: SourceTypeFile,
	}
	if fs.loadErr != nil {
		result.Note = fs.loadErr.Message
		return result, nil
	}
	realDir, err := filepath.EvalSymlinks(fs.dir)
	if err != nil {
		result.Note = "来源目录不可访问"
		return result, nil
	}
	if realDir != fs.realDir {
		result.Note = "来源目录经符号链接解析后与配置不一致，拒绝读取"
		return result, nil
	}
	target := filepath.Join(realDir, fs.base)
	before, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			result.Note = "来源文件不存在"
		} else if errors.Is(err, iofs.ErrPermission) {
			result.Note = "权限不足，无法读取来源元信息"
		} else {
			result.Note = "无法读取来源元信息"
		}
		return result, nil
	}
	if before.Mode()&os.ModeSymlink != 0 {
		result.Note = "来源文件是符号链接，与配置路径不一致，拒绝读取"
		return result, nil
	}
	if !before.Mode().IsRegular() {
		result.Note = "来源不是常规文件"
		return result, nil
	}
	result.SizeBytes = before.Size()
	result.ModifiedAt = before.ModTime().UTC().Format(time.RFC3339)
	file, err := os.Open(target)
	if err != nil {
		result.Note = "权限不足，无法读取来源"
		return result, nil
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		result.Note = "来源文件在读取前被替换，拒绝读取"
		return result, nil
	}
	result.Readable = true
	debugf(req.Debugf, "logs.stats source=%s path=%s", fs.name, target)
	if result.SizeBytes > StatsLineCountMaxBytes {
		result.Note = "文件超过 64 MiB，跳过行数统计（避免全量读取）"
		return result, nil
	}
	lines, err := countLines(io.LimitReader(file, StatsLineCountMaxBytes))
	if err != nil {
		result.Note = "统计行数失败"
		return result, nil
	}
	result.Lines = &lines
	return result, nil
}

// journalStats 返回 journal 来源的统计：journal 不提供单文件大小与行数。
func journalStats(name string) *StatsResult {
	status := journalStatus(name)
	note := "journal 不提供单文件大小与行数"
	if status.Note != "" {
		note = status.Note
	}
	return &StatsResult{
		Name: name, File: "journal", Path: displayPath(name, "journal"), Type: SourceTypeJournal,
		Readable: status.Readable, Note: note,
	}
}
