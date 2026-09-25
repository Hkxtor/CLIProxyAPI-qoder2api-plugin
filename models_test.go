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

	// 实时清单优先：注册 ID 用上游人类可读名，元数据（上下文/推理）保留。
	live, ok := byID["qo-Performance"]
	if !ok {
		t.Fatalf("cached live model missing: %v", keysOfModelInfo(catalog))
	}
	if live.Name != "gmodel" || live.DisplayName != "Performance" || live.ContextLength != 180000 || live.MaxCompletionTokens != 32768 {
		t.Fatalf("live model metadata lost: %+v", live)
	}
	if live.Thinking == nil {
		t.Fatal("reasoning model should expose thinking support")
	}
	// extra_models 是运维显式声明：即使与实时清单是同一个上游 SKU（gmodel）也照注册，
	// 这样才能用 `extra_models: ["qfmodel"]` 把旧版模型 ID 找回来。
	if _, ok := byID["qo-gmodel"]; !ok {
		t.Fatalf("extra_models 声明的模型应注册: %v", keysOfModelInfo(catalog))
	}
	// 而实时清单与兜底清单之间的 SKU 去重仍要生效（kmodel 在两边都有，只注册一次）。
	kmodelCount := 0
	for _, model := range catalog {
		if model.Name == "kmodel" {
			kmodelCount++
		}
	}
	if kmodelCount != 1 {
		t.Fatalf("同一上游 SKU 被重复注册 %d 次: %v", kmodelCount, keysOfModelInfo(catalog))
	}
	// 未启用的上游模型不注册。
	if _, ok := byID["qo-disabled-sku"]; ok {
		t.Fatal("disabled upstream model must not be registered")
	}
	// 兜底 SKU 与实时清单共存（kmodel 同时在两边出现、且上游没给 display_name，
	// 所以注册 ID 仍是 SKU 形式）。
	if _, ok := byID["qo-kmodel"]; !ok {
		t.Fatal("kmodel missing")
	}
	// 兜底清单里有名称的 SKU 用名称注册（拿不到实时清单时才走这条路）。
	if _, ok := byID["qo-Qwen3.8-Max"]; !ok {
		t.Fatalf("bundled SKU 应用人类可读名注册: %v", keysOfModelInfo(catalog))
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
		if model.ID == cfg.ModelPrefix+"Auto" {
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

// TestModelIDsUseHumanReadableNames 固化"客户端看到模型名而非上游 SKU"这个需求：
// CPA 的 /v1/models 只暴露模型 ID（宿主不给插件模型带 display_name），
// 所以可读性必须体现在 ID 本身，并且执行时能被还原成上游认识的 SKU。
func TestModelIDsUseHumanReadableNames(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)
	if errStore := storeCachedModels([]cachedModel{
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, IsReasoning: true},
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", Enable: true},
		{Key: "gmodel", Enable: true}, // 上游没给名称时退回 SKU
	}, "global"); errStore != nil {
		t.Fatalf("storeCachedModels: %v", errStore)
	}
	applyModelMappings(cfg)

	ep := func(name string) inspectModelCatalogEntry {
		catalog := buildModelCatalog(cfg)
		for _, model := range catalog {
			if model.ID == cfg.ModelPrefix+name {
				return inspectModelCatalogEntry{
					found:       true,
					sku:         model.Name,
					displayName: model.DisplayName,
					thinking:    model.Thinking != nil,
				}
			}
		}
		return inspectModelCatalogEntry{catalog: keysOfModelInfo(catalog)}
	}

	// 1) 注册 ID 是模型名。
	maxModel := ep("Qwen3.8-Max")
	if !maxModel.found {
		t.Fatalf("Qwen3.8-Max 未注册: %v", maxModel.catalog)
	}
	if maxModel.sku != "qmodel_38max" {
		t.Fatalf("Name 应保留上游 SKU: %q", maxModel.sku)
	}
	if !maxModel.thinking {
		t.Fatal("推理模型应保留 thinking 支持")
	}
	if flash := ep("Qwen3.8-Flash"); !flash.found || flash.sku != "qfmodel" {
		t.Fatalf("Qwen3.8-Flash 注册错误: %+v", flash)
	}
	// 上游没给名称 → 退回 SKU，不编名字。
	if noName := ep("gmodel"); !noName.found {
		t.Fatalf("缺少上游名称的模型应退回 SKU 注册: %v", noName.catalog)
	}

	// 2) 客户端拿注册 ID 请求时必须还原成上游 SKU。
	cases := map[string]string{
		"Qwen3.8-Max":   "qmodel_38max",
		"Qwen3.8-Flash": "qfmodel",
		// 插件侧仍能把旧 ID（上游 SKU）还原；但宿主路由表只含注册 ID，
		// 所以要用旧 ID 得在 extra_models 里显式声明（见下个用例）。
		"qmodel_38max":      "qmodel_38max",
		"qfmodel":           "qfmodel",
		"Kimi-K3":           "kmodel_latest", // 静态兜底别名
		"claude-sonnet-4-5": "gmodel",        // 内置关键字映射仍然生效
	}
	for input, want := range cases {
		if got := bridge.MapModel("", input); got != want {
			t.Fatalf("MapModel(%q) = %q, want %q", input, got, want)
		}
	}

	// 3) 前缀剥离后依然能映射（执行路径就是先 stripModelPrefix）。
	if got := bridge.MapModel("", stripModelPrefix(cfg.ModelPrefix, cfg.ModelPrefix+"Qwen3.8-Flash")); got != "qfmodel" {
		t.Fatalf("带前缀的请求未能还原 SKU: %q", got)
	}
}

// inspectModelCatalogEntry 是上面用例的取数辅助。
type inspectModelCatalogEntry struct {
	found       bool
	sku         string
	displayName string
	thinking    bool
	catalog     []string
}

// TestUserMappingOverridesDisplayAlias 用户规则优先于内置别名：
// 内置只负责"名称 → SKU"的还原，运维想改路由仍应说了算。
func TestUserMappingOverridesDisplayAlias(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t, func(cfg *pluginConfig) {
		// 带前缀的键也要生效（以前这种写法永远匹配不上）。
		cfg.ModelMappingRules = map[string]string{"qoder-Qwen3.8-Flash": "qmodel_38max"}
	})
	applyModelMappings(cfg)

	if got := bridge.MapModel("", "Qwen3.8-Flash"); got != "qmodel_38max" {
		t.Fatalf("带前缀的映射键未生效: %q", got)
	}
	// 另一个模型不受影响，仍走内置别名。
	if got := bridge.MapModel("", "GLM-5.3"); got != "gmodel" {
		t.Fatalf("其它模型的内置别名被破坏: %q", got)
	}
}

// TestExtraModelsCanRestoreLegacySKUIds 固化"旧 ID 兼容开关"：
// 宿主的路由表只认插件注册的 ID，所以想继续用 qoder-qfmodel 这类旧名字，
// 必须让插件显式把它注册回来。extra_models 是运维声明，不与实时清单做 SKU 去重。
func TestExtraModelsCanRestoreLegacySKUIds(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t, func(cfg *pluginConfig) {
		cfg.ExtraModels = []string{"qfmodel", "gmodel"}
	})
	if errStore := storeCachedModels([]cachedModel{
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", Enable: true},
		{Key: "gmodel", DisplayName: "GLM-5.3", Enable: true},
	}, "global"); errStore != nil {
		t.Fatalf("storeCachedModels: %v", errStore)
	}

	ids := map[string]bool{}
	for _, model := range buildModelCatalog(cfg) {
		ids[model.ID] = true
	}
	// 新 ID（模型名）与旧 ID（SKU）同时可用。
	for _, want := range []string{"qoder-Qwen3.8-Flash", "qoder-GLM-5.3", "qoder-qfmodel", "qoder-gmodel"} {
		if !ids[want] {
			t.Fatalf("缺少 %q：%v", want, keysOfModelInfo(buildModelCatalog(cfg)))
		}
	}
	// 旧 ID 在插件侧要能还原成上游 SKU。
	applyModelMappings(cfg)
	if got := bridge.MapModel("", "qfmodel"); got != "qfmodel" {
		t.Fatalf("旧 ID 未直通上游 SKU: %q", got)
	}
}
