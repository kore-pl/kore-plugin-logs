package boundary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
)

// TestBoundaryMalformedSources 是「异常输入」代表用例：恶意/畸形的来源配置
// 必须在 ParseSources 被拒，且拒绝原因可定位（不能静默接受或降级成路径）。
func TestBoundaryMalformedSources(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"路径穿越", `[{"name":"app","path":"/var/log/../../etc/passwd","type":"file"}]`, "Clean"},
		{"相对路径", `[{"name":"app","path":"app.log","type":"file"}]`, "绝对路径"},
		{"NUL 字节", "[{\"name\":\"app\",\"path\":\"/var/log/app\\u0000.log\",\"type\":\"file\"}]", "NUL"},
		{"反斜杠", `[{"name":"app","path":"/var/log\\app.log","type":"file"}]`, "反斜杠"},
		{"来源名穿越", `[{"name":"../../etc/passwd","path":"/var/log/app.log","type":"file"}]`, "非法"},
		{"来源名重复", `[{"name":"app","path":"/var/log/a.log","type":"file"},{"name":"app","path":"/var/log/b.log","type":"file"}]`, "重复"},
		{"类型非法", `[{"name":"app","path":"/var/log/app.log","type":"socket"}]`, "非法"},
		{"file 无 path", `[{"name":"app","type":"file"}]`, "不能为空"},
		{"未知字段", `[{"name":"app","path":"/var/log/app.log","type":"file","pathx":"/etc/passwd"}]`, "unknown field"},
		{"JSON 后有多余内容", `[{"name":"app","path":"/var/log/app.log","type":"file"}] garbage`, "多余"},
		{"不是数组", `{"name":"app"}`, "JSON 数组"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			specs, err := logic.ParseSources(tc.raw)
			if err == nil {
				t.Fatalf("ParseSources 接受了畸形输入：specs=%+v", specs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误=%q 期望包含 %q（拒绝原因必须可定位）", err.Error(), tc.want)
			}
		})
	}
}

// TestBoundaryInvalidMethodParams 覆盖方法级异常入参：越界数值、未登记/路径形态来源、
// 破坏性操作缺 confirm、超大关键字——全部返回结构化 E_INVALID，且不产生副作用。
func TestBoundaryInvalidMethodParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registry, active := seedActive(t, dir, "app", []byte("2026-11-23T00:00:00Z INFO line\n"))
	stateDir := t.TempDir()

	cases := []struct {
		name string
		call func() *logic.FaultError
	}{
		{"tail lines=-1", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: -1})
			return ferr
		}},
		{"tail lines 超上限", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "app", Lines: logic.MaxTailLines + 1})
			return ferr
		}},
		{"tail keyword 超 256 字节", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "app", Keyword: strings.Repeat("k", 257)})
			return ferr
		}},
		{"tail since 非 RFC3339", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "app", Since: "yesterday"})
			return ferr
		}},
		{"search limit 为负", func() *logic.FaultError {
			_, ferr := registry.Search(logic.SearchRequest{Source: "app", Query: "x", Limit: -5})
			return ferr
		}},
		{"来源名带路径", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "/etc/passwd", Lines: 1})
			return ferr
		}},
		{"未登记来源", func() *logic.FaultError {
			_, ferr := registry.Tail(logic.TailRequest{Source: "not-registered", Lines: 1})
			return ferr
		}},
		{"rotate 缺 confirm", func() *logic.FaultError {
			_, ferr := registry.Rotate(logic.RotateRequest{Source: "app"})
			return ferr
		}},
		{"rotate keep=-1", func() *logic.FaultError {
			_, ferr := registry.Rotate(logic.RotateRequest{Source: "app", Confirm: true, Keep: -1})
			return ferr
		}},
		{"rotate keep 超上限", func() *logic.FaultError {
			_, ferr := registry.Rotate(logic.RotateRequest{Source: "app", Confirm: true, Keep: logic.MaxRotateKeep + 1})
			return ferr
		}},
		{"clean 缺 confirm", func() *logic.FaultError {
			_, ferr := registry.Clean(logic.CleanRequest{Source: "app", StateDir: stateDir})
			return ferr
		}},
		{"clean 缺 stateDir", func() *logic.FaultError {
			_, ferr := registry.Clean(logic.CleanRequest{Source: "app", DryRun: true})
			return ferr
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ferr := tc.call()
			requireCode(t, tc.name, ferr, logic.CodeInvalid)
		})
	}

	// 异常入参不得产生副作用：活跃文件原样、state 目录无 retention.json。
	data, err := os.ReadFile(active)
	if err != nil || string(data) != "2026-11-23T00:00:00Z INFO line\n" {
		t.Fatalf("异常入参改动了活跃文件：data=%q err=%v", data, err)
	}
	if names := listDirNames(t, stateDir); len(names) != 0 {
		t.Fatalf("异常入参在 state 目录留下文件：%v", names)
	}
	if names := listDirNames(t, filepath.Dir(active)); len(names) != 1 || names[0] != "app.log" {
		t.Fatalf("异常入参在来源目录留下额外文件：%v", names)
	}
}
