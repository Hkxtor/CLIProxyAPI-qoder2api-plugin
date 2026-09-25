package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
)

// 本文件实现插件自有的管理接口。
//
// 安全边界（重要）：
//   - /v0/resource/plugins/qoder2api/console 只返回静态页面，不带任何账号/凭证数据；
//   - 所有读取与动作都走 /v0/management/plugins/qoder2api/...，由 CPA 的管理鉴权保护；
//   - 接口返回里永远不含 token（只回传账号名、区域、状态、额度与签到结果）。

const managementRequestTimeout = 3 * time.Minute

func handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	return okEnvelope(dispatchManagement(req))
}

func dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch {
	case method == http.MethodGet && matchesResourcePath(req.Path, "/console"):
		return htmlResponse(http.StatusOK, []byte(consolePageHTML(managementRoutePrefix)))
	case method == http.MethodGet && matchesManagementPath(req.Path, "/status"):
		return jsonResponse(http.StatusOK, buildStatusPayload())
	case method == http.MethodGet && matchesManagementPath(req.Path, "/logs"):
		return jsonResponse(http.StatusOK, buildLogsPayload(req))
	case method == http.MethodPost && matchesManagementPath(req.Path, "/checkin"):
		return handleCheckinRequest(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/quotas"):
		// 注意：单账号额度查询走宿主的 /v0/management/plugins/<id>/quota（它转发到本插件的
		// quota.fetch 能力），所以插件自己的批量额度接口必须换个路径，否则会被宿主路由遮蔽。
		return handleQuotaRequest(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/settings"):
		return handleSettingsRequest(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/models/refresh"):
		ctx, cancel := managementContext(req)
		defer cancel()
		count, errRefresh := refreshModelsFromUpstream(ctx, hostCallbackID(req))
		if errRefresh != nil {
			return errorResponse(errRefresh)
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "models": count})
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found", "path": req.Path, "method": method})
	}
}

// managementContext 给管理接口里的上游调用一个可取消的上下文。
func managementContext(req pluginapi.ManagementRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), managementRequestTimeout)
	return httpx.WithCallbackID(ctx, hostCallbackID(req)), cancel
}

func hostCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// ---- /status ----

type accountStatus struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Email       string `json:"email,omitempty"`
	Region      string `json:"region"`
	AuthMode    string `json:"auth_mode,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Path        string `json:"path,omitempty"`

	CheckinStatus  string `json:"checkin_status,omitempty"`
	CheckinMessage string `json:"checkin_message,omitempty"`
	CheckinAt      string `json:"checkin_at,omitempty"`
	StreakDays     int    `json:"streak_days,omitempty"`
	TotalDays      int    `json:"total_days,omitempty"`
	TotalCredits   int    `json:"total_credits,omitempty"`
}

type statusPayload struct {
	Plugin      string          `json:"plugin"`
	Version     string          `json:"version"`
	Region      string          `json:"region"`
	ModelPrefix string          `json:"model_prefix"`
	StateDir    string          `json:"state_dir"`
	LogLevel    string          `json:"log_level"`
	AutoCheckin bool            `json:"auto_checkin"`
	CheckinAt   string          `json:"auto_checkin_at"`
	LastAutoRun string          `json:"last_auto_checkin_on,omitempty"`
	Accounts    []accountStatus `json:"accounts"`
	Warning     string          `json:"warning,omitempty"`

	ModelCount      int    `json:"model_count"`
	RegisteredCount int    `json:"registered_models"`
	ModelsFetchedAt string `json:"models_fetched_at,omitempty"`
	ModelsRegion    string `json:"models_region,omitempty"`
	ModelPreview    []any  `json:"model_preview,omitempty"`
}

func buildStatusPayload() statusPayload {
	cfg := loadedConfig()
	enabled, at := effectiveCheckinSettings()
	state := snapshotState()

	payload := statusPayload{
		Plugin:      pluginDisplayName,
		Version:     effectivePluginVersion(),
		Region:      string(cfg.Region),
		ModelPrefix: cfg.ModelPrefix,
		StateDir:    cfg.StateDir,
		LogLevel:    cfg.LogLevel,
		AutoCheckin: enabled,
		CheckinAt:   at,
		LastAutoRun: state.LastAutoCheckinOn,
		Accounts:    []accountStatus{},
	}

	cached := cachedModels()
	payload.ModelCount = len(cached)
	// 与宿主模型注册表的口径区分开：这里报的是本插件当前会注册的模型总数，
	// 便于发现“插件已改前缀但宿主仍持有旧清单”这类不一致。
	payload.RegisteredCount = len(buildModelCatalog(cfg))
	if !state.ModelsFetchedAt.IsZero() {
		payload.ModelsFetchedAt = state.ModelsFetchedAt.Format(time.RFC3339)
	}
	payload.ModelsRegion = state.ModelsRegion
	for index, model := range cached {
		if index >= 20 {
			break
		}
		payload.ModelPreview = append(payload.ModelPreview, map[string]any{
			"key":            model.Key,
			"display_name":   model.DisplayName,
			"is_default":     model.IsDefault,
			"is_reasoning":   model.IsReasoning,
			"context_window": model.ContextWindow,
			"max_output":     model.MaxOutputTokens,
			"registered_id":  modelRegistrationID(cfg.ModelPrefix, model.Key, model.DisplayName),
			"enable":         model.Enable,
		})
	}

	listCtx := httpx.WithCallbackID(context.Background(), "")
	accounts, errList := listQoderAuthFiles(listCtx, "")
	if errList != nil {
		payload.Warning = "读取 CPA 账号列表失败：" + errList.Error()
		return payload
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	for _, entry := range accounts {
		item := accountStatus{
			ID:          entry.ID,
			AuthIndex:   entry.AuthIndex,
			Name:        entry.Name,
			Label:       entry.Label,
			Region:      string(loadedConfig().Region),
			Status:      entry.Status,
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			Path:        entry.Path,
		}
		// 账号自己的 region 优先于插件默认值（导出文件里的国际版账号就是 global）。
		// 解析失败不是错误：账号可能真的没凭证，状态照常展示，只是不带这些字段。
		if cred, errCred := credentialForAuth(listCtx, "", entry); errCred == nil {
			item.Region = string(cred.Region)
			item.Email = cred.Email
			if cred.RefreshToken != "" {
				item.AuthMode = "oauth"
			} else {
				item.AuthMode = "pat"
			}
		}
		if record, ok := state.Checkin[entry.ID]; ok {
			item.CheckinStatus = record.LastStatus
			item.CheckinMessage = record.LastMessage
			item.CheckinAt = record.LastAttempt
			item.StreakDays = record.Streak
			item.TotalDays = record.TotalDays
			item.TotalCredits = record.TotalCredits
		}
		payload.Accounts = append(payload.Accounts, item)
	}
	if len(payload.Accounts) == 0 {
		payload.Warning = "还没有 Qoder 账号：在 auths 目录放入 qoder-*.json（见 README）后 CPA 会自动识别"
	}
	return payload
}

// ---- /logs ----

func buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt(req, "since", 0)
	limit := queryInt(req, "limit", 200)
	page := logger.GetLogsSince(since, limit)
	entries := make([]map[string]any, 0, len(page.Entries))
	for _, entry := range page.Entries {
		entries = append(entries, map[string]any{
			"seq":     entry.Seq,
			"time":    entry.Time.Format("15:04:05.000"),
			"level":   string(entry.Level),
			"message": entry.Message,
		})
	}
	return map[string]any{"entries": entries, "last_seq": page.LastSeq}
}

// ---- /checkin ----

func handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		}
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	results, errRun := runCheckin(ctx, hostCallbackID(req), body.AccountIDs)
	if errRun != nil {
		return errorResponse(errRun)
	}
	claimed := 0
	for _, result := range results {
		if result.Status == checkinStatusClaimed {
			claimed++
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"total":   len(results),
		"claimed": claimed,
		"results": results,
	})
}

// ---- /quotas ----

func handleQuotaRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AccountIDs []string `json:"account_ids"`
		Concurrent int      `json:"concurrent"`
	}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		}
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)
	accounts, errList := listQoderAuthFiles(ctx, callbackID)
	if errList != nil {
		return errorResponse(newPluginError("auth_list_failed", errList.Error(), http.StatusBadGateway))
	}
	if len(body.AccountIDs) > 0 {
		wanted := map[string]struct{}{}
		for _, id := range body.AccountIDs {
			wanted[strings.TrimSpace(id)] = struct{}{}
		}
		filtered := accounts[:0]
		for _, entry := range accounts {
			if _, ok := wanted[entry.ID]; ok {
				filtered = append(filtered, entry)
			}
		}
		accounts = filtered
	}
	concurrency := body.Concurrent
	if concurrency <= 0 || concurrency > 4 {
		concurrency = 2
	}

	type quotaEntry struct {
		AccountID string                        `json:"account_id"`
		Name      string                        `json:"account"`
		Error     string                        `json:"error,omitempty"`
		Quota     *pluginapi.QuotaFetchResponse `json:"quota,omitempty"`
	}
	results := make([]quotaEntry, len(accounts))
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for index := range accounts {
		entry := accounts[index]
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			results[slot] = quotaEntry{AccountID: entry.ID, Name: accountLabel(entry)}
			cred, errCred := credentialForAuth(ctx, callbackID, entry)
			if errCred != nil {
				results[slot].Error = errCred.Error()
				return
			}
			quota, errFetch := fetchQoderQuota(ctx, cred)
			if errFetch != nil {
				results[slot].Error = errFetch.Error()
				return
			}
			converted := buildQuotaResponse(quota)
			results[slot].Quota = &converted
		}(index)
	}
	waitGroup.Wait()
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "accounts": results})
}

// ---- /settings ----

func handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "empty body"})
	}
	var body struct {
		AutoCheckin   *bool             `json:"auto_checkin"`
		AutoCheckinAt *string           `json:"auto_checkin_at"`
		ModelPrefix   *string           `json:"model_prefix"`
		ModelMapping  map[string]string `json:"model_mapping"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
	}
	if body.AutoCheckinAt != nil {
		if _, _, errClock := parseClock(*body.AutoCheckinAt); errClock != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errClock.Error()})
		}
	}
	// model_prefix 是注册期参数：宿主只在它自己的配置变更时重读插件的模型清单，
	// 插件单方面改前缀会让 /v1/models 与插件内部状态不一致，所以这里只接受“不变”，
	// 变更请求直接告诉调用方走宿主的插件配置接口。
	if body.ModelPrefix != nil {
		requested := strings.TrimSpace(*body.ModelPrefix)
		if requested != loadedConfig().ModelPrefix {
			return jsonResponse(http.StatusConflict, map[string]any{
				"error": "model_prefix_must_change_in_host_config",
				"message": "模型前缀属于插件配置，请用 CPA 的插件配置接口修改（会持久化并热重载）：" +
					"PATCH /v0/management/plugins/" + pluginID + "/config {\"model_prefix\":\"...\"}",
				"current": loadedConfig().ModelPrefix,
			})
		}
	}
	if errMutate := mutateState(func(state *pluginState) {
		if body.AutoCheckin != nil {
			value := *body.AutoCheckin
			state.AutoCheckin = &value
		}
		if body.AutoCheckinAt != nil {
			state.AutoCheckinAt = strings.TrimSpace(*body.AutoCheckinAt)
		}
		if body.ModelMapping != nil {
			state.ModelMapping = map[string]string{}
			for key, value := range body.ModelMapping {
				name := strings.TrimSpace(key)
				sku := strings.TrimSpace(value)
				if name == "" || sku == "" {
					continue
				}
				state.ModelMapping[name] = sku
			}
		}
	}); errMutate != nil {
		return errorResponse(newPluginError("state_write_failed", errMutate.Error(), http.StatusInternalServerError))
	}
	applyModelMappings(loadedConfig())
	enabled, at := effectiveCheckinSettings()
	payload := map[string]any{
		"ok":              true,
		"auto_checkin":    enabled,
		"auto_checkin_at": at,
		"model_mapping":   stateModelMapping(),
		"model_prefix":    loadedConfig().ModelPrefix,
	}
	return jsonResponse(http.StatusOK, payload)
}

// ---- 工具 ----

func matchesManagementPath(path, suffix string) bool {
	normalized := normalizePath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == managementRoutePrefix+suffix || normalized == "/v0/management"+managementRoutePrefix+suffix
}

func matchesResourcePath(path, suffix string) bool {
	normalized := normalizePath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == "/v0/resource/plugins/"+pluginID+suffix
}

func normalizePath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

func queryInt(req pluginapi.ManagementRequest, key string, fallback int) int {
	if req.Query == nil {
		return fallback
	}
	values := req.Query[key]
	if len(values) == 0 {
		return fallback
	}
	parsed := 0
	if _, errScan := fmt.Sscanf(strings.TrimSpace(values[0]), "%d", &parsed); errScan != nil {
		return fallback
	}
	return parsed
}

func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{htmlContentType}},
		Body:       body,
	}
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{jsonContentType}},
		Body:       raw,
	}
}

func errorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	message := err.Error()
	if pe, ok := err.(*pluginError); ok {
		message = pe.Message
		if pe.HTTPStatus > 0 {
			status = pe.HTTPStatus
		}
	}
	return jsonResponse(status, map[string]any{"error": message, "ok": false})
}
