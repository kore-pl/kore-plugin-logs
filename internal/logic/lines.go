package logic

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"time"
)

// asciiLower 只折叠 ASCII 大写字母，供不区分大小写的子串匹配使用；
// 非 ASCII（如中文）原样保留，不做有损转换。
func asciiLower(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	buf := []byte(s)
	for i, c := range buf {
		if c >= 'A' && c <= 'Z' {
			buf[i] = c + ('a' - 'A')
		}
	}
	return string(buf)
}

// foldInto 把 src 的 ASCII 大写折叠进 dst 并返回 dst，避免每行分配。
func foldInto(dst, src []byte) []byte {
	dst = append(dst[:0], src...)
	for i, c := range dst {
		if c >= 'A' && c <= 'Z' {
			dst[i] = c + ('a' - 'A')
		}
	}
	return dst
}

// lineTimeLayouts 是行首时间戳的候选格式；带年份的才可比较，无年份的解析失败即跳过过滤。
var lineTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05.999999999",
	"2006/01/02 15:04:05",
}

// parseLineTime 取行首 1-2 个字段（兼容 "2026-11-22 10:00:00"）按候选格式解析；
// 解析失败返回 false，调用方跳过该行的 since 过滤（保留该行）。
func parseLineTime(raw []byte) (time.Time, bool) {
	for fields := 1; fields <= 2; fields++ {
		token := firstFields(raw, fields)
		if len(token) == 0 {
			continue
		}
		text := string(token)
		for _, layout := range lineTimeLayouts {
			if ts, err := time.ParseInLocation(layout, text, time.Local); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// firstFields 返回行首前 n 个以空格/制表符分隔的字段（含中间分隔符，不含尾部空白）。
func firstFields(raw []byte, n int) []byte {
	count := 0
	for i, c := range raw {
		if c == ' ' || c == '\t' {
			if i > 0 && raw[i-1] != ' ' && raw[i-1] != '\t' {
				count++
				if count == n {
					return raw[:i]
				}
			}
			continue
		}
	}
	return raw
}

// sanitizeLine 把行内容转为合法 UTF-8 字符串（非法字节替换为 U+FFFD）。
func sanitizeLine(raw []byte) string {
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

// lineMatcher 组合 keyword（ASCII 大小写不敏感子串）与 since（行首时间戳粗略过滤）。
type lineMatcher struct {
	keyword  []byte
	since    time.Time
	hasSince bool
}

// newLineMatcher 构造匹配器；keyword 为空表示不过滤。
func newLineMatcher(keyword string, since time.Time, hasSince bool) lineMatcher {
	return lineMatcher{keyword: []byte(asciiLower(keyword)), since: since, hasSince: hasSince}
}

// match 判断一行是否命中；since 解析失败的行跳过过滤（保留）。
func (m lineMatcher) match(raw []byte, scratch []byte) (bool, []byte) {
	if len(m.keyword) > 0 {
		scratch = foldInto(scratch, raw)
		if !bytes.Contains(scratch, m.keyword) {
			return false, scratch
		}
	}
	if m.hasSince {
		if ts, ok := parseLineTime(raw); ok && ts.Before(m.since) {
			return false, scratch
		}
	}
	return true, scratch
}

// countLines 统计行数：换行数 + （文件非空且末尾无换行时补 1）。
func countLines(reader io.Reader) (int64, error) {
	buf := make([]byte, ReadBlockSize)
	var lines int64
	var last byte
	var total int64
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			last = buf[n-1]
			lines += int64(bytes.Count(buf[:n], []byte{'\n'}))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if total > 0 && last != '\n' {
		lines++
	}
	return lines, nil
}

// readCappedLine 从 bufio.Reader 读一行，最多保留 MaxLineBytes 字节并返回截断标记。
// 返回的 total 是这一行实际读取的原始字节数（含换行）。
func readCappedLine(reader *bufio.Reader) (line []byte, truncated bool, total int64, err error) {
	line = make([]byte, 0, 256)
	for {
		frag, readErr := reader.ReadSlice('\n')
		total += int64(len(frag))
		if len(line) < MaxLineBytes {
			room := MaxLineBytes - len(line)
			take := len(frag)
			if take > room {
				take = room
			}
			line = append(line, frag[:take]...)
			if take < len(frag) {
				truncated = true
			}
		} else if len(frag) > 0 {
			truncated = true
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, truncated, total, readErr
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		return line, truncated, total, readErr
	}
}
