package logic

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Tail 实现 logs.tail：从文件尾按 64 KiB 分块反向扫描行边界，凑够 N 行即停，
// keyword/since 在行内容上过滤；全程不整文件读取。journal 来源走 journalctl。
// 带 Position（file 来源）时改为从该偏移前向增量读取，只返回新增完整行，
// 并回传新的 Position 供下一次增量拉取。
func (r *Registry) Tail(req TailRequest) (*TailResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	need, perr := effectiveTailLines(req.Lines)
	if perr != nil {
		return nil, perr
	}
	if perr := checkKeywordLen(req.Keyword); perr != nil {
		return nil, perr
	}
	since, hasSince, perr := parseSince(req.Since)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	result := &TailResult{
		Source: entry.spec.Name, Requested: need, Keyword: req.Keyword,
		Entries: make([]TailLine, 0, need),
	}
	if hasSince {
		result.Since = since.Format(time.RFC3339Nano)
	}
	matcher := newLineMatcher(req.Keyword, since, hasSince)

	switch entry.spec.Type {
	case SourceTypeJournal:
		entries, scanned, truncated, lineTruncated, jerr := tailJournal(need, req.Keyword, since, hasSince, matcher)
		if jerr != nil {
			return nil, jerr
		}
		result.File = "journal"
		result.Entries = entries
		result.ScannedBytes = scanned
		result.Truncated = truncated
		result.LineTruncated = lineTruncated
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
		size := info.Size()
		debugf(req.Debugf, "logs.tail source=%s path=%s position=%d size=%d", entry.spec.Name, filepath.Join(entry.file.realDir, entry.file.base), req.Position, size)
		// 增量条件：position 有效且未超过当前文件大小；文件被轮转/截断（size < position）
		// 时退回全量反向扫描，position 重置为新文件尾。
		if req.Position > 0 && req.Position <= size {
			entries, position, scanned, truncated, lineTruncated, terr := tailIncremental(file, req.Position, size, need, matcher)
			if terr != nil {
				return nil, terr
			}
			result.File = entry.file.base
			result.Entries = entries
			result.Position = position
			result.ScannedBytes = scanned
			result.Truncated = truncated
			result.LineTruncated = lineTruncated
			break
		}
		entries, scanned, truncated, lineTruncated, terr := tailFileContent(file, size, need, matcher)
		if terr != nil {
			return nil, terr
		}
		result.File = entry.file.base
		result.Entries = entries
		result.Position = size
		result.ScannedBytes = scanned
		result.Truncated = truncated
		result.LineTruncated = lineTruncated
	}
	result.Count = len(result.Entries)
	return result, nil
}

// tailIncremental 从 start 偏移前向读取到 size，只保留**完整行**（以 \n 结尾），
// Position 推进到最后一条完整行之后；不完整的末尾行留给下一次增量（避免重复下发）。
// 新增内容超过 1 MiB 窗口时跳到最近 1 MiB（跳过部分置 truncated）；命中行超过 need
// 时只保留最新 need 条并置 truncated。返回：条目（旧→新）、新偏移、扫描字节数、截断标记。
func tailIncremental(file *os.File, start, size int64, need int, matcher lineMatcher) ([]TailLine, int64, int64, bool, bool, *FaultError) {
	entries := make([]TailLine, 0, need)
	if size <= start {
		// size == start：无新增；size < start 理论上不会到达（Tail 已回退全量）。
		return entries, start, 0, false, false, nil
	}
	scanned := size - start
	truncated := false
	if scanned > MaxIncrementalBytes {
		start = size - MaxIncrementalBytes
		scanned = size - start // 跳窗后按实际读取窗口上报
		truncated = true
	}
	buf := make([]byte, ReadBlockSize)
	var line []byte
	var scratch []byte
	lineOverflow := false
	lineTruncated := false
	dropped := false
	position := start
	var read int64
	for read < size-start {
		n := int64(len(buf))
		if remaining := size - start - read; n > remaining {
			n = remaining
		}
		chunk := buf[:n]
		if _, err := file.ReadAt(chunk, start+read); err != nil && !errors.Is(err, io.EOF) {
			return nil, position, 0, false, false, methodErr(CodeInternal, "读取来源文件失败")
		}
		read += n
		for i, c := range chunk {
			if c != '\n' {
				if len(line) < MaxLineBytes {
					line = append(line, c)
				} else {
					lineOverflow = true
				}
				continue
			}
			position = start + read - n + int64(i) + 1 // 本条完整行之后的偏移
			matched, next := matcher.match(line, scratch)
			scratch = next
			if lineOverflow {
				lineTruncated = true
			}
			if matched {
				if len(entries) >= need {
					copy(entries, entries[1:])
					entries[need-1] = tailLineOf(line, lineOverflow)
					dropped = true
					continue
				}
				entries = append(entries, tailLineOf(line, lineOverflow))
			}
			line = line[:0]
			lineOverflow = false
		}
	}
	return entries, position, scanned, truncated || dropped, lineTruncated, nil
}

// tailLineOf 由原始行字节构造带时间与级别的 TailLine。
func tailLineOf(raw []byte, truncated bool) TailLine {
	return TailLine{
		Text:      sanitizeLine(raw),
		Time:      lineTimeText(raw),
		Level:     parseLineLevel(raw),
		Truncated: truncated,
	}
}

// tailFileContent 从文件尾反向扫描行边界并逐行读取（最多 8 KiB/行），
// 直到凑够 need 条命中、到达文件开头或耗尽 64 MiB 扫描预算。
// 返回：条目（旧→新）、扫描字节数、是否因预算耗尽截断、是否有行超长截断。
func tailFileContent(file *os.File, size int64, need int, matcher lineMatcher) ([]TailLine, int64, bool, bool, *FaultError) {
	entries := make([]TailLine, 0, need)
	if size <= 0 {
		return entries, 0, false, false, nil
	}
	endsWithNewline, err := lastByteIsNewline(file, size)
	if err != nil {
		return nil, 0, false, false, methodErr(CodeInternal, "读取来源文件失败")
	}
	pos := size
	remaining := MaxScanBytes
	var scanned int64
	var newlines []int64
	atStart := false
	lineTruncated := false
	scratch := make([]byte, 0, MaxLineBytes)
	buf := make([]byte, ReadBlockSize)
	cache := blockCache{}
	lineIndex := 0
	truncated := false

	for {
		for len(entries) < need {
			start, end, ok := lineInterval(lineIndex, newlines, endsWithNewline, atStart, size)
			if !ok {
				break
			}
			lineIndex++
			raw, truncatedLine, perr := readLineAt(file, start, end, &cache)
			if perr != nil {
				return nil, scanned, false, false, perr
			}
			if truncatedLine {
				lineTruncated = true
			}
			matched, next := matcher.match(raw, scratch)
			scratch = next
			if !matched {
				continue
			}
			entries = append(entries, tailLineOf(raw, truncatedLine))
		}
		if len(entries) >= need || atStart || remaining <= 0 {
			truncated = len(entries) < need && !atStart && remaining <= 0
			break
		}
		n := int64(ReadBlockSize)
		if n > pos {
			n = pos
		}
		if n > remaining {
			n = remaining
		}
		start := pos - n
		chunk := buf[:n]
		if _, err := file.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, scanned, false, false, methodErr(CodeInternal, "读取来源文件失败")
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				newlines = append(newlines, start+int64(i))
			}
		}
		cache.start = start
		cache.data = append(cache.data[:0], chunk...)
		pos = start
		remaining -= n
		scanned += n
		atStart = pos == 0
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, scanned, truncated, lineTruncated, nil
}

// lineInterval 计算第 k 新行（0 起）的 [start,end) 区间。
// start 只有落在已扫描到的换行之后或文件开头时才已知；否则返回 ok=false。
func lineInterval(k int, newlines []int64, endsWithNewline, atStart bool, size int64) (start, end int64, ok bool) {
	if endsWithNewline {
		if k >= len(newlines) {
			return 0, 0, false
		}
		end = newlines[k]
		if k+1 < len(newlines) {
			return newlines[k+1] + 1, end, true
		}
		if atStart {
			return 0, end, true
		}
		return 0, 0, false
	}
	if k == 0 {
		end = size
	} else {
		if k-1 >= len(newlines) {
			return 0, 0, false
		}
		end = newlines[k-1]
	}
	if k < len(newlines) {
		return newlines[k] + 1, end, true
	}
	if atStart {
		return 0, end, true
	}
	return 0, 0, false
}

// lastByteIsNewline 探测文件最后一个字节是否为换行。
func lastByteIsNewline(file *os.File, size int64) (bool, error) {
	buf := make([]byte, 1)
	if _, err := file.ReadAt(buf, size-1); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return buf[0] == '\n', nil
}

// blockCache 保存最近一次读取的 64 KiB 块；反向扫描时绝大多数短行可直接命中内存，
// 避免逐行 syscall，只有跨越块边界的长行才回落到 ReadAt。
type blockCache struct {
	start int64
	data  []byte
}

// slice 在缓存覆盖 [start,end) 时返回对应切片。
func (c *blockCache) slice(start, end int64) ([]byte, bool) {
	if c.data == nil || start < c.start || end > c.start+int64(len(c.data)) {
		return nil, false
	}
	return c.data[start-c.start : end-c.start], true
}

// readLineAt 读取 [start,end) 的行内容，优先命中块缓存；超过 8 KiB 只保留前缀并标记截断。
func readLineAt(file *os.File, start, end int64, cache *blockCache) ([]byte, bool, *FaultError) {
	length := end - start
	if length <= 0 {
		return nil, false, nil
	}
	keep := length
	truncated := false
	if keep > MaxLineBytes {
		keep = MaxLineBytes
		truncated = true
	}
	stop := start + keep
	if raw, ok := cache.slice(start, stop); ok {
		return raw, truncated, nil
	}
	buf := make([]byte, keep)
	if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, false, methodErr(CodeInternal, "读取来源文件失败")
	}
	return buf, truncated, nil
}
