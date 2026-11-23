package logic

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// exportsDirName 是导出目录相对插件状态目录的子目录名（与 db-mysql 插件同口径）。
const exportsDirName = "exports"

// exportUnsafeChars 匹配文件名中不允许出现的字符；来源名本身已在白名单内，
// 这里做纵深防御：任何非法字符一律替换为下划线，杜绝路径拼接逃逸。
var exportUnsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// safeExportName 把来源名清洗为可安全拼接进文件名的片段：
// 只保留字母数字与 . _ -，其余替换为 _；结果为空 / "." / ".." 时回退 "export"，并限长 64。
func safeExportName(source string) string {
	clean := exportUnsafeChars.ReplaceAllString(strings.TrimSpace(source), "_")
	if clean == "" || clean == "." || clean == ".." {
		clean = "export"
	}
	if len(clean) > 64 {
		clean = clean[:64]
	}
	return clean
}

// Download 实现 logs.download：取来源最近 N 行（复用 tail 的反向扫描与安全口径）
// 拼成片段写入 <state>/exports/<source>-<UTC时间戳>.log（0600，临时文件 + rename 原子落盘），
// 总量达 5 MiB 即停止写入并标记截断；同时返回 base64 内容供前端直接触发浏览器下载
// （与 file.download 相同的 invoke 通道，内核无需新增静态路由）。
func (r *Registry) Download(req DownloadRequest) (*DownloadResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法导出日志片段")
	}
	need, perr := effectiveDownloadLines(req.Lines)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}

	var lines []TailLine
	var note string
	switch entry.spec.Type {
	case SourceTypeJournal:
		entries, _, truncated, lineTruncated, jerr := tailJournal(need, "", time.Time{}, false, newLineMatcher("", time.Time{}, false))
		if jerr != nil {
			return nil, jerr
		}
		lines = entries
		note = journalDownloadNote(truncated, lineTruncated)
	default:
		file, ferr := r.openFile(entry)
		if ferr != nil {
			return nil, ferr
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return nil, methodErr(CodeInternal, "读取来源元信息失败")
		}
		entries, _, _, lineTruncated, terr := tailFileContent(file, info.Size(), need, newLineMatcher("", time.Time{}, false))
		if terr != nil {
			return nil, terr
		}
		lines = entries
		if lineTruncated {
			note = "部分超长行已截断（单行上限 8 KiB）"
		}
	}
	debugf(req.Debugf, "logs.download source=%s lines=%d state_dir=%s", entry.spec.Name, need, stateDir)

	var builder strings.Builder
	written := 0
	truncatedBySize := false
	for _, line := range lines {
		if int64(builder.Len())+int64(len(line.Text)) > MaxDownloadBytes {
			truncatedBySize = true
			break
		}
		builder.WriteString(line.Text)
		builder.WriteByte('\n')
		written++
	}
	if truncatedBySize {
		if note != "" {
			note += "；"
		}
		note += "已截断到 5 MiB 导出上限"
	}

	filename := safeExportName(entry.spec.Name) + "-" + time.Now().UTC().Format("20061123T150405Z") + ".log"
	dir := filepath.Join(stateDir, exportsDirName)
	if err := mkdirPrivate(dir); err != nil {
		return nil, methodErr(CodeInternal, "创建导出目录失败: "+err.Error())
	}
	path := filepath.Join(dir, filename)
	content := builder.String()
	if err := writeFilePrivate(path, content); err != nil {
		return nil, methodErr(CodeInternal, "写入导出文件失败: "+err.Error())
	}
	return &DownloadResult{
		Path:       path,
		Filename:   filename,
		SizeBytes:  int64(len(content)),
		Lines:      written,
		DataBase64: base64.StdEncoding.EncodeToString([]byte(content)),
		Truncated:  truncatedBySize,
		Note:       note,
	}, nil
}

// journalDownloadNote 说明 journal 导出的截断口径。
func journalDownloadNote(truncated, lineTruncated bool) string {
	var parts []string
	if lineTruncated {
		parts = append(parts, "部分超长行已截断（单行上限 8 KiB）")
	}
	if truncated {
		parts = append(parts, "journal 输出超过 64 MiB 扫描预算，已截断")
	}
	return strings.Join(parts, "；")
}

// mkdirPrivate 创建 0700 导出目录（已存在则复用）。
func mkdirPrivate(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// writeFilePrivate 以 0600 原子写目标文件：先写同目录临时文件再 rename，
// 中途失败不留下半截导出文件。
func writeFilePrivate(path, content string) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
