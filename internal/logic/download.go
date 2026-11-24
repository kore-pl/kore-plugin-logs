package logic

import (
	"bufio"
	"encoding/base64"
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

// exportsDirName 是导出目录相对插件状态目录的子目录名（与 db-mysql 插件同口径）。
const exportsDirName = "exports"

// exportUnsafeChars 匹配文件名中不允许出现的字符；来源名本身已在白名单内，
// 这里做纵深防御：任何非法字符一律替换为下划线，杜绝路径拼接逃逸。
var exportUnsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// exportIDPattern 是导出文件 id 的合法字符集：首字符字母数字，其后允许 . _ -。
// 只允许单段文件名（不含路径分隔符），配合 resolveExportFile 的目录归属校验，
// 双重保证 id 不能指到 <state>/exports 之外。
var exportIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

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

// Download 实现 logs.download：
//   - 不传 id：按来源生成导出文件（默认最近 Lines 行；FromLine/ToLine 选择行区间），
//     写到 <state>/exports/<source>-<UTC时间戳>.log（0700 目录 / 0600 文件，临时文件 + rename 原子落盘）；
//   - 传 id：对既有导出文件按 Offset/Limit 分块读取，不把大文件整份塞进单帧；
//   - 未显式分块（0.3.0 默认语义）时仍返回整份 base64，保持既有调用方兼容。
//
// 体积边界：导出内容硬上限 5 MiB（超限截断并标记），单次分块默认 1 MiB / 上限 4 MiB，
// 全程按行流式处理，GB 级日志不会被整读进内存。
func (r *Registry) Download(req DownloadRequest) (*DownloadResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法导出日志片段")
	}
	if req.ID != "" {
		return readExportChunk(stateDir, req, req.ID)
	}
	if perr := validateLineRange(req.FromLine, req.ToLine); perr != nil {
		return nil, perr
	}
	need, perr := effectiveDownloadLines(req.Lines)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	rangeMode := req.FromLine > 0
	if rangeMode && entry.spec.Type == SourceTypeJournal {
		return nil, methodErr(CodeInvalid, "journal 来源不支持按行区间导出，请用 lines 取尾部窗口")
	}

	lines, note, ferr := r.exportLines(entry, req, need, rangeMode)
	if ferr != nil {
		return nil, ferr
	}
	debugf(req.Debugf, "logs.download source=%s lines=%d range=%v state_dir=%s", entry.spec.Name, need, rangeMode, stateDir)

	content, written, truncatedBySize := buildExportContent(lines)
	if truncatedBySize {
		note = joinNote(note, "已截断到 5 MiB 导出上限")
	}
	dir := filepath.Join(stateDir, exportsDirName)
	if err := mkdirPrivate(dir); err != nil {
		return nil, methodErr(CodeInternal, "创建导出目录失败: "+err.Error())
	}
	path, filename := uniqueExportPath(dir, entry.spec.Name)
	if err := writeFilePrivate(path, content); err != nil {
		return nil, methodErr(CodeInternal, "写入导出文件失败: "+err.Error())
	}
	// 写入时判断（不依赖任何定时器）：导出是中间产物，按份数 + 7 天自清理，
	// 避免"每导出一次就多一个文件"的无界增长。
	pruneExports(dir)
	result := &DownloadResult{
		Path: path, Filename: filename, ID: filename,
		SizeBytes: int64(len(content)), Lines: written, Truncated: truncatedBySize,
		Note: note, MaxChunkBytes: MaxDownloadChunk,
	}
	if !req.Chunked {
		result.DataBase64 = base64.StdEncoding.EncodeToString([]byte(content))
		result.NextOffset = result.SizeBytes
		result.EOF = true
		return result, nil
	}
	chunk, next, eof, cerr := readChunkAt(path, result.SizeBytes, req.Offset, req.Limit)
	if cerr != nil {
		return nil, cerr
	}
	result.Offset = req.Offset
	result.NextOffset = next
	result.EOF = eof
	result.ChunkBytes = len(chunk)
	result.DataBase64 = base64.StdEncoding.EncodeToString(chunk)
	return result, nil
}

// exportLines 取导出内容：行区间模式从文件头顺序读到 ToLine；默认模式取最近 need 行。
// 返回条目（旧→新）、截断说明与错误。
func (r *Registry) exportLines(entry *sourceEntry, req DownloadRequest, need int, rangeMode bool) ([]TailLine, string, *FaultError) {
	switch entry.spec.Type {
	case SourceTypeJournal:
		entries, _, truncated, lineTruncated, jerr := tailJournal(need, "", time.Time{}, false, newLineMatcher("", time.Time{}, false))
		if jerr != nil {
			return nil, "", jerr
		}
		return entries, journalDownloadNote(truncated, lineTruncated), nil
	default:
		file, ferr := r.openFile(entry)
		if ferr != nil {
			return nil, "", ferr
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return nil, "", methodErr(CodeInternal, "读取来源元信息失败")
		}
		if rangeMode {
			entries, truncated, lineTruncated, rerr := readLineRange(file, req.FromLine, req.ToLine)
			if rerr != nil {
				return nil, "", rerr
			}
			var note string
			if truncated {
				note = "行区间超过 64 MiB 扫描预算或行数上限，已截断"
			}
			if lineTruncated {
				note = joinNote(note, "部分超长行已截断（单行上限 8 KiB）")
			}
			return entries, note, nil
		}
		entries, _, _, lineTruncated, terr := tailFileContent(file, info.Size(), need, newLineMatcher("", time.Time{}, false))
		if terr != nil {
			return nil, "", terr
		}
		var note string
		if lineTruncated {
			note = "部分超长行已截断（单行上限 8 KiB）"
		}
		return entries, note, nil
	}
}

// validateLineRange 校验行区间入参：负数、to<from、跨度超 10000 行都拒绝。
func validateLineRange(from, to int64) *FaultError {
	if from < 0 || to < 0 {
		return methodErr(CodeInvalid, "from_line / to_line 不能为负数")
	}
	if from == 0 && to > 0 {
		return methodErr(CodeInvalid, "to_line 必须与 from_line 一起使用")
	}
	if from > 0 && to > 0 && to < from {
		return methodErr(CodeInvalid, "to_line 不能小于 from_line")
	}
	if from > 0 && to == 0 {
		to = from + MaxDownloadLines - 1
	}
	if from > 0 && to-from+1 > MaxDownloadLines {
		return methodErr(CodeInvalid, fmt.Sprintf("行区间最多 %d 行", MaxDownloadLines))
	}
	return nil
}

// buildExportContent 把行内容拼成导出文本：总长不超过 5 MiB，超限截断并返回标记。
func buildExportContent(lines []TailLine) (content string, written int, truncated bool) {
	var builder strings.Builder
	for _, line := range lines {
		if int64(builder.Len())+int64(len(line.Text))+1 > MaxDownloadBytes {
			truncated = true
			break
		}
		builder.WriteString(line.Text)
		builder.WriteByte('\n')
		written++
	}
	return builder.String(), written, truncated
}

// readLineRange 从文件头顺序读取 [from,to] 行（1 起，含两端），返回旧→新条目；
// 受 64 MiB 扫描预算与 MaxDownloadLines 行数上限约束，超限置 truncated。
func readLineRange(file *os.File, from, to int64) ([]TailLine, bool, bool, *FaultError) {
	if from < 1 {
		from = 1
	}
	if to == 0 {
		to = from + MaxDownloadLines - 1
	}
	limit := to - from + 1
	if limit > MaxDownloadLines {
		limit = MaxDownloadLines
	}
	entries := make([]TailLine, 0, limit)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, false, methodErr(CodeInternal, "定位来源文件失败")
	}
	reader := bufio.NewReaderSize(file, ReadBlockSize)
	var lineNo, scanned int64
	truncated := false
	lineTruncated := false
	for {
		raw, truncLine, n, err := readCappedLine(reader)
		scanned += n
		if n > 0 {
			lineNo++
			if lineNo >= from && lineNo <= to {
				if truncLine {
					lineTruncated = true
				}
				entries = append(entries, tailLineOf(raw, truncLine))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false, false, methodErr(CodeInternal, "读取来源文件失败")
		}
		if scanned >= MaxScanBytes {
			truncated = true
			break
		}
		if lineNo >= to {
			break
		}
	}
	return entries, truncated, lineTruncated, nil
}

// readExportChunk 读取既有导出的一个分块：id 必须是导出目录内的常规文件，
// offset 不得为负、不得超过文件大小，limit 缺省 1 MiB、上限 4 MiB。
func readExportChunk(stateDir string, req DownloadRequest, id string) (*DownloadResult, *FaultError) {
	path, info, ferr := resolveExportFile(stateDir, id)
	if ferr != nil {
		return nil, ferr
	}
	if req.Offset < 0 {
		return nil, methodErr(CodeInvalid, "offset 不能为负数")
	}
	if req.Offset > info.Size() {
		return nil, methodErr(CodeInvalid, "offset 超过导出文件大小")
	}
	chunk, next, eof, cerr := readChunkAt(path, info.Size(), req.Offset, req.Limit)
	if cerr != nil {
		return nil, cerr
	}
	return &DownloadResult{
		Path: path, Filename: id, ID: id, SizeBytes: info.Size(),
		DataBase64: base64.StdEncoding.EncodeToString(chunk),
		Offset:     req.Offset, NextOffset: next, EOF: eof, ChunkBytes: len(chunk),
		MaxChunkBytes: MaxDownloadChunk,
		Note:          "分块模式不统计总行数，请按 next_offset/eof 继续取下一块",
	}, nil
}

// readChunkAt 从 path 读取 [offset, offset+limit) 一段；limit 0 取默认 1 MiB。
func readChunkAt(path string, size, offset, limit int64) ([]byte, int64, bool, *FaultError) {
	if limit == 0 {
		limit = DefaultDownloadChunk
	}
	if limit < 0 || limit > MaxDownloadChunk {
		return nil, 0, false, methodErr(CodeInvalid, "limit 必须在 1–4194304 字节之间（不传按 1 MiB）")
	}
	if offset < 0 || offset > size {
		return nil, 0, false, methodErr(CodeInvalid, "offset 超出文件范围")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, methodErr(CodeInternal, "打开导出文件失败")
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, false, methodErr(CodeInternal, "定位导出文件偏移失败")
	}
	chunk, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, 0, false, methodErr(CodeInternal, "读取导出文件失败")
	}
	next := offset + int64(len(chunk))
	return chunk, next, next >= size, nil
}

// resolveExportFile 在 <state>/exports 内按 id 解析导出文件，并做三重归属校验：
// 文件名单段 + Lstat 非符号链接 + EvalSymlinks 后父目录仍是导出目录。
func resolveExportFile(stateDir, id string) (string, os.FileInfo, *FaultError) {
	if perr := validateExportID(id); perr != nil {
		return "", nil, perr
	}
	dir := filepath.Join(stateDir, exportsDirName)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return "", nil, methodErr(CodeNotFound, "导出目录不存在，请先调用 logs.download 生成导出文件")
		}
		return "", nil, methodErr(CodeInternal, "解析导出目录失败")
	}
	target := filepath.Join(realDir, id)
	if filepath.Dir(target) != realDir {
		return "", nil, methodErr(CodeInvalid, "导出 id 非法：越出导出目录")
	}
	before, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return "", nil, methodErr(CodeNotFound, "导出文件不存在："+id)
		}
		return "", nil, methodErr(CodeInternal, "读取导出文件元信息失败")
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return "", nil, methodErr(CodeInvalid, "导出文件是符号链接，已拒绝读取")
	}
	if !before.Mode().IsRegular() {
		return "", nil, methodErr(CodeInvalid, "导出目标不是常规文件")
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil || filepath.Dir(realTarget) != realDir {
		return "", nil, methodErr(CodeInvalid, "导出文件经符号链接解析后越出导出目录，已拒绝")
	}
	return target, before, nil
}

// validateExportID 校验导出 id：非空、限长、单段文件名、不含 ..。
func validateExportID(id string) *FaultError {
	if id == "" {
		return methodErr(CodeInvalid, "id 必填")
	}
	if len(id) > MaxExportIDLen {
		return methodErr(CodeInvalid, "id 超过 128 字节上限")
	}
	if strings.Contains(id, "..") || !exportIDPattern.MatchString(id) {
		return methodErr(CodeInvalid, "id 非法：只允许字母数字与 . _ -（不得含路径分隔符或 ..）")
	}
	return nil
}

// uniqueExportPath 在导出目录内生成不冲突的 <source>-<UTC时间戳>.log 路径。
func uniqueExportPath(dir, source string) (path, filename string) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	base := safeExportName(source) + "-" + stamp
	filename = base + ".log"
	path = filepath.Join(dir, filename)
	for i := 1; i <= 100; i++ {
		if _, err := os.Lstat(path); errors.Is(err, iofs.ErrNotExist) {
			return path, filename
		}
		filename = fmt.Sprintf("%s-%d.log", base, i)
		path = filepath.Join(dir, filename)
	}
	return path, filename
}

// joinNote 拼接截断说明（空串时直接返回新说明）。
func joinNote(note, extra string) string {
	if note == "" {
		return extra
	}
	return note + "；" + extra
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

// writeFilePrivate 以 0600 原子写目标文件：先写同目录唯一临时文件再 rename，
// 中途失败不留下半截导出文件。临时名必须唯一（固定 <path>.tmp 会让同目标并发导出互相踩踏）。
func writeFilePrivate(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
