package logic

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Search 实现 logs.search：从文件头按 64 KiB 逐块扫描匹配行，返回行号、
// 命中内容、已扫描字节数；达到 limit 或 64 MiB 上限即截断。
func (r *Registry) Search(req SearchRequest) (*SearchResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	if perr := checkSourceParam(req.Source); perr != nil {
		return nil, perr
	}
	if strings.TrimSpace(req.Keyword) == "" {
		return nil, methodErr(CodeInvalid, "keyword 必填")
	}
	if perr := checkKeywordLen(req.Keyword); perr != nil {
		return nil, perr
	}
	limit, perr := effectiveSearchLimit(req.Limit)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	result := &SearchResult{
		Source: entry.spec.Name, Keyword: req.Keyword, Limit: limit,
		Matches: make([]SearchMatch, 0, limit),
	}
	switch entry.spec.Type {
	case SourceTypeJournal:
		matches, scanned, truncated, jerr := searchJournal(req.Keyword, limit)
		if jerr != nil {
			return nil, jerr
		}
		result.File = "journal"
		result.Matches = matches
		result.ScannedBytes = scanned
		result.Truncated = truncated
	default:
		file, ferr := r.openFile(entry)
		if ferr != nil {
			return nil, ferr
		}
		defer func() { _ = file.Close() }()
		debugf(req.Debugf, "logs.search source=%s path=%s", entry.spec.Name, filepath.Join(entry.file.realDir, entry.file.base))
		matches, scanned, truncated, serr := searchFileContent(file, req.Keyword, limit)
		if serr != nil {
			return nil, serr
		}
		result.File = entry.file.base
		result.Matches = matches
		result.ScannedBytes = scanned
		result.Truncated = truncated
	}
	result.Count = len(result.Matches)
	return result, nil
}

// searchFileContent 顺序分块扫描文件；每行最多保留 8 KiB，跨块行用 carry 拼接。
func searchFileContent(file *os.File, keyword string, limit int) ([]SearchMatch, int64, bool, *FaultError) {
	matches := make([]SearchMatch, 0, limit)
	keywordLower := []byte(asciiLower(keyword))
	scratch := make([]byte, 0, MaxLineBytes)
	buf := make([]byte, ReadBlockSize)
	line := make([]byte, 0, MaxLineBytes)
	overflow := false
	lineNo := 0
	var scanned int64
	eof := false

	for scanned < MaxScanBytes {
		n := len(buf)
		if int64(n) > MaxScanBytes-scanned {
			n = int(MaxScanBytes - scanned)
		}
		read, err := file.Read(buf[:n])
		if read > 0 {
			scanned += int64(read)
			data := buf[:read]
			for len(data) > 0 {
				idx := bytes.IndexByte(data, '\n')
				if idx < 0 {
					line, overflow = appendCapped(line, overflow, data)
					break
				}
				line, overflow = appendCapped(line, overflow, data[:idx])
				lineNo++
				matched, next := searchMatch(line, keywordLower, scratch)
				scratch = next
				if matched {
					matches = append(matches, SearchMatch{LineNo: lineNo, Text: sanitizeLine(line), Truncated: overflow})
					if len(matches) >= limit {
						return matches, scanned, true, nil
					}
				}
				line = line[:0]
				overflow = false
				data = data[idx+1:]
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				eof = true
				if len(line) > 0 || overflow {
					lineNo++
					if matched, _ := searchMatch(line, keywordLower, scratch); matched {
						matches = append(matches, SearchMatch{LineNo: lineNo, Text: sanitizeLine(line), Truncated: overflow})
					}
				}
				break
			}
			return nil, scanned, false, methodErr(CodeInternal, "读取来源文件失败")
		}
	}
	truncated := len(matches) >= limit || (!eof && scanned >= MaxScanBytes)
	return matches, scanned, truncated, nil
}

// appendCapped 把片段追加到行缓冲，最多保留 MaxLineBytes；超出部分丢弃并标记截断。
func appendCapped(line []byte, overflow bool, seg []byte) ([]byte, bool) {
	if overflow {
		return line, true
	}
	room := MaxLineBytes - len(line)
	if len(seg) <= room {
		return append(line, seg...), false
	}
	return append(line, seg[:room]...), true
}

// searchMatch 对行做 ASCII 大小写不敏感子串匹配，复用 scratch 避免逐行分配。
func searchMatch(line, keywordLower, scratch []byte) (bool, []byte) {
	scratch = foldInto(scratch, line)
	return bytes.Contains(scratch, keywordLower), scratch
}
