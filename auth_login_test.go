package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
)

// 这些用例覆盖 CPA 面板 OAuth 登录（宿主 ABI：auth.login.start / auth.login.poll）。
// 背景：宿主把 GET /v0/management/qoder-auth-url 路由到插件的 StartLogin，
// 曾经因为插件没实现该方法而让面板报 “failed to generate authorization url”。

// callAuthLoginStart 调用 auth.login.start 并解出响应。
func callAuthLoginStart(t *testing.T, payload map[string]any) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal start request: %v", errMarshal)
	}
	body, errCall := handleAuthLoginStart(raw)
	if errCall != nil {
		t.Fatalf("handleAuthLoginStart: %v", errCall)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(body, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("start failed: %+v", env.Error)
	}
	var resp pluginapi.AuthLoginStartResponse
	if errResult := json.Unmarshal(env.Result, &resp); errResult != nil {
		t.Fatalf("decode start response: %v", errResult)
	}
	return resp
}

// callAuthLoginPoll 调用 auth.login.poll 并解出响应。
func callAuthLoginPoll(t *testing.T, payload map[string]any) pluginapi.AuthLoginPollResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal poll request: %v", errMarshal)
	}
	body, errCall := handleAuthLoginPoll(raw)
	if errCall != nil {
		t.Fatalf("handleAuthLoginPoll: %v", errCall)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(body, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("poll failed: %+v", env.Error)
	}
	var resp pluginapi.AuthLoginPollResponse
	if errResult := json.Unmarshal(env.Result, &resp); errResult != nil {
		t.Fatalf("decode poll response: %v", errResult)
	}
	return resp
}

// loginUpstream 装一个假 Qoder 上游：poll 端点由 authorized 控制，userinfo/plan 返回固定账号。
type loginUpstream struct {
	authorized bool
	pollStatus int
	pollBody   string
	pollCalls  int
	userinfo   map[string]any
	plan       string
}

func installLoginUpstream(host *fakeHost, upstream *loginUpstream) {
	host.upstream = func(method, rawURL, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(rawURL, "/deviceToken/poll"):
			upstream.pollCalls++
			if !upstream.authorized {
				return fakeUpstreamResponse{Status: 404, Body: `{"error":"not authorized"}`}
			}
			if upstream.pollStatus != 0 {
				return fakeUpstreamResponse{Status: upstream.pollStatus, Body: upstream.pollBody}
			}
			return fakeUpstreamResponse{Status: 200, Body: upstream.pollBody}
		case strings.Contains(rawURL, "/userinfo"):
			payload, _ := json.Marshal(upstream.userinfo)
			return fakeUpstreamResponse{Status: 200, Body: string(payload)}
		case strings.Contains(rawURL, "/user/plan"):
			payload, _ := json.Marshal(map[string]any{"plan_tier_name": upstream.plan})
			return fakeUpstreamResponse{Status: 200, Body: string(payload)}
		default:
			return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected url"}`}
		}
	}
}

func newLoginUpstream() *loginUpstream {
	return &loginUpstream{
		pollBody: `{"token":"dt-login-token","refresh_token":"rt-login-token"}`,
		userinfo: map[string]any{"userId": "7231", "email": "Tester@Example.com", "name": "Tester", "userType": "personal"},
		plan:     "Pro",
	}
}

// TestAuthLoginStartBuildsDeviceFlowURL 校验登录 URL 的参数与上游（桌面端）完全一致。
func TestAuthLoginStartBuildsDeviceFlowURL(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	resp := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	parsed, errParse := url.Parse(resp.URL)
	if errParse != nil {
		t.Fatalf("登录 URL 非法: %v", errParse)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != "https://qoder.com/device/selectAccounts" {
		t.Fatalf("登录页 = %q, want 国际版 device/selectAccounts", got)
	}
	query := parsed.Query()
	if query.Get("client_id") != qoderOAuthClientID {
		t.Fatalf("client_id = %q, want %q", query.Get("client_id"), qoderOAuthClientID)
	}
	if query.Get("challenge_method") != "S256" {
		t.Fatalf("challenge_method = %q, want S256", query.Get("challenge_method"))
	}
	if len(query.Get("challenge")) < 40 || len(query.Get("nonce")) < 20 {
		t.Fatalf("challenge/nonce 太短: challenge=%q nonce=%q", query.Get("challenge"), query.Get("nonce"))
	}
	if resp.State == "" {
		t.Fatal("state 必须非空（面板靠它轮询）")
	}
	if resp.Provider != providerKey {
		t.Fatalf("provider = %q, want %q", resp.Provider, providerKey)
	}
	if time.Until(resp.ExpiresAt) <= 0 || time.Until(resp.ExpiresAt) > authLoginFlowTTL+time.Minute {
		t.Fatalf("ExpiresAt 不在预期区间: %v", resp.ExpiresAt)
	}
	// 会话必须真的登记进去，否则 poll 会立刻报“会话不存在”。
	if _, ok := authLoginPending(resp.State); !ok {
		t.Fatal("登录会话未登记")
	}
	if region, _ := resp.Metadata["region"].(string); region != "global" {
		t.Fatalf("metadata.region = %v, want global", resp.Metadata["region"])
	}
}

// TestAuthLoginStartUsesDistinctStates 每次登录都必须有独立 state，避免并发登录互相覆盖。
func TestAuthLoginStartUsesDistinctStates(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	first := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	second := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	if first.State == second.State {
		t.Fatal("两次登录的 state 不能相同")
	}
	if first.URL == second.URL {
		t.Fatal("两次登录的 PKCE 参数应不同")
	}
	for _, state := range []string{first.State, second.State} {
		if _, ok := authLoginPending(state); !ok {
			t.Fatalf("state %s 未登记", state)
		}
	}
}

// TestAuthLoginRegionFollowsRequestAndConfig 区域决定登录域名（国际版/国内版不同）。
func TestAuthLoginRegionFollowsRequestAndConfig(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t, func(cfg *pluginConfig) { cfg.Region = "cn" })

	// 未指定 region → 跟配置（cn）
	fromConfig := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	if !strings.HasPrefix(fromConfig.URL, "https://qoder.com.cn/device/selectAccounts") {
		t.Fatalf("配置 cn 时登录页 = %q", fromConfig.URL)
	}
	// 显式指定 global → 覆盖配置
	explicit := callAuthLoginStart(t, map[string]any{"Provider": "qoder", "Metadata": map[string]any{"region": "global"}})
	if !strings.HasPrefix(explicit.URL, "https://qoder.com/device/selectAccounts") {
		t.Fatalf("显式 global 时登录页 = %q", explicit.URL)
	}
	// 宿主 query 参数会变成 []any，也要认
	asQuery := callAuthLoginStart(t, map[string]any{"Provider": "qoder", "Metadata": map[string]any{"region": []any{"global"}}})
	if !strings.HasPrefix(asQuery.URL, "https://qoder.com/device/selectAccounts") {
		t.Fatalf("query 形式 region 未生效: %q", asQuery.URL)
	}
}

// TestAuthLoginPollWaitsThenSucceeds 覆盖面板的真实节奏：先 wait，用户授权后 success。
func TestAuthLoginPollWaitsThenSucceeds(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	installLoginUpstream(host, upstream)

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})

	waiting := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if waiting.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("未授权时应为 pending，实际 %q（message=%q）", waiting.Status, waiting.Message)
	}
	if !strings.Contains(waiting.Message, "浏览器") {
		t.Fatalf("pending 消息应提示去浏览器授权，实际 %q", waiting.Message)
	}

	upstream.authorized = true
	success := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if success.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("授权后应为 success，实际 %q（message=%q）", success.Status, success.Message)
	}
	if _, ok := authLoginPending(start.State); ok {
		t.Fatal("成功后必须清掉登录会话")
	}
	// 会话结束后再轮询应报错，而不是无限 pending。
	again := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if again.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("重复轮询应为 error，实际 %q", again.Status)
	}
}

// TestAuthLoginPollAuthDataMatchesImportShape 登录产物必须与导入凭证同构：
// 同一个 auth.refresh 路径能解析、宿主也能从 Metadata 看出“可刷新”。
func TestAuthLoginPollAuthDataMatchesImportShape(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	upstream.authorized = true
	installLoginUpstream(host, upstream)

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	resp := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll 状态 = %q（message=%q）", resp.Status, resp.Message)
	}
	auth := resp.Auth
	if auth.Provider != providerKey {
		t.Fatalf("provider = %q, want %q", auth.Provider, providerKey)
	}
	if !strings.HasPrefix(auth.ID, providerKey+"-") || !strings.HasSuffix(auth.FileName, ".json") {
		t.Fatalf("ID/FileName 不符合落盘规则: id=%q file=%q", auth.ID, auth.FileName)
	}
	if auth.ID != strings.TrimSuffix(auth.FileName, ".json") {
		t.Fatalf("FileName 应由 ID 派生: id=%q file=%q", auth.ID, auth.FileName)
	}
	if auth.Label != "Tester" {
		t.Fatalf("label = %q, want Tester（优先用 /userinfo 的 name）", auth.Label)
	}
	if auth.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter 不能是零值（否则宿主会当成从未刷新的凭证）")
	}

	// 宿主判断“能不能刷新”看 Metadata 里的 refresh_token。
	if got, _ := auth.Metadata["refresh_token"].(string); got != "rt-login-token" {
		t.Fatalf("metadata.refresh_token = %v, want rt-login-token", auth.Metadata["refresh_token"])
	}
	if got, _ := auth.Metadata["auth_mode"].(string); got != "oauth" {
		t.Fatalf("metadata.auth_mode = %v, want oauth", auth.Metadata["auth_mode"])
	}
	if got := auth.Attributes["region"]; got != "global" {
		t.Fatalf("attributes.region = %q, want global", got)
	}

	// 关键：落盘内容必须能被导入路径的解析器读回（同构证明）。
	cred, errCred := parseQoderCredential(auth.StorageJSON, "global")
	if errCred != nil {
		t.Fatalf("登录产物无法被凭证解析器读取: %v", errCred)
	}
	if cred.Token != "dt-login-token" || cred.RefreshToken != "rt-login-token" {
		t.Fatalf("token 解析不一致: token=%q refresh=%q", cred.Token, cred.RefreshToken)
	}
	if cred.Email != "tester@example.com" {
		t.Fatalf("email = %q（应统一小写）", cred.Email)
	}
	if string(cred.Region) != "global" {
		t.Fatalf("region = %q", cred.Region)
	}

	var raw map[string]any
	if errUnmarshal := json.Unmarshal(auth.StorageJSON, &raw); errUnmarshal != nil {
		t.Fatalf("storage 不是合法 JSON: %v", errUnmarshal)
	}
	for _, key := range []string{"type", "device_token", "refresh_token", "region", "label", "email", "plan", "auth_mode"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("storage 缺少字段 %q：%s", key, auth.StorageJSON)
		}
	}
	if raw["plan"] != "Pro" {
		t.Fatalf("plan = %v, want Pro", raw["plan"])
	}
	if raw["qoder_account_id"] != "7231" {
		t.Fatalf("qoder_account_id = %v, want 7231", raw["qoder_account_id"])
	}
}

// TestAuthLoginPollWithoutUserinfoStillSucceeds /userinfo 失败不能阻断登录（凭证本身有效即可）。
func TestAuthLoginPollWithoutUserinfoStillSucceeds(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	upstream.authorized = true
	upstream.userinfo = nil // 空对象 → 拿不到 email/name
	installLoginUpstream(host, upstream)

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	resp := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll 状态 = %q（message=%q）", resp.Status, resp.Message)
	}
	if !strings.HasPrefix(resp.Auth.ID, providerKey+"-") || resp.Auth.ID == providerKey+"-" {
		t.Fatalf("缺少 email 时 ID 仍需可用: %q", resp.Auth.ID)
	}
	if resp.Auth.Label == "" {
		t.Fatal("label 不能为空（面板要显示）")
	}
}

// TestAuthLoginPollKeepsSessionOnUpstreamError 上游抖动/5xx 时应继续等待而不是判死。
func TestAuthLoginPollKeepsSessionOnUpstreamError(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	upstream.authorized = true
	upstream.pollStatus = 500
	upstream.pollBody = `{"error":"boom"}`
	installLoginUpstream(host, upstream)

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	failed := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if failed.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("上游 5xx 时应保持 pending（可重试），实际 %q", failed.Status)
	}
	if !strings.Contains(failed.Message, "500") {
		t.Fatalf("消息应带上游状态码，实际 %q", failed.Message)
	}
	if _, ok := authLoginPending(start.State); !ok {
		t.Fatal("上游失败不应清掉会话")
	}

	// 上游恢复（状态码与响应体都要恢复）→ 正常成功
	upstream.pollStatus = 0
	upstream.pollBody = `{"token":"dt-login-token","refresh_token":"rt-login-token"}`
	recovered := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if recovered.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("上游恢复后应成功，实际 %q（message=%q）", recovered.Status, recovered.Message)
	}
}

// TestAuthLoginPollRejectsUnknownAndExpiredState 面板拿着旧 state 轮询时要给明确错误。
func TestAuthLoginPollRejectsUnknownAndExpiredState(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	unknown := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": "deadbeef"})
	if unknown.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("未知 state 应为 error，实际 %q", unknown.Status)
	}
	if !strings.Contains(unknown.Message, "重新发起") {
		t.Fatalf("错误消息应指导用户重新登录，实际 %q", unknown.Message)
	}

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	entry, ok := authLoginPending(start.State)
	if !ok {
		t.Fatal("会话未登记")
	}
	entry.deadline = time.Now().Add(-time.Second) // 手工制造超时
	expired := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if expired.Status != pluginapi.AuthLoginStatusError || !strings.Contains(expired.Message, "超时") {
		t.Fatalf("超时状态错误: status=%q message=%q", expired.Status, expired.Message)
	}
	if _, still := authLoginPending(start.State); still {
		t.Fatal("超时会话应被清理")
	}
}

// TestAuthLoginPollThrottlesUpstreamCalls 面板轮询比上游建议快，插件侧要节流。
func TestAuthLoginPollThrottlesUpstreamCalls(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	installLoginUpstream(host, upstream)

	start := callAuthLoginStart(t, map[string]any{"Provider": "qoder"})
	callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	first := upstream.pollCalls
	if first != 1 {
		t.Fatalf("首次轮询应访问上游 1 次，实际 %d", first)
	}

	// 打开节流后连续轮询：窗口内一次上游都不该打（面板轮询远快于上游建议节奏）。
	authLoginUpstreamInterval = 50 * time.Millisecond
	defer func() { authLoginUpstreamInterval = 0 }()
	for i := 0; i < 3; i++ {
		resp := callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
		if resp.Status != pluginapi.AuthLoginStatusPending {
			t.Fatalf("节流期间应为 pending，实际 %q", resp.Status)
		}
	}
	if upstream.pollCalls != first {
		t.Fatalf("节流窗口内不应访问上游：calls = %d, want %d", upstream.pollCalls, first)
	}

	// 超过节流窗口后再轮询，才继续访问上游。
	time.Sleep(2 * authLoginUpstreamInterval)
	callAuthLoginPoll(t, map[string]any{"Provider": "qoder", "State": start.State})
	if upstream.pollCalls != first+1 {
		t.Fatalf("节流窗口结束后应恢复访问上游：calls = %d, want %d", upstream.pollCalls, first+1)
	}
}

// TestAuthLoginWireContractWithHostFieldNames 宿主序列化用 Go 字段名（无 json tag），必须能解析。
func TestAuthLoginWireContractWithHostFieldNames(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	upstream := newLoginUpstream()
	upstream.authorized = true
	installLoginUpstream(host, upstream)

	// 与宿主 pluginapi.AuthLoginStartRequest 的 JSON 形状一致。
	startRaw := []byte(`{"Provider":"qoder","BaseURL":"http://127.0.0.1:8317/v0/management/oauth-callback","Host":{"Port":8317},"Metadata":{"region":"global"}}`)
	body, errStart := handleAuthLoginStart(startRaw)
	if errStart != nil {
		t.Fatalf("handleAuthLoginStart: %v", errStart)
	}
	var startEnv envelope
	if errUnmarshal := json.Unmarshal(body, &startEnv); errUnmarshal != nil || !startEnv.OK {
		t.Fatalf("宿主字段名形式无法解析: ok=%v err=%+v raw=%s", startEnv.OK, startEnv.Error, body)
	}
	var start pluginapi.AuthLoginStartResponse
	if errResult := json.Unmarshal(startEnv.Result, &start); errResult != nil {
		t.Fatalf("decode start: %v", errResult)
	}

	pollRaw := []byte(`{"Provider":"qoder","State":"` + start.State + `","Host":{"Port":8317}}`)
	pollBody, errPoll := handleAuthLoginPoll(pollRaw)
	if errPoll != nil {
		t.Fatalf("handleAuthLoginPoll: %v", errPoll)
	}
	var pollEnv envelope
	if errUnmarshal := json.Unmarshal(pollBody, &pollEnv); errUnmarshal != nil || !pollEnv.OK {
		t.Fatalf("poll 无法解析: ok=%v err=%+v", pollEnv.OK, pollEnv.Error)
	}
	var poll pluginapi.AuthLoginPollResponse
	if errResult := json.Unmarshal(pollEnv.Result, &poll); errResult != nil {
		t.Fatalf("decode poll: %v", errResult)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll 状态 = %q（message=%q）", poll.Status, poll.Message)
	}
}

// TestSanitizeAuthID 落盘标识必须是文件名安全字符（宿主会拿它当文件名）。
func TestSanitizeAuthID(t *testing.T) {
	cases := map[string]string{
		"tester@example.com":     "tester@example.com",
		"a/b\\c:d*e?f":           "a-b-c-d-e-f",
		"  ..weird..  ":          "weird",
		"":                       "account",
		strings.Repeat("x", 100): strings.Repeat("x", 64),
	}
	for input, want := range cases {
		if got := sanitizeAuthID(input); got != want {
			t.Fatalf("sanitizeAuthID(%q) = %q, want %q", input, got, want)
		}
	}
	for _, bad := range []string{"..", ".", ""} {
		if got := sanitizeAuthID(bad); got == "" || strings.ContainsAny(got, `/\:*?"<>|`) {
			t.Fatalf("sanitizeAuthID(%q) 结果不安全: %q", bad, got)
		}
	}
}
