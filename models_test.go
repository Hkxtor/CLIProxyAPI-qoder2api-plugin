package main

import (
	"encoding/json"
	"strings"
	"testing"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
)

func TestStripModelPrefix(t *testing.T) {
	cases := []struct {
		prefix string
		model  string
		want   string
	}{
		{"qoder-", "qoder-gmodel", "gmodel"},
		{"qoder-", "gmodel", "gmodel"},
		{"", "qoder-gmodel", "qoder-gmodel"},
		{"qoder-", "  qoder-claude-sonnet  ", "claude-sonnet"},
	}
	for _, tc := range cases {
		if got := stripModelPrefix(tc.prefix, tc.model); got != tc.want {
			t.Errorf("stripModelPrefix(%q,%q) = %q, want %q", tc.prefix, tc.model, got, tc.want)
		}
	}
}

func TestBuildModelCatalogPrefixesAndDeduplicates(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t, func(cfg *pluginConfig) {
		cfg.ModelPrefix = "qo-"
		cfg.ExtraModels = []string{"my-model", "gmodel"}
	})
	if errStore := storeCachedModels([]cachedModel{
		{Key: "gmodel", DisplayName: "Performance", Enable: true, IsReasoning: true, ContextWindow: 180000, MaxOutputTokens: 32768},
		{Key: "kmodel", Enable: true},
		{Key: "disabled-sku", Enable: false},
	}, "global"); errStore != nil {
		t.Fatalf("storeCachedModels: %v", errStore)
	}

	catalog := buildModelCatalog(cfg)
	byID := map[string]pluginapi.ModelInfo{}
	for _, model := range catalog {
		if _, exists := byID[model.ID]; exists {
			t.Fatalf("duplicate model id %q", model.ID)
		}
		byID[model.ID] = model
	}

	// 实时清单优先：使用上游 display_name / 上下文窗口。
	live, ok := byID["qo-gmodel"]
	if !ok {
		t.Fatalf("cached live model missing: %v", keysOfModelInfo(catalog))
	}
	if live.DisplayName != "Performance" || live.ContextLength != 180000 || live.MaxCompletionTokens != 32768 {
		t.Fatalf("live model metadata lost: %+v", live)
	}
	if live.Thinking == nil {
		t.Fatal("reasoning model should expose thinking support")
	}
	// 未启用的上游模型不注册。
	if _, ok := byID["qo-disabled-sku"]; ok {
		t.Fatal("disabled upstream model must not be registered")
	}
	// 兜底 SKU 与实时清单共存（kmodel 同时在两边出现，只注册一次）。
	if _, ok := byID["qo-kmodel"]; !ok {
		t.Fatal("kmodel missing")
	}
	// 别名与 extra_models 都带前缀。
	if _, ok := byID["qo-claude-sonnet"]; !ok {
		t.Fatalf("curated alias missing: %v", keysOfModelInfo(catalog))
	}
	if _, ok := byID["qo-my-model"]; !ok {
		t.Fatal("extra_models entry missing")
	}
	// 所有 ID 都必须带前缀，避免被 CPA 的原生 provider 抢走（modelHasNativeExecutor 会跳过冲突模型）。
	for _, model := range catalog {
		if !strings.HasPrefix(model.ID, "qo-") {
			t.Fatalf("model %q is not prefixed", model.ID)
		}
		if model.OwnedBy != providerKey || model.Type != providerKey {
			t.Fatalf("model %q has unexpected ownership: %+v", model.ID, model)
		}
	}
}

func TestApplyModelMappingsFeedsBridge(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t, func(cfg *pluginConfig) {
		cfg.ModelMappingRules = map[string]string{"my-fancy-model": "performance"}
	})
	applyModelMappings(cfg)
	if mapped := bridge.MapModel("codex", "my-fancy-model"); mapped != "performance" {
		t.Fatalf("bridge.MapModel = %q, want performance", mapped)
	}

	// 页面设置里的映射同样生效（页面优先于配置）。
	if errMutate := mutateState(func(state *pluginState) {
		state.ModelMapping = map[string]string{"my-fancy-model": "ultimate"}
	}); errMutate != nil {
		t.Fatalf("mutateState: %v", errMutate)
	}
	applyModelMappings(cfg)
	if mapped := bridge.MapModel("codex", "my-fancy-model"); mapped != "ultimate" {
		t.Fatalf("bridge.MapModel = %q, want ultimate（页面设置应优先）", mapped)
	}
}

// TestBuildModelCatalogFallsBackWithoutLiveData 固化"无实时清单也能用"：
// 冷启动（还没有任何账号/网络）时必须注册兜底 SKU，否则 CPA 不会绑定执行器。
func TestBuildModelCatalogFallsBackWithoutLiveData(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)
	catalog := buildModelCatalog(cfg)
	if len(catalog) == 0 {
		t.Fatal("catalog must not be empty: CPA 需要静态模型来绑定插件执行器")
	}
	found := false
	for _, model := range catalog {
		if model.ID == cfg.ModelPrefix+"auto" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bundled SKU list missing: %v", keysOfModelInfo(catalog))
	}
}

func TestRefreshModelsFromUpstreamCachesCatalog(t *testing.T) {
	host := installFakeHost(t)
	cfg := setupTestPlugin(t)
	setupQoderUpstream(host, "")
	host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "qoder-main", AuthIndex: "idx-1", Name: "qoder-main.json", Type: providerKey}}
	host.authJSON["idx-1"] = `{"type":"qoder","token":"pt-test-token","region":"global"}`

	count, errRefresh := refreshModelsFromUpstream(newTestContext(), "")
	if errRefresh != nil {
		t.Fatalf("refreshModelsFromUpstream: %v", errRefresh)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	cached := cachedModels()
	if len(cached) != 1 || cached[0].Key != "gmodel" || cached[0].DisplayName != "Performance" {
		t.Fatalf("cached models = %+v", cached)
	}
	// 缓存必须落盘（state.json），重启后仍在。
	raw, errRead := json.Marshal(snapshotState().Models)
	if errRead != nil {
		t.Fatalf("marshal state models: %v", errRead)
	}
	if !strings.Contains(string(raw), "gmodel") {
		t.Fatalf("state snapshot missing cached models: %s", raw)
	}
	_ = cfg
}

func TestRefreshModelsWithoutAccounts(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	_, errRefresh := refreshModelsFromUpstream(newTestContext(), "")
	if errRefresh == nil {
		t.Fatal("refresh without accounts must fail with a clear error")
	}
	if !strings.Contains(errRefresh.Error(), "账号") {
		t.Fatalf("error should tell the operator what is missing: %v", errRefresh)
	}
}

func keysOfModelInfo(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}
