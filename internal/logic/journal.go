package logic

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// journalBin 是 systemd journal 的读取入口；journal 来源完全依赖它，不自带解析器。
const journalBin = "journalctl"

const (
	journalProbeTimeout  = 3 * time.Second
	journalTailTimeout   = 10 * time.Second
	journalSearchTimeout = 15 * time.Second
)

// journalStatus 只读探测 journal 来源：journalctl 是否存在、当前用户能否读取。
func journalStatus(name string) SourceStatus {
	status := SourceStatus{Name: name, File: "journal", Type: SourceTypeJournal}
	if _, err := exec.LookPath(journalBin); err != nil {
		status.Note = "未安装 journalctl"
		return status
	}
	status.Exists = true
	if err := journalProbe(); err != nil {
		status.Note = "journalctl 不可用：当前用户可能无权读取 journal"
		return status
	}
	status.Readable = true
	status.Note = "journal 不提供单文件大小与行数"
	return status
}

// journalProbe 执行 `journalctl -q --no-pager -n 0` 判断 journal 是否可读。
func journalProbe() error {
	ctx, cancel := context.WithTimeout(context.Background(), journalProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, journalBin, "-q", "--no-pager", "-n", "0")
	return cmd.Run()
}

// tailJournal 通过 journalctl 取最近 need 条（可按 since/keyword 过滤）；
// 流式读取并只保留最后 need 行原始内容（每行 ≤8 KiB），再在 Go 侧按同一口径复核。
func tailJournal(need int, keyword string, since time.Time, hasSince bool, matcher lineMatcher) ([]TailLine, int64, bool, bool, *FaultError) {
	args := []string{"-q", "--no-pager", "--output=short-iso", "-n", strconv.Itoa(need)}
	if hasSince {
		args = append(args, "--since="+since.Format(time.RFC3339))
	}
	if keyword != "" {
		args = append(args, "--case-sensitive=no", "--grep="+regexp.QuoteMeta(keyword))
	}
	ctx, cancel := context.WithTimeout(context.Background(), journalTailTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, journalBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, false, false, methodErr(CodeUpstream, "journalctl 无法启动")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, false, false, methodErr(CodeUpstream, "journalctl 无法启动")
	}
	reader := bufio.NewReaderSize(stdout, ReadBlockSize)
	type rawLine struct {
		text  []byte
		trunc bool
	}
	window := make([]rawLine, 0, need)
	var scanned int64
	truncated := false
	for {
		line, truncatedLine, n, readErr := readCappedLine(reader)
		scanned += n
		if n > 0 {
			window = append(window, rawLine{text: line, trunc: truncatedLine})
			if len(window) > need {
				window = window[1:]
			}
		}
		if readErr != nil {
			break
		}
		if scanned >= MaxScanBytes {
			truncated = true
			cancel()
			break
		}
	}
	if err := cmd.Wait(); err != nil && !truncated {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, scanned, false, false, methodErr(CodeUpstream, "journalctl 读取超时")
		}
		return nil, scanned, false, false, methodErr(CodeUpstream, "journalctl 读取失败")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, scanned, false, false, methodErr(CodeUpstream, "journalctl 读取超时")
	}

	entries := make([]TailLine, 0, need)
	scratch := make([]byte, 0, MaxLineBytes)
	lineTruncated := false
	for i := len(window) - 1; i >= 0 && len(entries) < need; i-- {
		if window[i].trunc {
			lineTruncated = true
		}
		matched, next := matcher.match(window[i].text, scratch)
		scratch = next
		if !matched {
			continue
		}
		entries = append(entries, TailLine{Text: sanitizeLine(window[i].text), Truncated: window[i].trunc})
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, scanned, truncated, lineTruncated, nil
}

// searchJournal 通过 journalctl 的 grep 预过滤后流式读取，达到 limit 或 64 MiB 即停。
func searchJournal(keyword string, limit int) ([]SearchMatch, int64, bool, *FaultError) {
	ctx, cancel := context.WithTimeout(context.Background(), journalSearchTimeout)
	defer cancel()
	args := []string{"-q", "--no-pager", "--output=short-iso", "--case-sensitive=no", "--grep=" + regexp.QuoteMeta(keyword)}
	cmd := exec.CommandContext(ctx, journalBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, false, methodErr(CodeUpstream, "journalctl 无法启动")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, false, methodErr(CodeUpstream, "journalctl 无法启动")
	}
	reader := bufio.NewReaderSize(stdout, ReadBlockSize)
	keywordLower := []byte(asciiLower(keyword))
	scratch := make([]byte, 0, MaxLineBytes)
	matches := make([]SearchMatch, 0, limit)
	var scanned int64
	lineNo := 0
	truncated := false
	for {
		line, lineTruncated, n, readErr := readCappedLine(reader)
		scanned += n
		if n > 0 {
			lineNo++
			matched, next := searchMatch(line, keywordLower, scratch)
			scratch = next
			if matched {
				matches = append(matches, SearchMatch{LineNo: lineNo, Text: sanitizeLine(line), Truncated: lineTruncated})
				if len(matches) >= limit {
					truncated = true
					cancel()
					break
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			cancel()
			_ = cmd.Wait()
			return nil, scanned, false, methodErr(CodeUpstream, "journalctl 读取失败")
		}
		if scanned >= MaxScanBytes {
			truncated = true
			cancel()
			break
		}
	}
	if err := cmd.Wait(); err != nil && !truncated {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, scanned, false, methodErr(CodeUpstream, "journalctl 查询超时")
		}
		return nil, scanned, false, methodErr(CodeUpstream, "journalctl 查询失败")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, scanned, false, methodErr(CodeUpstream, "journalctl 查询超时")
	}
	return matches, scanned, truncated, nil
}
