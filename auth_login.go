package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// 本文件实现 CPA 面板的 OAuth 登录（宿主 ABI：auth.login.start / auth.login.poll）。
//
// 宿主把 `GET /v0/management/qoder-auth-url` 交给插件的 StartLogin（内部按
// `<provider>-auth-url` 路由到已注册的 auth provider），面板随后轮询 oauth-callback
// 触发 PollLogin。登录成功后插件返回 AuthData，由宿主自己落盘成 auth 文件——
// 与手工导入的凭证完全同构，所以额度、签到、刷新链路都能直接复用。
//
// 流程对齐 Qoder 官方 device flow + PKCE（与上游 qoder2api/account/oauth.go 一致）：
//
//	start: {DeviceLoginBase}?nonce=&challenge=&challenge_method=S256&client_id=
//	poll : {PollEndpoint}?nonce=&verifier=&challenge_method=S256
//	       404 = 用户还没在浏览器完成授权；200 = {token, refresh_token}
const (
	// qoderOAuthClientID 是 Qoder 桌面端使用的公开 client id（与上游实现一致）。
	qoderOAuthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// authLoginFlowTTL 是单次登录会话的有效期（与上游 10 分钟一致）。
	authLoginFlowTTL = 10 * time.Minute
	// authLoginHTTPTimeout 限制单次上游请求时间。
	authLoginHTTPTimeout = 20 * time.Second
	// authLoginMaxResponseBytes 限制读取上游响应的字节数。
	authLoginMaxResponseBytes = 64 << 10
)

// authLoginUpstreamInterval 是同一会话两次访问上游 poll 端点之间的最小间隔。
// 面板可能每 1~2 秒轮询一次，节流后不至于把上游打满。测试里会被调小/调大。
var authLoginUpstreamInterval = 1200 * time.Millisecond

// pendingAuthLogin 是一次进行中的登录会话。
// 用指针保存：轮询会更新 lastPoll，需要写回 map。
type pendingAuthLogin struct {
	state    string
	nonce    string
	verifier string
	region   qoder.Region
	deadline time.Time
	lastPoll time.Time
}

var authLoginStore = struct {
	mu      sync.Mutex
	pending map[string]*pendingAuthLogin
}{pending: map[string]*pendingAuthLogin{}}

type authLoginStartRPCRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authLoginPollRPCRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthLoginStart 生成 PKCE 参数与登录 URL，并登记一个待轮询的登录会话。
func handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpc authLoginStartRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	region := authLoginRegion(rpc.Metadata)
	verifier, challenge, errPKCE := newPKCEPair()
	if errPKCE != nil {
		return nil, newPluginError("qoder_login_failed", errPKCE.Error(), http.StatusInternalServerError)
	}
	nonce, errNonce := randomHex(16)
	if errNonce != nil {
		return nil, newPluginError("qoder_login_failed", errNonce.Error(), http.StatusInternalServerError)
	}
	state, errState := randomHex(16)
	if errState != nil {
		return nil, newPluginError("qoder_login_failed", errState.Error(), http.StatusInternalServerError)
	}

	endpoints := qoder.GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", qoderOAuthClientID)
	loginURL := endpoints.DeviceLoginBase + "?" + params.Encode()

	deadline := time.Now().Add(authLoginFlowTTL)
	storeAuthLoginPending(&pendingAuthLogin{
		state:    state,
		nonce:    nonce,
		verifier: verifier,
		region:   region,
		deadline: deadline,
	})
	logger.Info("oauth login started (region=%s, state=%s)", region, shortState(state))
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       loginURL,
		State:     state,
		ExpiresAt: deadline,
		Metadata:  map[string]any{"region": string(region)},
	})
}

// handleAuthLoginPoll 轮询一次登录会话：等待授权 → 取 token → 返回可直接落盘的 AuthData。
func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpc authLoginPollRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	entry, ok := authLoginPending(strings.TrimSpace(rpc.State))
	if !ok {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		})
	}
	if time.Now().After(entry.deadline) {
		dropAuthLoginPending(entry.state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录超时：10 分钟内未完成授权，请重新发起登录",
		})
	}
	// 节流：面板轮询频率高于上游建议（上游客户端是 1 秒一次）。
	if wait := authLoginUpstreamInterval - time.Since(entry.lastPoll); wait > 0 {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待在浏览器中完成授权…",
		})
	}
	markAuthLoginPolled(entry)

	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), authLoginHTTPTimeout)
	defer cancel()

	token, refreshToken, waiting, errPoll := pollQoderDeviceToken(ctx, entry)
	if errPoll != nil {
		// 网络抖动不该直接判死：保持会话，下次轮询再试。
		logger.Info("oauth login poll failed (state=%s), will retry: %v", shortState(entry.state), errPoll)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "与 Qoder 通信失败，正在重试：" + errPoll.Error(),
		})
	}
	if waiting {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待在浏览器中完成授权…",
		})
	}

	profile := fetchQoderLoginProfile(ctx, token, entry.region)
	plan := fetchQoderLoginPlan(ctx, token, entry.region)
	storage, cred := buildLoginAuthStorage(entry.region, token, refreshToken, profile, plan)
	authID := loginAuthID(profile, entry.state)
	dropAuthLoginPending(entry.state)
	logger.Info("oauth login succeeded: id=%s region=%s email=%s refresh=%t",
		authID, entry.region, cred.Email, cred.RefreshToken != "")

	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:         providerKey,
			ID:               authID,
			FileName:         authID + ".json",
			Label:            firstNonEmpty(profile.Name, profile.Email, authID),
			StorageJSON:      storage,
			Metadata:         credentialMetadata(cred),
			Attributes:       credentialAttributes(cred),
			NextRefreshAfter: nextAuthRefreshAt(),
		},
	})
}

// qoderLoginProfile 是 /userinfo 返回的账号信息（取不到时保持零值）。
type qoderLoginProfile struct {
	UserID string
	Email  string
	Name   string
	Type   string
}

// pollQoderDeviceToken 访问上游 poll 端点。
// waiting=true 表示用户尚未授权（上游 404），调用方应继续等待。
func pollQoderDeviceToken(ctx context.Context, entry *pendingAuthLogin) (token string, refreshToken string, waiting bool, err error) {
	endpoints := qoder.GetEndpoints(entry.region)
	pollURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		endpoints.PollEndpoint, url.QueryEscape(entry.nonce), url.QueryEscape(entry.verifier))
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if errRequest != nil {
		return "", "", false, errRequest
	}
	resp, errDo := httpx.Client(ctx, authLoginHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", false, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			logger.Debug("oauth login poll: close body: %v", errClose)
		}
	}()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, authLoginMaxResponseBytes))

	switch resp.StatusCode {
	case http.StatusNotFound:
		// 上游语义：还没授权。
		return "", "", true, nil
	case http.StatusOK:
	default:
		return "", "", false, fmt.Errorf("上游 poll 返回 HTTP %d：%s", resp.StatusCode, summarizeBody(body))
	}

	var payload map[string]interface{}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return "", "", false, fmt.Errorf("上游 poll 响应不是合法 JSON：%w", errUnmarshal)
	}
	deviceToken, _ := payload["token"].(string)
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return "", "", false, fmt.Errorf("上游 poll 响应里没有 token")
	}
	refreshToken, _ = payload["refresh_token"].(string)
	return deviceToken, strings.TrimSpace(refreshToken), false, nil
}

// fetchQoderLoginProfile 取账号信息（失败只降级：没有 email/name 也能用）。
func fetchQoderLoginProfile(ctx context.Context, token string, region qoder.Region) qoderLoginProfile {
	endpoints := qoder.GetEndpoints(region)
	result, errProfile := httpGetBearerJSON(ctx, endpoints.UserinfoBase, token)
	if errProfile != nil {
		logger.Info("oauth login: fetch userinfo failed, falling back to token-only auth: %v", errProfile)
		return qoderLoginProfile{}
	}
	return qoderLoginProfile{
		UserID: strings.TrimSpace(authLoginString(result["userId"])),
		Email:  strings.ToLower(strings.TrimSpace(authLoginString(result["email"]))),
		Name:   strings.TrimSpace(authLoginString(result["name"])),
		Type:   strings.TrimSpace(authLoginString(result["userType"])),
	}
}

// fetchQoderLoginPlan 取套餐名（失败留空）。
func fetchQoderLoginPlan(ctx context.Context, token string, region qoder.Region) string {
	endpoints := qoder.GetEndpoints(region)
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		logger.Debug("oauth login: fetch plan failed: %v", errPlan)
		return ""
	}
	return strings.TrimSpace(authLoginString(result["plan_tier_name"]))
}

// buildLoginAuthStorage 生成与导入路径同构的 auth 文件内容。
// 保持扁平格式（device_token/refresh_token/region/label/email/plan），
// 刷新时代码（auth.refresh）能原样读回这些字段。
func buildLoginAuthStorage(region qoder.Region, token, refreshToken string, profile qoderLoginProfile, plan string) ([]byte, qoderCredential) {
	payload := map[string]interface{}{
		"type":         providerKey,
		"device_token": token,
		"region":       string(region),
		"auth_mode":    "oauth",
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	label := firstNonEmpty(profile.Name, profile.Email)
	if label != "" {
		payload["label"] = label
	}
	if profile.Email != "" {
		payload["email"] = profile.Email
	}
	if plan != "" {
		payload["plan"] = plan
	}
	if profile.UserID != "" {
		payload["qoder_account_id"] = profile.UserID
	}
	if profile.Type != "" {
		payload["qoder_user_type"] = profile.Type
	}
	storage, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		// map 里只有字符串，理论上不会失败；退化成紧凑编码保证登录不中断。
		storage, _ = json.Marshal(payload)
	}
	cred := qoderCredential{
		Token:        token,
		RefreshToken: refreshToken,
		Region:       region,
		Label:        label,
		Email:        profile.Email,
	}
	return storage, cred
}

// loginAuthID 生成宿主侧 auth 标识（会用于落盘文件名，必须是文件名安全字符）。
func loginAuthID(profile qoderLoginProfile, state string) string {
	base := firstNonEmpty(profile.Email, profile.UserID, "login-"+shortState(state))
	return providerKey + "-" + sanitizeAuthID(base)
}

func sanitizeAuthID(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-.@")
	if out == "" {
		out = "account"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-.@")
	}
	return out
}

// authLoginRegion 决定登录走哪个区域：请求参数优先，其次插件配置。
func authLoginRegion(metadata map[string]any) qoder.Region {
	if metadata != nil {
		if raw, ok := metadata["region"]; ok {
			switch value := raw.(type) {
			case string:
				if strings.TrimSpace(value) != "" {
					return qoder.NormalizeRegion(value)
				}
			case []any:
				if len(value) > 0 {
					if text, okText := value[0].(string); okText && strings.TrimSpace(text) != "" {
						return qoder.NormalizeRegion(text)
					}
				}
			case []string:
				if len(value) > 0 && strings.TrimSpace(value[0]) != "" {
					return qoder.NormalizeRegion(value[0])
				}
			}
		}
	}
	return loadedConfig().Region
}

// newPKCEPair 生成 PKCE verifier 与 S256 challenge（与上游同样用 32 字节随机）。
func newPKCEPair() (verifier, challenge string, err error) {
	if _, errRead := rand.Read(make([]byte, 1)); errRead != nil {
		return "", "", errRead
	}
	buffer := make([]byte, 32)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", errRead
	}
	return fmt.Sprintf("%x", buffer), nil
}

// shortState 只用于日志，避免把完整 state 写进日志。
func shortState(state string) string {
	if len(state) <= 8 {
		return state
	}
	return state[:8]
}

func summarizeBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return "(空响应)"
	}
	return text
}

// asString 宽容地把 JSON 值转成字符串（上游个别字段可能是数字）。
func authLoginString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%.0f", typed)
	default:
		return ""
	}
}

func storeAuthLoginPending(entry *pendingAuthLogin) {
	now := time.Now()
	authLoginStore.mu.Lock()
	defer authLoginStore.mu.Unlock()
	// 顺手清掉过期会话，避免长时间运行后 map 里堆垃圾。
	for key, item := range authLoginStore.pending {
		if now.After(item.deadline) {
			delete(authLoginStore.pending, key)
		}
	}
	authLoginStore.pending[entry.state] = entry
}

func authLoginPending(state string) (*pendingAuthLogin, bool) {
	if state == "" {
		return nil, false
	}
	authLoginStore.mu.Lock()
	defer authLoginStore.mu.Unlock()
	entry, ok := authLoginStore.pending[state]
	return entry, ok
}

func markAuthLoginPolled(entry *pendingAuthLogin) {
	authLoginStore.mu.Lock()
	defer authLoginStore.mu.Unlock()
	entry.lastPoll = time.Now()
}

func dropAuthLoginPending(state string) {
	authLoginStore.mu.Lock()
	defer authLoginStore.mu.Unlock()
	delete(authLoginStore.pending, state)
}
