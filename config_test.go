package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
	"github.com/pengtang1024/kore-plugin-logs/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// newConfigForTest 构造只依赖临时状态目录的运行期配置（不读环境变量）。
func newConfigForTest(t *testing.T) *config {
	t.Helper()
	dir := t.TempDir()
	return &config{
		id:       "logs",
		store:    plugincfg.NewStore(dir),
		values:   fileValues{Sources: []string{}},
		source:   plugincfg.SourceDefault,
		registry: logic.NewRegistry(nil),
	}
}

// TestConfigGetDefault 校验未落盘时 source=default 且 schema 可直接渲染。
func TestConfigGetDefault(t *testing.T) {
	cfg := newConfigForTest(t)
	req := &pluginapi.Request{Method: "logs.config.get", Params: json.RawMessage(`{}`)}
	resp, _ := dispatch(req, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.get 失败: %v", resp.Error)
	}
	var got plugincfg.GetResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("解析结果: %v", err)
	}
	if got.Source != plugincfg.SourceDefault {
		t.Fatalf("source=%q，期望 default", got.Source)
	}
	if got.Schema.Title != "日志设置" || len(got.Schema.Fields) != 1 || got.Schema.Fields[0].Type != plugincfg.TypeList {
		t.Fatalf("schema 不符: %+v", got.Schema)
	}
	if string(got.Values) != `{"sources":[]}` {
		t.Fatalf("values=%s", got.Values)
	}
}

// TestConfigSetHotApply 校验 config.set 后同一进程内 logs.sources 立即使用新来源，
// 且配置文件 0600、source 变为 file。
func TestConfigSetHotApply(t *testing.T) {
	cfg := newConfigForTest(t)
	body := json.RawMessage(`{"values":{"sources":["syslog=/var/log/syslog"]}}`)
	req := &pluginapi.Request{Method: "logs.config.set", Params: body}
	resp, _ := dispatch(req, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.set 失败: %v", resp.Error)
	}
	var set plugincfg.SetResult
	if err := json.Unmarshal(resp.Result, &set); err != nil {
		t.Fatalf("解析 set 结果: %v", err)
	}
	if !set.Saved || !set.Applied || set.RestartRequired {
		t.Fatalf("set 结果=%+v", set)
	}
	if cfg.source != plugincfg.SourceFile {
		t.Fatalf("source=%q，期望 file", cfg.source)
	}
	info, err := os.Stat(cfg.store.Path())
	if err != nil {
		t.Fatalf("配置文件未落盘: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("配置文件权限=%o，期望 600", info.Mode().Perm())
	}
	if filepath.Base(cfg.store.Path()) != plugincfg.ConfigFileName {
		t.Fatalf("配置文件名=%s", cfg.store.Path())
	}
	// 不重启、不重载配置：直接调用业务方法，registry 已替换。
	sourcesResp, _ := dispatch(&pluginapi.Request{Method: "logs.sources", Params: json.RawMessage(`{}`)}, cfg, testLogger())
	if sourcesResp.Error != nil {
		t.Fatalf("logs.sources 失败: %v", sourcesResp.Error)
	}
	var sources logic.SourcesResult
	if err := json.Unmarshal(sourcesResp.Result, &sources); err != nil {
		t.Fatalf("解析 sources: %v", err)
	}
	if sources.Count != 1 || sources.Sources[0].Name != "syslog" {
		t.Fatalf("新来源未生效: %+v", sources)
	}
}

// TestConfigPriorityFileOverEnv 校验 config.json > KORE_LOGS_SOURCES。
func TestConfigPriorityFileOverEnv(t *testing.T) {
	dir := t.TempDir()
	store := plugincfg.NewStore(dir)
	if err := store.Write(json.RawMessage(`{"sources":["file-src=/tmp/file.log"]}`)); err != nil {
		t.Fatalf("预置配置: %v", err)
	}
	t.Setenv("KORE_LOGS_SOURCES", `[{"name":"env-src","path":"/tmp/env.log","type":"file"}]`)
	values, source, registry, err := resolveLogValues(store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != plugincfg.SourceFile {
		t.Fatalf("source=%q，期望 file", source)
	}
	if len(values.Sources) != 1 || values.Sources[0] != "file-src=/tmp/file.log" {
		t.Fatalf("values=%v", values.Sources)
	}
	result, ferr := registry.Sources(logic.FaultNone)
	if ferr != nil || result.Count != 1 || result.Sources[0].Name != "file-src" {
		t.Fatalf("registry 未按文件配置: %+v / %v", result, ferr)
	}
}

// TestConfigEnvCompat 校验老用户环境变量仍生效（source=env）。
func TestConfigEnvCompat(t *testing.T) {
	store := plugincfg.NewStore(t.TempDir())
	t.Setenv("KORE_LOGS_SOURCES", `[{"name":"env-src","path":"/tmp/env.log","type":"file"},{"name":"journal","type":"journal"}]`)
	values, source, registry, err := resolveLogValues(store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != plugincfg.SourceEnv {
		t.Fatalf("source=%q，期望 env", source)
	}
	if len(values.Sources) != 2 || values.Sources[1] != "journal=journal" {
		t.Fatalf("values=%v", values.Sources)
	}
	if got := registry; got == nil {
		t.Fatal("registry 为空")
	}
}

// TestConfigSetRejectsInvalid 校验非法值返回 E_INVALID，且不写文件、不改内存。
func TestConfigSetRejectsInvalid(t *testing.T) {
	cfg := newConfigForTest(t)
	req := &pluginapi.Request{Method: "logs.config.set", Params: json.RawMessage(`{"values":{"sources":["missing-equals"]}}`)}
	resp, _ := dispatch(req, cfg, testLogger())
	if resp.Error == nil || resp.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("期望 E_INVALID，得到 %+v", resp.Error)
	}
	if cfg.source != plugincfg.SourceDefault || len(cfg.values.Sources) != 0 {
		t.Fatalf("失败后内存被改动: %+v", cfg.values)
	}
	if _, err := os.Stat(cfg.store.Path()); !os.IsNotExist(err) {
		t.Fatalf("失败后不应写配置文件，Stat err=%v", err)
	}
}
