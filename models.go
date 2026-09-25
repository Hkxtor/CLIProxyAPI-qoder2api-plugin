package main

import (
	"context"
	"net/http"
	"strings"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
)

// 本文件负责"插件注册给 CPA 的模型清单"。
//
// 为什么模型 ID 必须带前缀：
// CPA 在 internal/pluginhost/adapters_executors.go 里会跳过"已被原生 provider 提供"
// 的模型 ID（modelHasNativeExecutor）。Qoder 的上游 SKU key（gmodel/dmodel/...）
// 本身不冲突，但如果我们注册 claude-sonnet-4-5 这种通用名，冲突会让本插件整个
// 执行器都注册不上。因此默认统一加 qoder- 前缀，执行时再剥掉。

// bundledSKUs 是上游 agent_chat_generation 的兜底 SKU 列表。
// 来源：移植自 qoder2api/internal/bridge/claude.go 的 HandleListModels 兜底清单，
// 不是凭空构造的；真实可用集合以控制台"刷新模型"拉到的实时清单为准。
var bundledSKUs = []string{
	"auto",
	"qmodel_latest", "qmodel", "qmodel_38max",
	"qfmodel", "q37fmodel",
	"dmodel", "dfmodel",
	"gmodel", "gfmodel", "gm51model",
	"kmodel_latest", "kmodel",
	"mmodel",
}

// bundledSKUDisplayNames 是兜底 SKU 的人类可读名，取自国际版实时清单实测值
// （见 docs/verification.md）。没把握的 SKU 不编名字：宁可用 SKU 当 ID，
// 也不给客户端看一个猜出来的模型名。
var bundledSKUDisplayNames = map[string]string{
	"auto":          "Auto",
	"ultimate":      "Ultimate",
	"performance":   "Performance",
	"efficient":     "Efficient",
	"qmodel_38max":  "Qwen3.8-Max",
	"qfmodel":       "Qwen3.8-Flash",
	"qmodel_latest": "Qwen3.7-Max",
	"qmodel":        "Qwen3.7-Plus",
	"kmodel_latest": "Kimi-K3",
	"kmodel":        "Kimi-K2.8-Preview",
	"gmodel":        "GLM-5.3",
	"gfmodel":       "GLM-5.3-Flash",
	"dmodel":        "DeepSeek-V4-Pro",
	"dfmodel":       "DeepSeek-Flash",
	"mmodel":        "MiniMax-M3",
}

// curatedAlias 是给人类用的别名：名字里的关键字会被 bridge 的关键字映射表
// （见 internal/bridge/bridge.go 的 defaultModelMapping）翻成上游 SKU。
// 因此别名表达的是"路由意图"，真实落到哪个 SKU 由 model_mapping 决定。
type curatedAlias struct {
	ID          string
	DisplayName string
	Description string
	Keyword     string
	Reasoning   bool
}

var curatedAliases = []curatedAlias{
	{"claude-sonnet", "Claude Sonnet（关键字映射）", "命中 sonnet 关键字，默认映射到上游 gmodel；可用 model_mapping 覆盖", "sonnet", true},
	{"claude-opus", "Claude Opus（关键字映射）", "命中 opus 关键字，默认映射到上游 qmodel_38max；可用 model_mapping 覆盖", "opus", true},
	{"claude-haiku", "Claude Haiku（关键字映射）", "命中 haiku 关键字，默认映射到上游 qfmodel；可用 model_mapping 覆盖", "haiku", false},
	{"gpt", "GPT 系（关键字映射）", "命中 gpt 关键字，默认映射到上游 dmodel；可用 model_mapping 覆盖", "gpt", true},
	{"gemini", "Gemini 系（关键字映射）", "命中 gemini 关键字，默认映射到上游 gmodel；可用 model_mapping 覆盖", "gemini", true},
}

const (
	defaultContextWindow   = 180000
	defaultMaxOutputTokens = 16384
	reasoningMaxOutput     = 32768
)

// buildModelCatalog 汇总本插件注册给 CPA 的模型清单。
//
// 顺序即优先级：实时清单（含上游 display_name 与上下文窗口）优先，其余补齐。
func buildModelCatalog(cfg pluginConfig) []pluginapi.ModelInfo {
	type entry struct {
		info pluginapi.ModelInfo
	}
	catalog := map[string]entry{}
	order := make([]string, 0, 32)
	// 按上游 SKU 去重：模型 ID 现在是人类可读名，而 extra_models / 兜底清单里写的是
	// 裸 SKU（如 gmodel）。只按 ID 去重的话，同一个上游 SKU 会以两个 ID 注册（
	// qoder-GLM-5.3 与 qoder-gmodel），客户端下拉里就会出现重复模型。
	registeredSKU := map[string]string{}
	addInternal := func(info pluginapi.ModelInfo, force bool) {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			return
		}
		if sku := strings.TrimSpace(info.Name); sku != "" && !force {
			if existing, seen := registeredSKU[sku]; seen && existing != id {
				return
			}
			registeredSKU[sku] = id
		}
		if _, exists := catalog[id]; exists {
			return
		}
		order = append(order, id)
		info.ID = id
		if info.Object == "" {
			info.Object = "model"
		}
		if info.Created == 0 {
			info.Created = 1735689600 // 2025-01-01，占位时间戳
		}
		if info.OwnedBy == "" {
			info.OwnedBy = providerKey
		}
		if info.Type == "" {
			info.Type = providerKey
		}
		catalog[id] = entry{info: info}
	}
	// add 按上游 SKU 去重；addForced 用于 extra_models：那是运维显式声明的 ID，
	// 即使与实时清单是同一个上游 SKU（如 gmodel）也应照注册，这样才能拿它
	// 恢复旧版 ID（extra_models: ["qfmodel"] → qoder-qfmodel 又能用）。
	add := func(info pluginapi.ModelInfo) { addInternal(info, false) }
	addForced := func(info pluginapi.ModelInfo) { addInternal(info, true) }

	prefix := cfg.ModelPrefix

	// 1) 实时清单：带上游真实 display_name / 上下文窗口 / 推理标记。
	for _, model := range cachedModels() {
		if !model.Enable {
			continue
		}
		info := pluginapi.ModelInfo{
			ID:                        modelRegistrationID(prefix, model.Key, model.DisplayName),
			Name:                      model.Key,
			DisplayName:               firstNonEmpty(model.DisplayName, model.Key),
			Description:               "来自 Qoder 上游实时模型清单",
			ContextLength:             int64(firstNonZero(model.ContextWindow, model.MaxInputTokens, defaultContextWindow)),
			MaxCompletionTokens:       int64(firstNonZero(model.MaxOutputTokens, defaultMaxOutputTokens)),
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		}
		if model.IsReasoning {
			info.Thinking = &pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true}
		}
		add(info)
	}

	// 2) 兜底 SKU：保证实时清单不可用时也能用。
	for _, sku := range bundledSKUs {
		name := firstNonEmpty(bundledSKUDisplayNames[sku], sku)
		add(pluginapi.ModelInfo{
			ID:                        prefix + name,
			Name:                      sku,
			DisplayName:               name,
			Description:               "Qoder 上游 SKU（兜底清单，未与实时清单比对）",
			ContextLength:             defaultContextWindow,
			MaxCompletionTokens:       defaultMaxOutputTokens,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		})
	}

	// 3) 人类可读别名：关键字映射到上游 SKU。
	for _, alias := range curatedAliases {
		info := pluginapi.ModelInfo{
			ID:                        prefix + alias.ID,
			Name:                      alias.ID,
			DisplayName:               alias.DisplayName,
			Description:               alias.Description,
			ContextLength:             defaultContextWindow,
			MaxCompletionTokens:       defaultMaxOutputTokens,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		}
		if alias.Reasoning {
			info.Thinking = &pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true}
		}
		add(info)
	}

	// 4) 用户额外声明的模型 ID（不含前缀，按关键字映射走）。
	for _, extra := range cfg.ExtraModels {
		name := strings.TrimSpace(extra)
		addForced(pluginapi.ModelInfo{
			ID:                        prefix + name,
			Name:                      name,
			DisplayName:               firstNonEmpty(bundledSKUDisplayNames[name], name),
			Description:               "由 extra_models 配置注册",
			ContextLength:             defaultContextWindow,
			MaxCompletionTokens:       defaultMaxOutputTokens,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		})
	}

	// 保持注册顺序（实时 → 兜底 → 别名 → 扩展）。
	ordered := make([]pluginapi.ModelInfo, 0, len(order))
	for _, id := range order {
		ordered = append(ordered, catalog[id].info)
	}
	return ordered
}

// modelRegistrationID 生成注册给 CPA 的模型 ID：前缀 + 人类可读名。
//
// 为什么要用名称而不是上游 SKU：CPA 的 /v1/models 与各客户端下拉只显示模型 ID
// （宿主的插件模型条目不带 display_name 字段），用 SKU 会让用户看到 qoder-qmodel_38max
// 这种内部代号。上游请求仍走 SKU，靠 modelAliasMappings 还原。
func modelRegistrationID(prefix, sku, displayName string) string {
	return prefix + firstNonEmpty(displayName, sku)
}

// modelAliasMappings 给出"人类可读名 → 上游 SKU"的内置映射，
// 让客户端请求 qoder-Qwen3.8-Flash 时能还原成上游认识的 qfmodel。
// 实时清单优先（上游改名后立即生效），静态表只在没拉过清单时兜底。
func modelAliasMappings() map[string]string {
	out := make(map[string]string, len(bundledSKUDisplayNames)+8)
	for sku, name := range bundledSKUDisplayNames {
		if strings.TrimSpace(name) != "" {
			out[strings.TrimSpace(name)] = sku
		}
	}
	for _, model := range cachedModels() {
		key := strings.TrimSpace(model.Key)
		name := strings.TrimSpace(model.DisplayName)
		if key == "" || name == "" {
			continue
		}
		out[name] = key
	}
	return out
}

// applyModelMappings 把配置与页面设置里的映射表注入 bridge 的 MapModel。
func applyModelMappings(cfg pluginConfig) {
	// 顺序即优先级（后写覆盖先写）：
	//   1) 内置别名：人类可读名 → 上游 SKU（模型 ID 用名称注册，必须能还原）；
	//   2) YAML 配置规则；
	//   3) 页面设置——与其它插件一致，"页面上改过的值"优先于 YAML
	//      （否则页面上的修改会被配置悄悄盖掉）。
	flat := modelAliasMappings()
	prefix := strings.TrimSpace(cfg.ModelPrefix)
	// 用户规则优先于内置别名（内置只负责把模型名还原成 SKU，
	// 运维想改路由仍应说了算）；`qoder-qfmodel` 这类带前缀的键按裸键处理。
	applyRules := func(rules map[string]string) {
		// 先落带前缀的键，再落裸键：两神写法同时出现时裸键优先（随机 map 顺序不影响结果）。
		for key, value := range rules {
			if bare, ok := unprefixedMappingKey(key, prefix); ok {
				flat[bare] = value
			}
		}
		for key, value := range rules {
			if strings.TrimSpace(key) == "" {
				continue
			}
			if _, prefixed := unprefixedMappingKey(key, prefix); prefixed {
				continue
			}
			flat[key] = value
		}
	}
	applyRules(cfg.ModelMappingRules)
	applyRules(stateModelMapping())
	if len(flat) == 0 {
		bridge.SetModelMappingProvider(nil)
		return
	}
	snapshot := flat
	bridge.SetModelMappingProvider(func() (map[string]map[string]string, map[string]string) {
		return nil, snapshot
	})
}

// unprefixedMappingKey 把 `qoder-qfmodel` 这类带前缀的映射键还原成执行路径实际查询的裸键
// （执行前会先 stripModelPrefix，带前缀的键否则永远匹配不上）。
func unprefixedMappingKey(key, prefix string) (string, bool) {
	if prefix == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	bare := strings.TrimSpace(strings.TrimPrefix(key, prefix))
	return bare, bare != ""
}

func handleModelStatic(request []byte) ([]byte, error) {
	cfg := loadedConfig()
	models := buildModelCatalog(cfg)
	logger.Debug("model.static: %d models (prefix=%q)", len(models), cfg.ModelPrefix)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// authModelRPCRequest 与宿主 rpcAuthModelRequest 对齐。
type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleModelForAuth 按账号返回模型：实时清单可用时同样按账号过滤，
// 保证 CPA 在按凭证聚合模型时也能看到 qoder provider。
func handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	cfg := loadedConfig()
	models := buildModelCatalog(cfg)

	// 该账号不可用（例如没有 token）时不返回模型，避免 CPA 为一个坏凭证宣称可用。
	// 用 bundle 感知的解析：多账号导出文件里的单账号也要认得。
	if _, errCred := parseQoderCredentialForAccount(rpc.StorageJSON, cfg.Region, rpc.AuthID, "", ""); errCred != nil {
		logger.Debug("model.for_auth: credential unusable for %s: %v", rpc.AuthID, errCred)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// refreshModelsFromUpstream 用某个可用账号拉取上游实时模型清单并缓存。
func refreshModelsFromUpstream(ctx context.Context, callbackID string) (int, error) {
	accounts, errList := listQoderAuthFiles(ctx, callbackID)
	if errList != nil {
		return 0, errList
	}
	if len(accounts) == 0 {
		return 0, newPluginError("no_account", "没有可用的 Qoder 账号：请先添加 auths/qoder-*.json", http.StatusBadRequest)
	}
	cfg := loadedConfig()
	callCtx := httpx.WithCallbackID(ctx, callbackID)

	var lastErr error
	for _, account := range accounts {
		cred, errCred := credentialForAuth(callCtx, callbackID, account)
		if errCred != nil {
			lastErr = errCred
			continue
		}
		b, errBridge := bridgeFor(callCtx, account.ID, cred)
		if errBridge != nil {
			lastErr = errBridge
			continue
		}
		models, errModels := b.ListAvailableModels(callCtx)
		if errModels != nil {
			lastErr = errModels
			continue
		}
		cached := make([]cachedModel, 0, len(models))
		for _, model := range models {
			cached = append(cached, cachedModel{
				Key:             model.Key,
				DisplayName:     model.DisplayName,
				ContextWindow:   model.ContextWindow,
				MaxOutputTokens: model.MaxOutputTokens,
				MaxInputTokens:  model.MaxInputTokens,
				IsReasoning:     model.IsReasoning,
				IsDefault:       model.IsDefault,
				Enable:          model.Enable,
			})
		}
		if len(cached) == 0 {
			lastErr = newPluginError("empty_model_list", "上游返回空模型清单", http.StatusBadGateway)
			continue
		}
		if errStore := storeCachedModels(cached, string(cfg.Region)); errStore != nil {
			return 0, errStore
		}
		// 清单变了，注册 ID（人类可读名）与别名映射都要跟着更新。
		applyModelMappings(cfg)
		logger.Info("refreshed %d models from upstream via %s", len(cached), account.Name)
		return len(cached), nil
	}
	if lastErr == nil {
		lastErr = newPluginError("model_refresh_failed", "所有账号都未能拉取模型清单", http.StatusBadGateway)
	}
	return 0, lastErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
