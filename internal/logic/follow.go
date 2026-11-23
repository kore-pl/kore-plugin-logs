package logic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// followCursorVersion 是游标编码版本；版本不符的游标按失效处理（回退尾部窗口并重建）。
const followCursorVersion = 1

// followCursor 是 logs.follow 游标的明文结构：来源名 + 已消费字节偏移 + 最近一次读取时的文件大小。
// 游标对调用方是不透明字符串；服务端用它判断「同一来源 + 偏移是否仍有效」。
type followCursor struct {
	Version  int    `json:"v"`
	Source   string `json:"source"`
	Position int64  `json:"position"`
	Size     int64  `json:"size"`
}

// Follow 实现 logs.follow：带游标的增量读取，**不启动 ticker、不起后台 goroutine、不做推送**，
// 每次调用只读一次文件（G-19：空闲 CPU/定时器为 0，是否继续拉取由调用方按需决定）。
//
// 语义：
//   - 空 cursor：读最近 MaxLines 行建立初始游标（position=文件尾）；
//   - cursor 有效且 position ≤ 当前大小：从 position 前向读取，只回完整行，不重复下发；
//   - 文件轮转/截断（position > 当前大小）或游标与来源不匹配：重置为尾部窗口并置 reset=true；
//   - eof=true 表示当前游标已消费到文件尾；末尾不完整行不下发、等补全后再取（position 不推进）。
func (r *Registry) Follow(req FollowRequest) (*FollowResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	if perr := checkKeywordLen(req.Keyword); perr != nil {
		return nil, perr
	}
	maxLines, maxBytes, perr := effectiveFollowLimits(req.MaxLines, req.MaxBytes)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	if entry.spec.Type == SourceTypeJournal {
		return nil, methodErr(CodeUnsupported, "journal 来源没有稳定字节偏移，不支持增量游标；请用 logs.tail")
	}
	file, ferr := r.openFile(entry)
	if ferr != nil {
		return nil, ferr
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, methodErr(CodeInternal, "读取来源元信息失败")
	}
	debugf(req.Debugf, "logs.follow source=%s cursor_len=%d size=%d", entry.spec.Name, len(req.Cursor), info.Size())

	result := &FollowResult{
		Source: entry.spec.Name, File: entry.file.base, Keyword: req.Keyword,
		Entries: make([]TailLine, 0, maxLines),
	}
	matcher := newLineMatcher(req.Keyword, time.Time{}, false)
	cursor, resetReason := decodeFollowCursor(req.Cursor, req.Source)
	size := info.Size()

	if cursor != nil && cursor.Position > size {
		// 文件被轮转或截断：旧偏移已失效，回退尾部窗口重新建立游标。
		cursor = nil
		resetReason = "文件已轮转或截断（游标偏移超过当前大小），已重置"
	}
	if cursor == nil {
		entries, _, _, lineTruncated, terr := tailFileContent(file, size, maxLines, matcher)
		if terr != nil {
			return nil, terr
		}
		result.Entries = entries
		result.Position = size
		result.EOF = true
		result.LineTruncated = lineTruncated
		if req.Cursor != "" {
			result.Reset = true
			result.Note = resetReason
		}
		result.Count = len(entries)
		result.Cursor = encodeFollowCursor(entry.spec.Name, size, size)
		return result, nil
	}

	if cursor.Position == size {
		// 已消费到文件尾：无新增，游标保持不变（不空转、不扫描）。
		result.Position = size
		result.EOF = true
		result.Cursor = encodeFollowCursor(entry.spec.Name, size, size)
		return result, nil
	}
	entries, position, scanned, truncated, lineTruncated, terr := followForward(file, cursor.Position, size, maxLines, maxBytes, matcher)
	if terr != nil {
		return nil, terr
	}
	result.Entries = entries
	result.Position = position
	result.ScannedBytes = scanned
	result.Truncated = truncated
	result.LineTruncated = lineTruncated
	result.Count = len(entries)
	result.EOF = position == size
	if !result.EOF && !truncated {
		result.Note = "末尾存在未完成行，等补全后再拉取"
	}
	result.Cursor = encodeFollowCursor(entry.spec.Name, position, size)
	return result, nil
}

// followForward 从 start 前向读取到 size（单次最多 maxBytes 字节），只回完整行：
// 命中行最多保留 maxLines 条，position 推进到最后一条完整行之后；达到行数上限或窗口耗尽
// 而仍有未读内容时置 truncated。返回：条目（旧→新）、新偏移、实际扫描字节、截断、行截断。
func followForward(file *os.File, start, size int64, maxLines int, maxBytes int64, matcher lineMatcher) ([]TailLine, int64, int64, bool, bool, *FaultError) {
	entries := make([]TailLine, 0, maxLines)
	position := start
	if size <= start {
		return entries, start, 0, false, false, nil
	}
	window := size - start
	if window > maxBytes {
		window = maxBytes
	}
	windowEnd := start + window
	buf := make([]byte, ReadBlockSize)
	line := make([]byte, 0, 256)
	scratch := make([]byte, 0, MaxLineBytes)
	lineOverflow := false
	lineTruncated := false
	var read int64
	for read < window {
		n := int64(len(buf))
		if n > window-read {
			n = window - read
		}
		chunk := buf[:n]
		if _, err := file.ReadAt(chunk, start+read); err != nil && !errors.Is(err, io.EOF) {
			return nil, position, read, false, false, methodErr(CodeInternal, "读取来源文件失败")
		}
		chunkStart := start + read
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
			if lineOverflow {
				lineTruncated = true
			}
			matched, next := matcher.match(line, scratch)
			scratch = next
			if matched {
				entries = append(entries, tailLineOf(line, lineOverflow))
			}
			position = chunkStart + int64(i) + 1
			line = line[:0]
			lineOverflow = false
			if len(entries) >= maxLines {
				return entries, position, read, position < size, lineTruncated, nil
			}
		}
	}
	// 窗口读满但文件还有后续内容：未消费部分交给下一次调用（游标停在最后一条完整行后）。
	if position < size && windowEnd < size {
		return entries, position, read, true, lineTruncated, nil
	}
	return entries, position, read, false, lineTruncated, nil
}

// effectiveFollowLimits 校验单次增量上限：行数 0→200（1–500），字节 0→1 MiB（1–4 MiB）。
func effectiveFollowLimits(maxLines int, maxBytes int64) (int, int64, *FaultError) {
	if maxLines == 0 {
		maxLines = DefaultFollowLines
	}
	if maxLines < 1 || maxLines > MaxFollowLines {
		return 0, 0, methodErr(CodeInvalid, "max_lines 必须在 1 到 500 之间")
	}
	if maxBytes == 0 {
		maxBytes = DefaultFollowBytes
	}
	if maxBytes < 1 || maxBytes > MaxFollowBytes {
		return 0, 0, methodErr(CodeInvalid, "max_bytes 必须在 1 到 4194304 之间")
	}
	return maxLines, maxBytes, nil
}

// encodeFollowCursor 把「来源 + 偏移 + 文件大小」编码为不透明游标（base64url，无填充）。
func encodeFollowCursor(source string, position, size int64) string {
	raw, err := json.Marshal(followCursor{Version: followCursorVersion, Source: source, Position: position, Size: size})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeFollowCursor 解析游标：空串返回 (nil, "")；非法/版本不符/来源不匹配返回 (nil, 原因)。
func decodeFollowCursor(cursor, source string) (*followCursor, string) {
	if cursor == "" {
		return nil, ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, "游标不是合法的 base64url 编码，已重置"
	}
	var decoded followCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, "游标内容非法，已重置"
	}
	if decoded.Version != followCursorVersion {
		return nil, "游标版本不支持，已重置"
	}
	if decoded.Source != source {
		return nil, "游标与来源不匹配，已重置"
	}
	if decoded.Position < 0 || decoded.Size < 0 {
		return nil, "游标偏移非法，已重置"
	}
	return &decoded, ""
}
