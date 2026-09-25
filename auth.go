package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/cosy"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// 本文件实现 CPA 的 auth provider 能力：把 Qoder 凭证（PAT / OAuth device token）
// 交给 CPA 作为标准 auth 文件管理，从而直接获得多账号调度、冷却、重试与额度面板。
//
// auth 文件最小形态（auths/qoder-main.json）：
//
//	{"type": "qoder", "token": "pt-xxxx", "region": "global", "label": "主账号"}
//
// refresh 策略：
//   - 能验证通过 → 顺手带上更"新鲜"的 token（PAT 交换会返回 refreshToken）并顺延刷新时间；
//   - 上游明确拒绝（401/403）→ 返回错误，交给 CPA 标记该凭证；
//   - 网络/上游抖动 → 不改动凭证，只安排较短的重试时间，避免把好账号标坏。

const (
	authRefreshInterval    = 12 * time.Hour
	authRefreshRetryAfter  = 5 * time.Minute
	authParseFileNameHint  = "qoder"
	authRefreshHTTPTimeout = 30 * time.Second
	// accountExportFormat 是 qoder2api 桌面端账号导出的 format 标记。
	accountExportFormat = "qoder2api-accounts"
)

// authFileEntry 是宿主 host.auth.list 返回的凭证条目。
type authFileEntry = pluginapi.HostAuthFileEntry

type authParseRPCRequest struct {
	pluginapi.AuthParseRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authRefreshRPCRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse 识别并解析 Qoder auth 文件。
// 只认自己名下的文件：type=qoder、文件名以 qoder 开头，或显式带 qoder 字段，
// 避免把其它 provider 的凭证误吞。
func handleAuthParse(request []byte) ([]byte, error) {
	var rpc authParseRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	if len(rpc.RawJSON) == 0 {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(rpc.RawJSON, &raw); errUnmarshal != nil {
		// 非 JSON 的凭证文件不归本插件处理。
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// qoder2api 导出文件（多账号）优先：一个文件展开成多个 CPA 账号。
	if isQoderAccountExport(raw) {
		return handleAccountExportParse(rpc, raw)
	}
	if !looksLikeQoderAuth(raw, rpc.FileName, rpc.Provider) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	cfg := loadedConfig()
	cred, errCred := parseQoderCredential(rpc.RawJSON, cfg.Region)
	if errCred != nil {
		return nil, newPluginError("qoder_credential_missing", errCred.Error(), http.StatusUnprocessableEntity)
	}
	label := firstNonEmpty(cred.Label, cred.Email, rpc.FileName, rpc.Path)
	logger.Info("parsed qoder auth %s (region=%s, oauth=%t)", label, cred.Region, cred.RefreshToken != "")
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:         providerKey,
			ID:               strings.TrimSuffix(rpc.FileName, ".json"),
			FileName:         rpc.FileName,
			Label:            label,
			StorageJSON:      rpc.RawJSON,
			Metadata:         credentialMetadata(cred),
			Attributes:       credentialAttributes(cred),
			NextRefreshAfter: nextAuthRefreshAt(),
		},
	})
}

// exportAccountEntry 是 qoder2api 导出文件里的一条账号。
// 字段名对齐 export 文件（name/email/region/auth_mode/api_mode/secret）。
type exportAccountEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Plan     string `json:"plan"`
	Region   string `json:"region"`
	AuthMode string `json:"auth_mode"`
	APIMode  string `json:"api_mode"`
	Disabled bool   `json:"disabled"`
	Secret   string `json:"secret"`
}

// isQoderAccountExport 判断文件是否是 qoder2api 的账号导出（含凭证）。
//
// 判定要看正向证据（format 标记或带 secret 的条目），不能只看有没有 accounts 数组，
// 否则会把用户其它 JSON 文件抢过来解析。
func isQoderAccountExport(raw map[string]interface{}) bool {
	accounts, ok := raw["accounts"].([]interface{})
	if !ok || len(accounts) == 0 {
		return false
	}
	if format, _ := raw["format"].(string); strings.EqualFold(strings.TrimSpace(format), accountExportFormat) {
		return true
	}
	for _, entry := range accounts {
		record, okRecord := entry.(map[string]interface{})
		if !okRecord {
			continue
		}
		if secret, _ := record["secret"].(string); strings.TrimSpace(secret) != "" {
			return true
		}
		if token, _ := record["device_token"].(string); strings.TrimSpace(token) != "" {
			return true
		}
	}
	return false
}

// handleAccountExportParse 把一个导出文件展开成多个 CPA 账号。
//
// 宿主支持一次返回多个 auth（Auths），多账号文件会被标记为“插件虚拟账号”：
// 不能单独编辑/删除，也不会被回写到源文件，改账号就在这个文件里改。
func handleAccountExportParse(rpc authParseRPCRequest, raw map[string]interface{}) ([]byte, error) {
	cfg := loadedConfig()
	rawAccounts, _ := raw["accounts"].([]interface{})
	accountList := make([]exportAccountEntry, 0, len(rawAccounts))
	for _, entry := range rawAccounts {
		encoded, errMarshal := json.Marshal(entry)
		if errMarshal != nil {
			continue
		}
		var account exportAccountEntry
		if errUnmarshal := json.Unmarshal(encoded, &account); errUnmarshal != nil {
			continue
		}
		accountList = append(accountList, account)
	}

	parsed := make([]pluginapi.AuthData, 0, len(accountList))
	skipped := 0
	baseName := strings.TrimSuffix(rpc.FileName, ".json")
	for index, account := range accountList {
		storage, cred, errBuild := buildExportAuthStorage(account, cfg.Region)
		if errBuild != nil {
			logger.Debug("export entry %d skipped: %v", index, errBuild)
			skipped++
			continue
		}
		id := firstNonEmpty(strings.TrimSpace(account.ID), fmt.Sprintf("%s-%d", baseName, index+1))
		parsed = append(parsed, pluginapi.AuthData{
			Provider:         providerKey,
			ID:               id,
			FileName:         rpc.FileName,
			Label:            firstNonEmpty(account.Name, account.Email, id),
			Disabled:         account.Disabled,
			StorageJSON:      storage,
			Metadata:         credentialMetadata(cred),
			Attributes:       credentialAttributes(cred),
			NextRefreshAfter: nextAuthRefreshAt(),
		})
	}

	if len(parsed) == 0 {
		return nil, newPluginError("qoder_credential_missing",
			fmt.Sprintf("导出文件 %s 里没有可用凭证（%d 条账号均缺少 secret；导出时需勾选包含凭证 include_secrets）",
				rpc.FileName, len(accountList)), http.StatusUnprocessableEntity)
	}
	logger.Info("imported %d qoder account(s) from %s (%d skipped)", len(parsed), rpc.FileName, skipped)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auths: parsed})
}

// buildExportAuthStorage 把一条导出账号转成 CPA auth 文件内容。
// 输出保持插件自己的扁平格式（device_token/refresh_token/region/label/email），
// 这样账号的来源、区域与展示名都能留住，刷新时也不会丢字段。
func buildExportAuthStorage(account exportAccountEntry, fallbackRegion qoder.Region) ([]byte, qoderCredential, error) {
	secret := strings.TrimSpace(account.Secret)
	if secret == "" {
		return nil, qoderCredential{}, fmt.Errorf("账号 %q 缺少 secret", account.ID)
	}
	payload := map[string]interface{}{"type": providerKey}
	deviceToken, refreshToken := "", ""
	if strings.HasPrefix(secret, "{") {
		inner := map[string]interface{}{}
		if errUnmarshal := json.Unmarshal([]byte(secret), &inner); errUnmarshal != nil {
			return nil, qoderCredential{}, fmt.Errorf("账号 %q 的 secret 不是合法 JSON: %w", account.ID, errUnmarshal)
		}
		for key, value := range inner {
			payload[key] = value
		}
		deviceToken, _ = inner["device_token"].(string)
		refreshToken, _ = inner["refresh_token"].(string)
	} else {
		payload["device_token"] = secret
		deviceToken = secret
	}
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return nil, qoderCredential{}, fmt.Errorf("账号 %q 的 secret 里没有 device_token", account.ID)
	}
	region := qoder.NormalizeRegion(firstNonEmpty(account.Region, string(fallbackRegion)))
	payload["region"] = string(region)
	if label := firstNonEmpty(account.Name, account.Email); label != "" {
		payload["label"] = label
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		payload["email"] = email
	}
	if plan := strings.TrimSpace(account.Plan); plan != "" {
		payload["plan"] = plan
	}
	if mode := strings.TrimSpace(account.AuthMode); mode != "" {
		payload["auth_mode"] = mode
	}
	if accountID := strings.TrimSpace(account.ID); accountID != "" {
		payload["qoder_account_id"] = accountID
	}
	storage, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, qoderCredential{}, errMarshal
	}
	cred := qoderCredential{
		Token:        deviceToken,
		RefreshToken: strings.TrimSpace(refreshToken),
		Region:       region,
		Label:        firstNonEmpty(account.Name, account.Email),
		Email:        strings.TrimSpace(account.Email),
	}
	return storage, cred, nil
}

// credentialMetadata 是回给宿主的账号元数据。
//
// 关键：宿主判断“这个凭证能不能刷新”看的是 Metadata 里的 refresh_token
// （sdk/cliproxy/auth/authHasRefreshCredential），没有它则 /auth-files/refresh 与自动刷新
// 都会直接跳过该账号——OAuth 账号的 device token 过期后永远不会被续期。
func credentialMetadata(cred qoderCredential) map[string]any {
	metadata := map[string]any{"region": string(cred.Region)}
	if cred.Email != "" {
		metadata["email"] = cred.Email
	}
	if cred.RefreshToken != "" {
		metadata["refresh_token"] = cred.RefreshToken
		metadata["auth_mode"] = "oauth"
	}
	return metadata
}

// nextAuthRefreshAt 给出宿主下次应该刷新该凭证的时间。
// 零值会让自动刷新循环把它当成“从未刷新过”的凭证；这里直接对齐插件的刷新周期。
func nextAuthRefreshAt() time.Time {
	return time.Now().Add(authRefreshInterval)
}

// credentialAttributes 是回给宿主展示的属性（不含任何凭证）。
func credentialAttributes(cred qoderCredential) map[string]string {
	attributes := map[string]string{"region": string(cred.Region)}
	if cred.Email != "" {
		attributes["email"] = cred.Email
	}
	if cred.RefreshToken != "" {
		attributes["auth_mode"] = "oauth"
	}
	return attributes
}

// looksLikeQoderAuth 判断该凭证文件是否属于本插件。
func looksLikeQoderAuth(raw map[string]interface{}, fileName, provider string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), providerKey) {
		return true
	}
	if typeValue, ok := raw["type"].(string); ok && strings.EqualFold(strings.TrimSpace(typeValue), providerKey) {
		return true
	}
	if _, ok := raw["qoder_token"]; ok {
		return true
	}
	// OAuth 形态（含从导出文件里拆出来的单账号）也要认：device_token / secret。
	if _, ok := raw["device_token"]; ok {
		return true
	}
	if secret, ok := raw["secret"].(string); ok && strings.TrimSpace(secret) != "" {
		return true
	}
	base := strings.ToLower(strings.TrimSuffix(fileName, ".json"))
	return strings.HasPrefix(base, authParseFileNameHint) || strings.Contains(base, "-"+authParseFileNameHint) ||
		strings.Contains(base, "_"+authParseFileNameHint)
}

// handleAuthRefresh 校验凭证并在必要/可能时更新存储内容。
func handleAuthRefresh(request []byte) ([]byte, error) {
	var rpc authRefreshRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), authRefreshHTTPTimeout)
	defer cancel()

	cred, errCred := resolveRPCCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, "", rpc.AuthID, rpc.Attributes)
	if errCred != nil {
		return nil, newPluginError("qoder_credential_missing", errCred.Error(), http.StatusUnauthorized)
	}

	updated, errValidate := validateCredential(ctx, cred, rpc.StorageJSON)
	if errValidate != nil {
		classified := classifyCredentialError(errValidate)
		if classified.HTTPStatus == http.StatusUnauthorized || classified.HTTPStatus == http.StatusForbidden {
			logger.Error("auth refresh rejected for %s: %s", rpc.AuthID, classified.Message)
			return nil, classified
		}
		// 抖动/未知错误：保留原凭证，稍后重试。
		logger.Error("auth refresh deferred for %s: %s", rpc.AuthID, classified.Message)
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             buildRefreshedAuth(rpc, rpc.StorageJSON, cred),
			NextRefreshAfter: time.Now().Add(authRefreshRetryAfter),
		})
	}

	storage := rpc.StorageJSON
	if len(updated) > 0 {
		storage = updated
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             buildRefreshedAuth(rpc, storage, cred),
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	})
}

// buildRefreshedAuth 组装刷新结果：只回传凭证本体与区域，其余字段由宿主补齐。
func buildRefreshedAuth(rpc authRefreshRPCRequest, storage []byte, cred qoderCredential) pluginapi.AuthData {
	data := pluginapi.AuthData{
		Provider:    providerKey,
		ID:          rpc.AuthID,
		StorageJSON: storage,
		Attributes:  map[string]string{"region": string(cred.Region)},
	}
	if rpc.Metadata != nil {
		data.Metadata = rpc.Metadata
	}
	return data
}

// validateCredential 用一次真实的上游调用验证凭证是否可用。
// storageJSON 是 auth 文件原文：上游下发新 token 时在它之上做最小更新，
// 以免把用户自己写的字段（disabled / prefix / proxy_url / note 等）拿去。
// 返回的 storage 非空时表示上游下发了新的凭证内容（PAT 交换会带 refreshToken）。
func validateCredential(ctx context.Context, cred qoderCredential, storageJSON []byte) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(cred.Token), "dt-") {
		if _, errUser := bridge.FetchUserInfoWithToken(ctx, cred.Token, cred.Region); errUser != nil {
			return nil, errUser
		}
		return nil, nil
	}
	seed := cosy.FingerprintSeed("", cred.Token)
	endpoints := qoder.GetEndpoints(cred.Region)
	jobToken, errExchange := cosy.ExchangeJobToken(ctx, cred.Token,
		cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), endpoints.JobTokenURL)
	if errExchange != nil {
		return nil, errExchange
	}
	refreshed := jobTokenRefreshStorage(cred, storageJSON, jobToken)
	return refreshed, nil
}

// jobTokenRefreshStorage 在 jobToken 交换返回新 token 时更新 auth 文件内容。
//
// 基于原文件做合并更新，而不是从头构造：CPA 的 auth 文件里还可能有 user 自己维护的
// disabled / prefix / proxy_url / note / weight 等字段，重建会静默丢掉它们。
func jobTokenRefreshStorage(cred qoderCredential, storageJSON []byte, jobToken map[string]interface{}) []byte {
	refreshToken := bridge.StrVal(jobToken, "refreshToken")
	securityToken := bridge.StrVal(jobToken, "securityOauthToken")
	if refreshToken == "" && securityToken == "" {
		return nil
	}
	storage := map[string]interface{}{}
	if len(storageJSON) > 0 {
		if errUnmarshal := json.Unmarshal(storageJSON, &storage); errUnmarshal != nil || storage == nil {
			storage = map[string]interface{}{}
		}
	}
	storage["type"] = providerKey
	storage["token"] = cred.Token
	if cred.Region != "" {
		storage["region"] = string(cred.Region)
	}
	if cred.Label != "" {
		storage["label"] = cred.Label
	}
	if cred.Email != "" {
		storage["email"] = cred.Email
	}
	if refreshToken != "" {
		storage["refresh_token"] = refreshToken
	}
	if securityToken != "" {
		storage["security_oauth_token"] = securityToken
	}
	raw, errMarshal := json.MarshalIndent(storage, "", "  ")
	if errMarshal != nil {
		return nil
	}
	return raw
}

// classifyCredentialError 区分"凭证被拒"与"暂时不通"：
// 前者要交给 CPA 标记凭证，后者不能误伤好账号。
func classifyCredentialError(err error) *pluginError {
	message := err.Error()
	lower := strings.ToLower(message)
	// 排队/服务未就绪不是凭证问题：不能让一个有效账号因为上游排队被标成“凭证失效”。
	if _, queued := bridge.ParseQueueSignal(message); queued {
		return &pluginError{Code: "qoder_model_busy", Message: message, HTTPStatus: http.StatusServiceUnavailable}
	}
	switch {
	case strings.Contains(message, "HTTP 401"), strings.Contains(message, "HTTP 403"),
		strings.Contains(lower, "unauthorized"), strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "invalid token"), strings.Contains(lower, "token expired"):
		return &pluginError{Code: "qoder_credential_invalid", Message: message, HTTPStatus: http.StatusUnauthorized}
	case bridge.IsTransientTransport(err):
		return &pluginError{Code: "qoder_upstream_transient", Message: message, HTTPStatus: http.StatusBadGateway}
	default:
		return &pluginError{Code: "qoder_upstream_error", Message: message, HTTPStatus: http.StatusBadGateway}
	}
}

// ---- 账号枚举（签到与账号管理页共用）----

// parseQoderCredentialForAccount 在文件级来源（host.auth.list/get）上解析凭证。
//
// 文件可能是单账号 auth 文件，也可能是 qoder2api 导出文件（一个文件多个账号）。
// 后者必须按账号挑对那一条，否则多账号导出会全部指向第一条（甚至第一条已失效时全部失败）。
// hints 依次是账号 id / email / label，来自宿主的账号条目。
func parseQoderCredentialForAccount(storageJSON []byte, fallbackRegion qoder.Region, hints ...string) (qoderCredential, error) {
	cred, errFlat := parseQoderCredential(storageJSON, fallbackRegion)
	if errFlat == nil {
		return cred, nil
	}
	entry, errEntry := selectExportEntry(storageJSON, hints...)
	if errEntry != nil {
		// 返回原始错误：单账号文件的报错对用户更有指向性。
		return qoderCredential{}, errFlat
	}
	_, cred, errBuild := buildExportAuthStorage(entry, fallbackRegion)
	if errBuild != nil {
		return qoderCredential{}, errBuild
	}
	return cred, nil
}

// selectExportEntry 从导出文件里挑一条账号：按 id/email/label 匹配，都不中则退到第一条可用条目。
func selectExportEntry(storageJSON []byte, hints ...string) (exportAccountEntry, error) {
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(storageJSON, &raw); errUnmarshal != nil {
		return exportAccountEntry{}, errUnmarshal
	}
	if !isQoderAccountExport(raw) {
		return exportAccountEntry{}, fmt.Errorf("not a qoder2api account export")
	}
	entries, _ := raw["accounts"].([]interface{})
	candidates := make([]exportAccountEntry, 0, len(entries))
	for _, item := range entries {
		encoded, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			continue
		}
		var entry exportAccountEntry
		if errUnmarshal := json.Unmarshal(encoded, &entry); errUnmarshal != nil {
			continue
		}
		candidates = append(candidates, entry)
	}
	for _, hint := range hints {
		needle := strings.ToLower(strings.TrimSpace(hint))
		if needle == "" {
			continue
		}
		for _, entry := range candidates {
			for _, value := range []string{entry.ID, entry.Email, entry.Name} {
				if strings.ToLower(strings.TrimSpace(value)) == needle {
					return entry, nil
				}
			}
		}
	}
	var firstErr error
	for _, entry := range candidates {
		if _, _, errBuild := buildExportAuthStorage(entry, qoder.RegionGlobal); errBuild == nil {
			return entry, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("导出文件里没有可用凭证")
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("导出文件里没有账号")
	}
	return exportAccountEntry{}, firstErr
}

// listQoderAuthFiles 通过 host.auth.list 列出所有 Qoder 凭证。
func listQoderAuthFiles(ctx context.Context, callbackID string) ([]pluginapi.HostAuthFileEntry, error) {
	raw, errCall := callHostScoped(callbackID, pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	for _, entry := range resp.Files {
		if entry.RuntimeOnly {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(entry.Type), providerKey) ||
			strings.EqualFold(strings.TrimSpace(entry.Provider), providerKey) {
			out = append(out, entry)
		}
	}
	return out, nil
}

// credentialForAuth 读取某个凭证的物理 JSON 并解析成 Qoder 凭证。
//
// 宿主这里给的是账号级条目，但读回来的是文件内容：单账号文件就直接解，
// 导出文件（多账号）则按 entry.ID / label 挑对那一条。结果带短暂缓存，
// 避免管理页轮询把 host.auth.get 打满（一个账号一次）。
func credentialForAuth(ctx context.Context, callbackID string, entry pluginapi.HostAuthFileEntry) (qoderCredential, error) {
	index := strings.TrimSpace(entry.AuthIndex)
	if index == "" {
		index = strings.TrimSpace(entry.ID)
	}
	cacheKey := index
	if cacheKey == "" {
		cacheKey = entry.Path
	}
	if cred, ok := lookupCredentialCache(cacheKey); ok {
		return cred, nil
	}

	raw, errCall := callHostScoped(callbackID, pluginabi.MethodHostAuthGet, map[string]string{"auth_index": index})
	if errCall != nil {
		return qoderCredential{}, errCall
	}
	var resp pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return qoderCredential{}, fmt.Errorf("decode host.auth.get: %w", errUnmarshal)
	}
	cred, errCred := parseQoderCredentialForAccount(resp.JSON, loadedConfig().Region, entry.ID, entry.Label, entry.Name)
	if errCred != nil {
		return qoderCredential{}, errCred
	}
	if entry.AccountType != "" && cred.Label == "" {
		cred.Label = entry.AccountType
	}
	storeCredentialCache(cacheKey, cred)
	return cred, nil
}

// credentialFromAuthRef 按 auth_index（或 auth_id → auth_index）去宿主取回该账号的凭证。
//
// 必须先走 id 而不是只看 StorageJSON：宿主的**管理端额度路由**
// （internal/api/handlers/management/plugin_quota.go）只发 AuthIndex/AuthID + Metadata/Attributes，
// **完全不发 StorageJSON**；而插件设计上不把 token 放进 metadata/attributes，
// 于是只认 StorageJSON 就会把额度查询误报成“凭证缺失”（实测 502）。
//
// 注意：宿主的 host.auth.get 只认 auth_index（按 id 查会失败），所以要先用列表把 id 映射成 index。
func credentialFromAuthRef(ctx context.Context, callbackID string, authIndex, authID string) (qoderCredential, error) {
	index := strings.TrimSpace(authIndex)
	id := strings.TrimSpace(authID)
	if index == "" {
		if id == "" {
			return qoderCredential{}, fmt.Errorf("request carried neither storage nor auth index/id")
		}
		candidates, errList := listQoderAuthFiles(ctx, callbackID)
		if errList != nil {
			return qoderCredential{}, fmt.Errorf("%w (also could not list auth files to map id %q)", errList, id)
		}
		for _, candidate := range candidates {
			if strings.TrimSpace(candidate.ID) == id {
				index = strings.TrimSpace(candidate.AuthIndex)
				break
			}
		}
		if index == "" {
			return qoderCredential{}, fmt.Errorf("auth %q has no auth index (host.auth.get needs one)", id)
		}
	}
	return credentialForAuth(ctx, callbackID, pluginapi.HostAuthFileEntry{ID: id, AuthIndex: index})
}

// resolveRPCCredential 统一解决“这次 RPC 该用哪个凭证”。
//
// 优先用宿主直接给的 storage（单账号文件 / 导出拆出的单账号），
// 没有则回退到 auth_index/auth_id 反查；两条路都支持 qoder2api 多账号导出文件。
func resolveRPCCredential(ctx context.Context, callbackID string, storageJSON []byte, authIndex, authID string, attributes map[string]string) (qoderCredential, error) {
	cfg := loadedConfig()
	if len(bytes.TrimSpace(storageJSON)) > 0 {
		cred, errCred := parseQoderCredentialForAccount(storageJSON, cfg.Region, authID, "", "")
		if errCred == nil {
			applyRegionAttribute(&cred, attributes)
			return cred, nil
		}
		// storage 解析不了时（例如宿主只给了元数据）继续尝试按 id 反查。
		if strings.TrimSpace(authIndex) == "" && strings.TrimSpace(authID) == "" {
			return qoderCredential{}, errCred
		}
	}
	cred, errRef := credentialFromAuthRef(ctx, callbackID, authIndex, authID)
	if errRef != nil {
		return qoderCredential{}, errRef
	}
	applyRegionAttribute(&cred, attributes)
	return cred, nil
}

// applyRegionAttribute 让调用方传来的 region 覆盖凭证里的 region。
func applyRegionAttribute(cred *qoderCredential, attributes map[string]string) {
	if cred == nil {
		return
	}
	if region := strings.TrimSpace(attributes["region"]); region != "" {
		cred.Region = qoder.NormalizeRegion(region)
	}
}

// credentialCacheTTL 是账号凭证解析结果的缓存时长。
// 凭证轮换主要由宿主驱动（它会把新 JSON 交给 model.for_auth / executor），
// 这里主要服务管理页轮询，缓存 30s 既够用又能及时看到换号。
const credentialCacheTTL = 30 * time.Second

type credentialCacheEntry struct {
	at   time.Time
	cred qoderCredential
}

var (
	credentialCacheMu sync.Mutex
	credentialCache   = map[string]credentialCacheEntry{}
)

func lookupCredentialCache(key string) (qoderCredential, bool) {
	if strings.TrimSpace(key) == "" {
		return qoderCredential{}, false
	}
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	entry, ok := credentialCache[key]
	if !ok || time.Since(entry.at) > credentialCacheTTL {
		return qoderCredential{}, false
	}
	return entry.cred, true
}

func storeCredentialCache(key string, cred qoderCredential) {
	if strings.TrimSpace(key) == "" {
		return
	}
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	credentialCache[key] = credentialCacheEntry{at: time.Now(), cred: cred}
}
