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
		debugf(req.Debugf, "logs.tail source=%s path=%s", entry.spec.Name, filepath.Join(entry.file.realDir, entry.file.base))
		entries, scanned, truncated, lineTruncated, terr := tailFileContent(file, info.Size(), need, matcher)
		if terr != nil {
			return nil, terr
		}
		result.File = entry.file.base
		result.Entries = entries
		result.ScannedBytes = scanned
		result.Truncated = truncated
		result.LineTruncated = lineTruncated
	}
	result.Count = len(result.Entries)
	return result, nil
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
			entries = append(entries, TailLine{Text: sanitizeLine(raw), Truncated: truncatedLine})
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
