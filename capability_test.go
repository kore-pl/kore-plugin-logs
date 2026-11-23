package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"regexp"
	"testing"
)

// methodActionPattern 与内核 method_capabilities 校验用的动作段正则一致。
var methodActionPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`)

// semverPattern 与内核 manifest 校验用的版本正则同口径（测试不锁死具体版本号）。
var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// privilegedCaps 是协议允许出现在方法级能力表里的 6 项宿主能力。
var privilegedCaps = map[string]bool{
	"system:read": true, "system:admin": true,
	"net:read": true, "net:write": true,
	"proc:read": true, "proc:write": true,
}

// expectedMethodCaps 是本仓对外声明的方法级能力表（表驱动，改方法必须同步改这里）。
var expectedMethodCaps = map[string]string{
	"logs.sources":    "system:read",
	"logs.tail":       "system:read",
	"logs.search":     "system:read",
	"logs.stats":      "system:read",
	"logs.follow":     "system:read",
	"logs.download":   "system:read",
	"logs.rotate":     "system:admin",
	"logs.config.get": "system:read",
	"logs.config.set": "system:admin",
}

// readInfoManifest 读取 info.json 与 manifest.json（发布件与内嵌清单的版本必须同步）。
func readInfoManifest(t *testing.T) (map[string]json.RawMessage, []byte, map[string]json.RawMessage) {
	t.Helper()
	infoRaw, err := os.ReadFile("info.json")
	if err != nil {
		t.Fatalf("读 info.json 失败: %v", err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		t.Fatalf("解析 info.json 失败: %v", err)
	}
	manifestRaw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("读 manifest.json 失败: %v", err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("解析 manifest.json 失败: %v", err)
	}
	return info, infoRaw, manifest
}

// TestMethodCapabilitiesDeclared 校验 method_capabilities：表驱动一致性、取值合法、
// 覆盖全部已分发方法、与 manifest 版本同步。
func TestMethodCapabilitiesDeclared(t *testing.T) {
	info, infoRaw, manifest := readInfoManifest(t)

	var id, version, requiresKore string
	if err := json.Unmarshal(info["id"], &id); err != nil || id != "logs" {
		t.Fatalf("id=%q err=%v，期望 logs", id, err)
	}
	if err := json.Unmarshal(info["version"], &version); err != nil || !semverPattern.MatchString(version) {
		t.Fatalf("version=%q err=%v，期望 MAJOR.MINOR.PATCH", version, err)
	}
	if err := json.Unmarshal(info["requires_kore"], &requiresKore); err != nil || requiresKore == "" {
		t.Fatalf("requires_kore 缺失: %v", err)
	}
	var manifestVersion string
	if err := json.Unmarshal(manifest["version"], &manifestVersion); err != nil || manifestVersion != version {
		t.Fatalf("manifest.version=%q 与 info.version=%q 不一致（err=%v）", manifestVersion, version, err)
	}

	var declared map[string]string
	if err := json.Unmarshal(info["method_capabilities"], &declared); err != nil {
		t.Fatalf("解析 method_capabilities 失败: %v", err)
	}
	if len(declared) != len(expectedMethodCaps) {
		t.Fatalf("method_capabilities 条数=%d，期望 %d", len(declared), len(expectedMethodCaps))
	}
	for method, want := range expectedMethodCaps {
		got, ok := declared[method]
		if !ok {
			t.Fatalf("缺少方法级能力声明: %s", method)
		}
		if got != want {
			t.Fatalf("%s=%q，期望 %q", method, got, want)
		}
	}
	for method, capability := range declared {
		if !privilegedCaps[capability] {
			t.Fatalf("%s=%q 不是 6 项特权能力之一", method, capability)
		}
		action, ok := cutPrefix(method, id+".")
		if !ok || !methodActionPattern.MatchString(action) {
			t.Fatalf("方法名 %q 非法（须为 logs.<action>）", method)
		}
		// 除 config.get / config.set 外，声明的方法必须真的能分发（防声明了不存在的方法）。
		if action != "config.get" && action != "config.set" {
			if _, known := businessVerb(method, id); !known {
				t.Fatalf("%s 已声明但 businessVerb 不认识（声明与分发漂移）", method)
			}
		}
	}
	// 每个已分发业务方法都必须有方法级声明（不再回退插件级）。
	for _, verb := range []string{"sources", "tail", "search", "stats", "download", "follow", "rotate"} {
		if _, ok := declared["logs."+verb]; !ok {
			t.Fatalf("业务方法 logs.%s 缺少方法级声明", verb)
		}
	}

	// 重复键必须被拒绝：标准库 map 会静默后者覆盖，这里从原始 JSON 逐键计数。
	counts := methodCapabilityKeyCounts(t, infoRaw)
	for method, count := range counts {
		if count != 1 {
			t.Fatalf("method_capabilities 含重复键 %q（出现 %d 次）", method, count)
		}
	}
}

// TestManifestEmbeddedFields 校验内嵌 manifest 与 info.json 的握手字段语义一致（make dist 同口径；
// 两者缩进不同，按 JSON 值比较而不是字节比较）。
func TestManifestEmbeddedFields(t *testing.T) {
	info, _, manifest := readInfoManifest(t)
	for _, key := range []string{"id", "version", "protocol", "requires_kore", "capabilities"} {
		var infoValue, manifestValue any
		if err := json.Unmarshal(info[key], &infoValue); err != nil {
			t.Fatalf("解析 info.%s 失败: %v", key, err)
		}
		if err := json.Unmarshal(manifest[key], &manifestValue); err != nil {
			t.Fatalf("解析 manifest.%s 失败: %v", key, err)
		}
		if !reflect.DeepEqual(infoValue, manifestValue) {
			t.Fatalf("%s 不一致：info=%s manifest=%s", key, info[key], manifest[key])
		}
	}
}

// methodCapabilityKeyCounts 从原始 JSON 统计 method_capabilities 每个键的出现次数
// （用于发现重复键；json.Unmarshal 到 map 会静默覆盖，测不出来）。
func methodCapabilityKeyCounts(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	counts := map[string]int{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return counts
		}
		if err != nil {
			t.Fatalf("解析 info.json token 失败: %v", err)
		}
		key, ok := token.(string)
		if !ok {
			continue
		}
		if key != "method_capabilities" {
			continue
		}
		valueToken, err := decoder.Token()
		if err != nil {
			t.Fatalf("读 method_capabilities 起始 token 失败: %v", err)
		}
		delim, ok := valueToken.(json.Delim)
		if !ok || delim != '{' {
			return counts
		}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				t.Fatalf("读方法键失败: %v", err)
			}
			methodKey, _ := keyToken.(string)
			counts[methodKey]++
			var capability string
			if err := decoder.Decode(&capability); err != nil {
				t.Fatalf("读 %q 的能力值失败: %v", methodKey, err)
			}
		}
		if _, err := decoder.Token(); err != nil {
			t.Fatalf("读 method_capabilities 结束 token 失败: %v", err)
		}
	}
}

// cutPrefix 是 strings.CutPrefix 的本地版本（避免额外 import 干扰表驱动测试可读性）。
func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}
