package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/cosy"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// 本文件实现 CPA 的 quota provider 能力：把 Qoder 的套餐额度与个人拓展包
// 规范化成管理端可渲染的配额分组。
//
// 上游接口（移植自 qoder2api/account/oauth.go，逻辑逐条对齐）：
//   - GET {QuotaEndpoint} → {isQuotaExceeded, expiresAt, userQuota{used,total,remaining,resetTime}, addOnQuota{...}}
//   - GET {PlanEndpoint}  → {plan_tier_name}
//
// 注意取数用的 token 不是 auth 文件里那个：PAT 必须先换成 jobToken 里的
// securityOauthToken，device token 才能直接用。

const quotaHTTPTimeout = 30 * time.Second

type quotaRPCRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type quotaResetRPCRequest struct {
	pluginapi.QuotaResetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// qoderQuotaBucket 是上游额度桶。
type qoderQuotaBucket struct {
	Used      float64
	Total     float64
	Remaining float64
	ResetTime string
}

// qoderQuota 是一次额度查询结果。
type qoderQuota struct {
	Plan            string
	UserQuota       *qoderQuotaBucket
	AddonQuota      *qoderQuotaBucket
	IsQuotaExceeded bool
	ExpiresAt       int64
}

func handleQuotaDescribe(request []byte) ([]byte, error) {
	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        pluginDisplayName,
		SupportsReset:      false,
	})
}

func handleQuotaFetch(request []byte) ([]byte, error) {
	var rpc quotaRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), quotaHTTPTimeout)
	defer cancel()

	cred, errCred := resolveRPCCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthIndex, rpc.AuthID, rpc.Attributes)
	if errCred != nil {
		return nil, newPluginError("qoder_credential_missing", errCred.Error(), http.StatusBadRequest)
	}

	quota, errFetch := fetchQoderQuota(ctx, cred)
	if errFetch != nil {
		return nil, errFetch
	}
	return okEnvelope(buildQuotaResponse(quota))
}

// handleQuotaReset Qoder 上游没有配额重置接口：明确返回"不支持"，
// 避免管理端以为重置成功。
func handleQuotaReset(request []byte) ([]byte, error) {
	var rpc quotaResetRPCRequest
	_ = decodeStringRequest(request, &rpc)
	return okEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: "Qoder 上游没有配额重置接口（额度按上游计费周期自动恢复）",
	})
}

// fetchQoderQuota 查询账号额度。
func fetchQoderQuota(ctx context.Context, cred qoderCredential) (*qoderQuota, error) {
	bearer, errToken := quotaBearerToken(ctx, cred)
	if errToken != nil {
		return nil, errToken
	}
	endpoints := qoder.GetEndpoints(cred.Region)

	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return nil, errQuota
	}
	quota := &qoderQuota{
		Plan:            fetchPlanTierName(ctx, endpoints, bearer),
		IsQuotaExceeded: raw["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(raw, "expiresAt")),
		UserQuota:       extractQuotaBucket(raw, "userQuota"),
		AddonQuota:      extractQuotaBucket(raw, "addOnQuota"),
	}
	return quota, nil
}

// quotaBearerToken 把凭证换成额度接口认识的 bearer token。
func quotaBearerToken(ctx context.Context, cred qoderCredential) (string, error) {
	deviceToken, _ := bridge.ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		return deviceToken, nil
	}
	seed := cosy.FingerprintSeed("", cred.Token)
	endpoints := qoder.GetEndpoints(cred.Region)
	jobToken, errExchange := cosy.ExchangeJobToken(ctx, cred.Token,
		cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), endpoints.JobTokenURL)
	if errExchange != nil {
		return "", classifyCredentialError(fmt.Errorf("exchange token: %w", errExchange))
	}
	oauthToken := bridge.StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return "", newPluginError("qoder_credential_invalid", "jobToken 响应里没有 securityOauthToken", http.StatusUnauthorized)
	}
	return oauthToken, nil
}

// httpGetBearerJSON 发一个带 Bearer 的 GET 并解析成 JSON 对象。
func httpGetBearerJSON(ctx context.Context, endpoint, token string) (map[string]interface{}, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, newPluginError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errDo := httpx.Client(ctx, quotaHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, newPluginError("qoder_upstream_transient", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, newPluginError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, newPluginError("qoder_upstream_error",
			fmt.Sprintf("额度接口返回 HTTP %d: %s", resp.StatusCode, truncateForMessage(string(body), 300)), status)
	}
	var result map[string]interface{}
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return nil, newPluginError("qoder_upstream_error", "额度接口返回的不是 JSON: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// fetchPlanTierName 查询套餐名；失败不影响额度展示，只留空。
func fetchPlanTierName(ctx context.Context, endpoints qoder.Endpoints, token string) string {
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		logger.Debug("fetch plan tier failed: %v", errPlan)
		return ""
	}
	if name, ok := result["plan_tier_name"].(string); ok {
		return name
	}
	return ""
}

// extractQuotaBucket 提取额度桶；全零视为不存在（上游会返回空对象）。
func extractQuotaBucket(data map[string]interface{}, key string) *qoderQuotaBucket {
	obj, ok := data[key].(map[string]interface{})
	if !ok {
		return nil
	}
	bucket := &qoderQuotaBucket{
		Used:      toFloat(obj, "used"),
		Total:     toFloat(obj, "total"),
		Remaining: toFloat(obj, "remaining"),
	}
	if reset, ok := obj["resetTime"].(string); ok {
		bucket.ResetTime = reset
	} else if reset, ok := obj["reset_time"].(string); ok {
		bucket.ResetTime = reset
	}
	if bucket.Total == 0 && bucket.Used == 0 && bucket.Remaining == 0 {
		return nil
	}
	return bucket
}

// buildQuotaResponse 把上游额度翻译成管理端可渲染的分组。
func buildQuotaResponse(quota *qoderQuota) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	if quota.Plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: quota.Plan, TierName: quota.Plan, TierID: quota.Plan}
	}
	if group := quotaGroup("套餐额度", quota.UserQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if group := quotaGroup("个人拓展包", quota.AddonQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if quota.UserQuota != nil {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:   "user_quota_remaining",
			Label: "套餐剩余额度",
			Value: quota.UserQuota.Remaining,
			Unit:  "credits",
		})
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:   "user_quota_used",
			Label: "套餐已用额度",
			Value: quota.UserQuota.Used,
			Unit:  "credits",
		})
	}
	if quota.AddonQuota != nil {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:   "addon_quota_remaining",
			Label: "拓展包剩余额度",
			Value: quota.AddonQuota.Remaining,
			Unit:  "credits",
		})
	}
	if quota.IsQuotaExceeded {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:   "quota_exceeded",
			Label: "额度状态",
			Value: 1,
			Unit:  "已用尽",
		})
	}
	if quota.ExpiresAt > 0 {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:    "expires_at",
			Label:  "额度到期时间",
			Value:  float64(quota.ExpiresAt),
			Format: "number",
			Unit:   "unix",
		})
	}
	return response
}

// quotaGroup 把额度桶转成"剩余比例"分组。
func quotaGroup(displayName string, bucket *qoderQuotaBucket) *pluginapi.QuotaGroup {
	if bucket == nil {
		return nil
	}
	fraction := 0.0
	switch {
	case bucket.Total > 0:
		fraction = clampFraction(bucket.Remaining / bucket.Total)
	case bucket.Remaining > 0:
		fraction = 1
	}
	description := fmt.Sprintf("已用 %.0f / 总计 %.0f，剩余 %.0f", bucket.Used, bucket.Total, bucket.Remaining)
	entry := pluginapi.QuotaBucket{
		Window:            "billing-cycle",
		RemainingFraction: fraction,
		ResetTime:         bucket.ResetTime,
		Description:       description,
	}
	return &pluginapi.QuotaGroup{DisplayName: displayName, Buckets: []pluginapi.QuotaBucket{entry}}
}

func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func toFloat(m map[string]interface{}, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	}
	return 0
}

func truncateForMessage(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
