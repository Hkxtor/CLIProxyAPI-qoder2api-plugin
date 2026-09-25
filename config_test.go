package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qoder2api-plugin/internal/qoder"
)

func TestDecodeConfigParsesSupportedFields(t *testing.T) {
	raw := []byte(`
# 宿主追加的字段
enabled: true
priority: 3
region: cn
state_dir: "/var/lib/qoder2api-plugin"
model_prefix: "qo-"
extra_models: "claude-sonnet-4-5, my-model"
log_level: DEBUG
log_to_file: true
auto_checkin: true
auto_checkin_at: "07:30"
machine_salt: "abc123"
model_mapping: "claude-sonnet-4-5=gmodel,my-model=performance"
`)
	cfg, errDecode := decodeConfig(raw)
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if cfg.Region != qoder.RegionCN {
		t.Errorf("region = %q, want cn", cfg.Region)
	}
	if cfg.StateDir != "/var/lib/qoder2api-plugin" {
		t.Errorf("state_dir = %q", cfg.StateDir)
	}
	if cfg.ModelPrefix != "qo-" {
		t.Errorf("model_prefix = %q", cfg.ModelPrefix)
	}
	if len(cfg.ExtraModels) != 2 || cfg.ExtraModels[0] != "claude-sonnet-4-5" || cfg.ExtraModels[1] != "my-model" {
		t.Errorf("extra_models = %v", cfg.ExtraModels)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q", cfg.LogLevel)
	}
	if !cfg.LogToFile || !cfg.AutoCheckin {
		t.Errorf("log_to_file/auto_checkin = %v/%v", cfg.LogToFile, cfg.AutoCheckin)
	}
	if cfg.AutoCheckinAt != "07:30" {
		t.Errorf("auto_checkin_at = %q", cfg.AutoCheckinAt)
	}
	if cfg.MachineSalt != "abc123" {
		t.Errorf("machine_salt = %q", cfg.MachineSalt)
	}
	if got := cfg.ModelMappingRules["claude-sonnet-4-5"]; got != "gmodel" {
		t.Errorf("model_mapping[claude-sonnet-4-5] = %q", got)
	}
	if got := cfg.ModelMappingRules["my-model"]; got != "performance" {
		t.Errorf("model_mapping[my-model] = %q", got)
	}
}

func TestDecodeConfigRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"bad region":        "region: moon\n",
		"bad clock":         "auto_checkin_at: 25:99\n",
		"bad log level":     "log_level: verbose\n",
		"bad mapping":       "model_mapping: \"just-a-name\"\n",
		"bad mapping value": "model_mapping: \"name=\"\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, errDecode := decodeConfig([]byte(raw)); errDecode == nil {
				t.Fatalf("decodeConfig(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestDecodeConfigDefaultsAndUnknownKeys(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte("enabled: true\nfuture_field: whatever\nregion: global\n"))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if cfg.ModelPrefix != defaultModelPrefix {
		t.Errorf("model_prefix = %q, want %q", cfg.ModelPrefix, defaultModelPrefix)
	}
	if cfg.AutoCheckinAt != defaultCheckinAt {
		t.Errorf("auto_checkin_at = %q, want %q", cfg.AutoCheckinAt, defaultCheckinAt)
	}
	if cfg.AutoCheckin {
		t.Error("auto_checkin should default to false")
	}
	if cfg.StateDir == "" {
		t.Error("state_dir should default to a non-empty path")
	}
}

func TestSplitConfigLineHandlesCommentsAndQuotes(t *testing.T) {
	cases := []struct {
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{`region: cn # 国内`, "region", "cn", true},
		{`state_dir: "/tmp/x"`, "state_dir", "/tmp/x", true},
		{"  # comment", "", "", false},
		{"- item", "", "", false},
		{"no-colon", "", "", false},
		{"key:", "key", "", true},
	}
	for _, tc := range cases {
		key, value, ok := splitConfigLine(tc.line)
		if ok != tc.wantOK || key != tc.wantKey || value != tc.wantValue {
			t.Errorf("splitConfigLine(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.line, key, value, ok, tc.wantKey, tc.wantValue, tc.wantOK)
		}
	}
}

func TestParseClock(t *testing.T) {
	if hour, minute, errParse := parseClock("10:00"); errParse != nil || hour != 10 || minute != 0 {
		t.Errorf("parseClock(10:00) = %d,%d,%v", hour, minute, errParse)
	}
	if _, _, errParse := parseClock("10"); errParse == nil {
		t.Error("parseClock(10) should fail")
	}
	if _, _, errParse := parseClock("24:00"); errParse == nil {
		t.Error("parseClock(24:00) should fail")
	}
}

func TestEnsureMachineSaltIsStable(t *testing.T) {
	cfg := setupTestPlugin(t)

	first, errFirst := ensureMachineSalt(cfg)
	if errFirst != nil {
		t.Fatalf("ensureMachineSalt: %v", errFirst)
	}
	if len(first) < 32 {
		t.Fatalf("salt looks too short: %q", first)
	}
	second, errSecond := ensureMachineSalt(cfg)
	if errSecond != nil {
		t.Fatalf("ensureMachineSalt: %v", errSecond)
	}
	if first != second {
		t.Fatalf("salt changed between calls: %q -> %q（变更会让全部账号指纹漂移）", first, second)
	}
	raw, errRead := os.ReadFile(filepath.Join(cfg.StateDir, saltFileName))
	if errRead != nil {
		t.Fatalf("read salt file: %v", errRead)
	}
	if strings.TrimSpace(string(raw)) != first {
		t.Fatalf("salt file content = %q, want %q", strings.TrimSpace(string(raw)), first)
	}
}

func TestEnsureMachineSaltHonorsOverride(t *testing.T) {
	cfg := setupTestPlugin(t, func(cfg *pluginConfig) { cfg.MachineSalt = "explicit-salt" })
	salt, errSalt := ensureMachineSalt(cfg)
	if errSalt != nil {
		t.Fatalf("ensureMachineSalt: %v", errSalt)
	}
	if salt != "explicit-salt" {
		t.Fatalf("salt = %q, want explicit-salt", salt)
	}
	if _, errStat := os.Stat(filepath.Join(cfg.StateDir, saltFileName)); errStat == nil {
		t.Error("override should not write a salt file")
	}
}

// TestDecodeConfigAcceptsYAMLListForms 固化一个踩过的坑：
// `extra_models` 写成 YAML 列表（块列表或流式列表）时以前会被静默忽略 ——
// 宿主面板里显示配置已保存，插件却看不到，于是"配了不生效"。
// （真实复现：PATCH 传数组 → 宿主写成块列表 → 插件不再注册旧模型 ID。）
func TestDecodeConfigAcceptsYAMLListForms(t *testing.T) {
	forms := map[string]string{
		"标量字符串":    "extra_models: qfmodel,gmodel\n",
		"标量带空格":    "extra_models: qfmodel gmodel\n",
		"块列表":      "extra_models:\n- qfmodel\n- gmodel\n",
		"缩进块列表":    "extra_models:\n  - qfmodel\n  - gmodel\n",
		"块列表带引号":   "extra_models:\n  - \"qfmodel\"\n  - 'gmodel'\n",
		"流式列表":     "extra_models: [qfmodel, gmodel]\n",
		"流式列表带引号":  "extra_models: [\"qfmodel\", 'gmodel']\n",
		"列表后续跟其它键": "extra_models:\n- qfmodel\n- gmodel\nregion: cn\n",
	}
	for name, raw := range forms {
		cfg, errDecode := decodeConfig([]byte(raw))
		if errDecode != nil {
			t.Fatalf("%s: decodeConfig: %v", name, errDecode)
		}
		if len(cfg.ExtraModels) != 2 || cfg.ExtraModels[0] != "qfmodel" || cfg.ExtraModels[1] != "gmodel" {
			t.Fatalf("%s: ExtraModels = %v, want [qfmodel gmodel]", name, cfg.ExtraModels)
		}
	}

	// 列表字段后面的其它键仍要正常解析（不能被续行收集吃掉）。
	cfg, errDecode := decodeConfig([]byte("region: cn\nextra_models:\n- qfmodel\nlog_level: debug\nqueue_max_waits: 1\n"))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if cfg.Region != qoder.NormalizeRegion("cn") || cfg.LogLevel != "debug" || cfg.QueueMaxWaits != 1 {
		t.Fatalf("列表字段之后的键被破坏: region=%v log_level=%q queue_max_waits=%d", cfg.Region, cfg.LogLevel, cfg.QueueMaxWaits)
	}
	if len(cfg.ExtraModels) != 1 || cfg.ExtraModels[0] != "qfmodel" {
		t.Fatalf("ExtraModels = %v", cfg.ExtraModels)
	}
}
