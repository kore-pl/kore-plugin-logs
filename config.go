package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-logs/internal/logic"
	"github.com/pengtang1024/kore-plugin-logs/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单 manifest.json（只含握手字段，不含 size_bytes，避免自指震荡）。
//
//go:embed manifest.json
var embeddedInfo []byte

// manifest 是运行期需要的清单字段，其余字段归内核校验。
type manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	RequiresKore string   `json:"requires_kore"`
	Capabilities []string `json:"capabilities"`
}

// fileValues 是 <state_dir>/plugins/logs/config.json 的显式形状（G-24：不引入 object）。
// sources 每项形如 "名称=绝对路径"；journal 来源写 "名称=journal"。
type fileValues struct {
	Sources []string `json:"sources"`
}

// config 是插件运行期配置：配置契约（config.json）+ 环境变量 + 内嵌清单。
type config struct {
	socket       string
	id           string
	token        string
	version      string
	requiresKore string
	capabilities []string
	startedAt    time.Time
	fault        string
	// stateDir 是插件状态目录（KORE_STATE_DIR），配置落盘为 <stateDir>/config.json。
	stateDir string
	// store 是配置文件的 0600 原子读写器。
	store *plugincfg.Store
	// values / source 是 config.get 的当前值与来源（file / env / default）。
	values fileValues
	source string
	// registry 是来源白名单；config.set 后在同一进程内替换，下一次方法调用即生效。
	registry *logic.Registry
}

// loadConfig 读取配置契约文件与环境变量，并按 file > env > default 解析来源白名单。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 manifest.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	if err := plugincfg.ValidateSchema(logSourcesSchema()); err != nil {
		return config{}, fmt.Errorf("config schema 非法: %w", err)
	}
	stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	store := plugincfg.NewStore(stateDir)
	values, source, registry, err := resolveLogValues(store)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		startedAt:    time.Now(),
		fault:        os.Getenv("KORE_FAULT"),
		stateDir:     stateDir,
		store:        store,
		values:       values,
		source:       source,
		registry:     registry,
	}
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}

// resolveLogValues 实现优先级 config.json > KORE_LOGS_SOURCES > 代码默认值。
// 配置文件损坏或非法时启动失败（不静默回退），避免用户以为保存生效了却跑着旧值。
func resolveLogValues(store *plugincfg.Store) (fileValues, string, *logic.Registry, error) {
	if store.Exists() {
		raw, _, err := store.Read()
		if err != nil {
			return fileValues{}, "", nil, err
		}
		var values fileValues
		if err := strictDecode(raw, &values); err != nil {
			return fileValues{}, "", nil, fmt.Errorf("解析插件配置 %s: %w", store.Path(), err)
		}
		specs, err := parseSourceList(values.Sources)
		if err != nil {
			return fileValues{}, "", nil, fmt.Errorf("插件配置 %s 非法: %w", store.Path(), err)
		}
		return fileValues{Sources: specsToList(specs)}, plugincfg.SourceFile, logic.NewRegistry(specs), nil
	}
	if raw := os.Getenv("KORE_LOGS_SOURCES"); strings.TrimSpace(raw) != "" {
		specs, err := logic.ParseSources(raw)
		if err != nil {
			return fileValues{}, "", nil, err
		}
		return fileValues{Sources: specsToList(specs)}, plugincfg.SourceEnv, logic.NewRegistry(specs), nil
	}
	return fileValues{Sources: []string{}}, plugincfg.SourceDefault, logic.NewRegistry(nil), nil
}

// strictDecode 解码 JSON 并拒绝未知字段与尾随内容，避免拼错字段被静默忽略。
func strictDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("JSON 对象之后存在多余内容")
	}
	return nil
}

// parseSourceList 把 "名称=路径" 文本列表转成来源配置；journal 来源写 "名称=journal"。
func parseSourceList(items []string) ([]logic.SourceSpec, error) {
	specs := make([]logic.SourceSpec, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		raw := strings.TrimSpace(item)
		if raw == "" {
			return nil, fmt.Errorf("第 %d 项日志来源为空；请删除空行或填写 名称=绝对路径", i+1)
		}
		name, target, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 项 %q 缺少 =；格式为 名称=绝对路径（journal 来源写 名称=journal）", i+1, raw)
		}
		name = strings.TrimSpace(name)
		target = strings.TrimSpace(target)
		if name == "" || target == "" {
			return nil, fmt.Errorf("第 %d 项 %q 的名称与路径都不能为空", i+1, raw)
		}
		if seen[name] {
			return nil, fmt.Errorf("第 %d 项来源名 %q 重复", i+1, name)
		}
		seen[name] = true
		spec := logic.SourceSpec{Name: name, Path: target, Type: logic.SourceTypeFile}
		if strings.EqualFold(target, "journal") {
			spec.Type = logic.SourceTypeJournal
			spec.Path = ""
		}
		specs = append(specs, spec)
	}
	if err := logic.ValidateSources(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// specsToList 把内部来源配置还原成配置契约的 "名称=路径" 文本形态。
func specsToList(specs []logic.SourceSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Type == logic.SourceTypeJournal {
			out = append(out, spec.Name+"=journal")
			continue
		}
		out = append(out, spec.Name+"="+spec.Path)
	}
	return out
}

// logSourcesSchema 是 config.get 返回的表单描述（字段类型见 docs/PLUGIN-CONFIG.md §2）。
func logSourcesSchema() plugincfg.Schema {
	return plugincfg.Schema{
		Title: "日志设置",
		Fields: []plugincfg.Field{{
			Key:         "sources",
			Label:       "日志来源",
			Type:        plugincfg.TypeList,
			Item:        plugincfg.TypeString,
			Default:     json.RawMessage(`[]`),
			Required:    false,
			Help:        "每项一条 <名称>=<绝对路径>，或 <名称>=journal；例：syslog=/var/log/syslog、journal=journal",
			Placeholder: "syslog=/var/log/syslog",
		}},
	}
}
