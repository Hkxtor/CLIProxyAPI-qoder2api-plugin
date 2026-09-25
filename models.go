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
	add := func(info pluginapi.ModelInfo) {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			return
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

	prefix := cfg.ModelPrefix

	// 1) 实时清单：带上游真实 display_name / 上下文窗口 / 推理标记。
	for _, model := range cachedModels() {
		if !model.Enable {
			continue
		}
		info := pluginapi.ModelInfo{
			ID:                        prefix + model.Key,
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

	// 2) 兜底 SKU：保证实时清单不可用时也能用（前缀 = 上游 key）。
	for _, sku := range bundledSKUs {
		add(pluginapi.ModelInfo{
			ID:                        prefix + sku,
			Name:                      sku,
			DisplayName:               sku,
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
		add(pluginapi.ModelInfo{
			ID:                        prefix + strings.TrimSpace(extra),
			Name:                      strings.TrimSpace(extra),
			DisplayName:               strings.TrimSpace(extra),
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

// applyModelMappings 把配置与页面设置里的映射表注入 bridge 的 MapModel。
func applyModelMappings(cfg pluginConfig) {
	// 先放配置里的规则，再用页面设置覆盖：与其它插件一致，
	// "页面上改过的值"优先于 YAML（否则页面上的修改会被配置悄悄盖掉）。
	flat := map[string]string{}
	for key, value := range cfg.ModelMappingRules {
		flat[key] = value
	}
	for key, value := range stateModelMapping() {
		flat[key] = value
	}
	if len(flat) == 0 {
		bridge.SetModelMappingProvider(nil)
		return
	}
	snapshot := flat
	bridge.SetModelMappingProvider(func() (map[string]map[string]string, map[string]string) {
		return nil, snapshot
	})
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
