package logic

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// TestCountLines 覆盖行数统计的边界：空文件、无尾换行、空行。
func TestCountLines(t *testing.T) {
	cases := []struct {
		content string
		want    int64
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\n\n", 2},
		{"\n", 1},
	}
	for _, tc := range cases {
		got, err := countLines(strings.NewReader(tc.content))
		if err != nil {
			t.Fatalf("countLines(%q) 错误: %v", tc.content, err)
		}
		if got != tc.want {
			t.Fatalf("countLines(%q)=%d，期望 %d", tc.content, got, tc.want)
		}
	}
}

// TestReadCappedLine 覆盖超长行截断与后续行不受影响。
func TestReadCappedLine(t *testing.T) {
	long := strings.Repeat("x", MaxLineBytes+100)
	reader := bufio.NewReaderSize(strings.NewReader(long+"\nnext\n"), 1024)
	line, truncated, total, err := readCappedLine(reader)
	if err != nil {
		t.Fatalf("readCappedLine 错误: %v", err)
	}
	if !truncated || len(line) != MaxLineBytes || total != int64(len(long)+1) {
		t.Fatalf("len=%d truncated=%v total=%d", len(line), truncated, total)
	}
	next, truncated, _, err := readCappedLine(reader)
	if err != nil || string(next) != "next" || truncated {
		t.Fatalf("next=%q truncated=%v err=%v", next, truncated, err)
	}
}

// TestRouteTimeParsing 覆盖 since 依赖的行首时间戳解析。
func TestRouteTimeParsing(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
	}{
		{"2026-11-22T10:00:00Z rest", true},
		{"2026-11-22T10:00:00.123456789+08:00 rest", true},
		{"2026-11-22 10:00:00 rest", true},
		{"Nov 22 10:00:00 host", false},
		{"no-time", false},
		{"", false},
	}
	for _, tc := range cases {
		_, ok := parseLineTime([]byte(tc.line))
		if ok != tc.ok {
			t.Fatalf("parseLineTime(%q) ok=%v，期望 %v", tc.line, ok, tc.ok)
		}
	}
	ts, ok := parseLineTime([]byte("2026-11-22T10:00:00Z rest"))
	if !ok || !ts.Equal(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("ts=%v ok=%v", ts, ok)
	}
}

// TestASCIICaseFold 覆盖 keyword 的 ASCII 大小写折叠与中文原样保留。
func TestASCIICaseFold(t *testing.T) {
	if asciiLower("ERROR 错误") != "error 错误" {
		t.Fatalf("asciiLower=%q", asciiLower("ERROR 错误"))
	}
	scratch := foldInto(nil, []byte("AbC"))
	if string(scratch) != "abc" {
		t.Fatalf("foldInto=%q", scratch)
	}
	matcher := newLineMatcher("错误", time.Time{}, false)
	if matched, _ := matcher.match([]byte("中文错误信息"), nil); !matched {
		t.Fatal("中文 keyword 应命中")
	}
}

// TestSanitizeLine 覆盖非法 UTF-8 字节替换，保证 JSON 可序列化。
func TestSanitizeLine(t *testing.T) {
	got := sanitizeLine([]byte{0xff, 'a'})
	if !strings.Contains(got, "a") || !strings.Contains(got, "\uFFFD") {
		t.Fatalf("sanitizeLine=%q", got)
	}
}

// TestParseLineLevel 覆盖行首窗口的级别解析：规范大写词、词边界、否定样例。
func TestParseLineLevel(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"2026-11-23T10:00:00Z ERROR boom", "error"},
		{"2026-11-23 10:00:00 [ERROR] boom", "error"},
		{"2026-11-23 10:00:00 WARN disk 90%", "warn"},
		{"2026-11-23 10:00:00 WARNING disk 90%", "warn"},
		{"2026-11-23T10:00:00Z INFO started", "info"},
		{"2026-11-23 10:00:00 DEBUG trace x=1", "debug"},
		{"Nov 23 10:17:01 host CRON[123]: (root) CMD", ""},
		{"2026-11-23 10:00:00 INFORMATIONAL msg", ""},
		{"2026-11-23 10:00:00 ERRNO 12 happened", ""},
		{"2026-11-23 10:00:00 perror failed", ""},
		{"2026-11-23 10:00:00 error lowercase ignored", ""},
		{"error beyond window should be ignored: " + strings.Repeat("x", 64) + " ERROR", ""},
	}
	for _, tc := range cases {
		if got := parseLineLevel([]byte(tc.line)); got != tc.want {
			t.Fatalf("parseLineLevel(%q)=%q，期望 %q", tc.line, got, tc.want)
		}
	}
}

// TestParseLineTimeShortISO 覆盖 journalctl short-iso 的 +0800 无冒号偏移格式。
func TestParseLineTimeShortISO(t *testing.T) {
	ts, ok := parseLineTime([]byte("2026-11-23T10:17:01+0800 host sshd[1]: accept"))
	if !ok {
		t.Fatalf("short-iso 时间解析失败")
	}
	if got := ts.Format("2006-01-02T15:04:05Z07:00"); got != "2026-11-23T10:17:01+08:00" {
		t.Fatalf("time=%q，期望 2026-11-23T10:17:01+08:00", got)
	}
}

// TestLineTimeText 覆盖规范化输出与解析失败的空串。
func TestLineTimeText(t *testing.T) {
	if got := lineTimeText([]byte("2026-11-22T10:00:00Z INFO ok")); got != "2026-11-22T10:00:00Z" {
		t.Fatalf("time=%q，期望 RFC3339 规范形", got)
	}
	if got := lineTimeText([]byte("no timestamp here")); got != "" {
		t.Fatalf("time=%q，期望空串", got)
	}
}
