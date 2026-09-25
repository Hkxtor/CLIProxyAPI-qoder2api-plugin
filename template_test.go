package main

import (
	"strings"
	"testing"
)

// TestBuildTemplateBaseParsesEmbeddedTemplate 固化一个重要事实：
// baseprompt.json 里的 {UUIDn}/{TIME1} 占位符必须被替换后才是合法 JSON。
// qoder2api 用 `_ = json.Unmarshal(...)` 静默忽略解析失败，插件改为报错——
// 这个测试保证内嵌模板始终可用。
func TestBuildTemplateBaseParsesEmbeddedTemplate(t *testing.T) {
	template, errTemplate := buildTemplateBase()
	if errTemplate != nil {
		t.Fatalf("buildTemplateBase: %v", errTemplate)
	}
	if template == nil {
		t.Fatal("template is nil")
	}
	if _, ok := template["messages"]; !ok {
		t.Errorf("template has no messages field, keys=%v", keysOf(template))
	}
	modelConfig, ok := template["model_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("template.model_config is %T", template["model_config"])
	}
	if _, ok := modelConfig["key"]; !ok {
		t.Errorf("model_config has no key field: %v", modelConfig)
	}
	if _, ok := template["request_id"]; !ok {
		t.Errorf("template has no request_id field, keys=%v", keysOf(template))
	}
}

func TestBuildTemplateBaseProducesFreshUUIDs(t *testing.T) {
	first, errFirst := buildTemplateBase()
	if errFirst != nil {
		t.Fatalf("buildTemplateBase: %v", errFirst)
	}
	second, errSecond := buildTemplateBase()
	if errSecond != nil {
		t.Fatalf("buildTemplateBase: %v", errSecond)
	}
	if asString(first["request_set_id"]) == asString(second["request_set_id"]) {
		t.Error("两次构造的占位符应当各自独立（否则会把不同请求串成同一会话）")
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

func asString(value interface{}) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
