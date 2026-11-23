package logic

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Search 实现 logs.search：全文件按 64 KiB 逐块扫描，统计**全文命中总行数**（Total），
// Matches 只保留前 limit 条（含行号、行首时间、级别、命中内容）；达到 limit 不再存储但
// 继续扫描计数，64 MiB 扫描预算耗尽未到 EOF 即置 Truncated。keyword 为字面子串
// （大小写不敏感，等价 grep -iF，正则元字符按字面处理），journal 来源先经 journalctl
// --grep（QuoteMeta 字面化）预过滤再按同一口径复核。
func (r *Registry) Search(req SearchRequest) (*SearchResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	if perr := checkSourceParam(req.Source); perr != nil {
		return nil, perr
	}
	query := effectiveQuery(req)
	if strings.TrimSpace(query) == "" {
		return nil, methodErr(CodeInvalid, "query 必填")
	}
	if perr := checkKeywordLen(query); perr != nil {
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
		Source: entry.spec.Name, Keyword: query, Limit: limit,
		Matches: make([]SearchMatch, 0, limit),
	}
	switch entry.spec.Type {
	case SourceTypeJournal:
		matches, total, scanned, truncated, jerr := searchJournal(query, limit)
		if jerr != nil {
			return nil, jerr
		}
		result.File = "journal"
		result.Matches = matches
		result.Total = total
		result.ScannedBytes = scanned
		result.Truncated = truncated
	default:
		file, ferr := r.openFile(entry)
		if ferr != nil {
			return nil, ferr
		}
		defer func() { _ = file.Close() }()
		debugf(req.Debugf, "logs.search source=%s path=%s", entry.spec.Name, filepath.Join(entry.file.realDir, entry.file.base))
		matches, total, scanned, truncated, serr := searchFileContent(file, query, limit)
		if serr != nil {
			return nil, serr
		}
		result.File = entry.file.base
		result.Matches = matches
		result.Total = total
		result.ScannedBytes = scanned
		result.Truncated = truncated
	}
	result.Count = len(result.Matches)
	if result.Total > result.Count {
		result.Truncated = true
	}
	return result, nil
}

// effectiveQuery 解析检索词：0.3.0 起首选 query；0.2.x 的 keyword 继续兼容。
func effectiveQuery(req SearchRequest) string {
	if req.Query != "" {
		return req.Query
	}
	return req.Keyword
}

// searchFileContent 顺序分块扫描全文件；每行最多保留 8 KiB，跨块行用 carry 拼接。
// 达到 limit 后不再存储命中，但继续扫描统计 Total；返回：命中、全文命中总数、
// 已扫描字节数、是否因预算耗尽未读到 EOF。
func searchFileContent(file *os.File, keyword string, limit int) ([]SearchMatch, int, int64, bool, *FaultError) {
	matches := make([]SearchMatch, 0, limit)
	keywordLower := []byte(asciiLower(keyword))
	scratch := make([]byte, 0, MaxLineBytes)
	buf := make([]byte, ReadBlockSize)
	line := make([]byte, 0, MaxLineBytes)
	overflow := false
	lineNo := 0
	total := 0
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
					total++
					if len(matches) < limit {
						matches = append(matches, searchMatchOf(lineNo, line, overflow))
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
						total++
						if len(matches) < limit {
							matches = append(matches, searchMatchOf(lineNo, line, overflow))
						}
					}
				}
				break
			}
			return nil, 0, scanned, false, methodErr(CodeInternal, "读取来源文件失败")
		}
	}
	truncated := !eof
	return matches, total, scanned, truncated, nil
}

// searchMatchOf 由原始行构造带时间与级别的 SearchMatch。
func searchMatchOf(lineNo int, raw []byte, truncated bool) SearchMatch {
	return SearchMatch{
		LineNo:    lineNo,
		Text:      sanitizeLine(raw),
		Time:      lineTimeText(raw),
		Level:     parseLineLevel(raw),
		Truncated: truncated,
	}
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
