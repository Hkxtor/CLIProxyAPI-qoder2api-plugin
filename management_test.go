package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"qoder2api-plugin/cpasdk/pluginapi"
)

func TestMatchesManagementAndResourcePaths(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/plugins/qoder2api/status", true},
		{"/plugins/qoder2api/status/", true},
		{"/v0/management/plugins/qoder2api/status", true},
		{"/v0/management/plugins/other/status", false},
		{"/plugins/qoder2api/statuses", false},
	}
	for _, tc := range cases {
		if got := matchesManagementPath(tc.path, "/status"); got != tc.want {
			t.Errorf("matchesManagementPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if !matchesResourcePath("/v0/resource/plugins/qoder2api/console", "/console") {
		t.Error("resource path should match")
	}
	if matchesResourcePath("/v0/resource/plugins/qoder2api/status", "/console") {
		t.Error("wrong resource path must not match")
	}
}

// TestStatusPayloadNeverLeaksCredentials 是安全断言：管理接口返回里
// 不允许出现 token 原文（页面/管理端可能被截图、导出）。
func TestStatusPayloadNeverLeaksCredentials(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.authFiles = []pluginapi.HostAuthFileEntry{
		{ID: "qoder-main", AuthIndex: "1", Name: "qoder-main.json", Type: providerKey, Label: "主号", Status: "active"},
	}
	host.authJSON["1"] = `{"type":"qoder","token":"pt-SUPER-SECRET-VALUE","region":"cn"}`
	if errStore := storeCachedModels([]cachedModel{{Key: "gmodel", DisplayName: "Performance", Enable: true}}, "global"); errStore != nil {
		t.Fatalf("storeCachedModels: %v", errStore)
	}
	if errMutate := mutateState(func(state *pluginState) {
		state.Checkin["qoder-main"] = checkinRecord{LastStatus: checkinStatusClaimed, LastMessage: "领取成功 +100", TotalCredits: 100, Streak: 3}
	}); errMutate != nil {
		t.Fatalf("mutateState: %v", errMutate)
	}

	payload := buildStatusPayload()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal status: %v", errMarshal)
	}
	text := string(raw)
	if strings.Contains(text, "SUPER-SECRET") {
		t.Fatalf("status payload leaked a credential: %s", text)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].ID != "qoder-main" {
		t.Fatalf("accounts = %+v", payload.Accounts)
	}
	if payload.Accounts[0].CheckinStatus != checkinStatusClaimed || payload.Accounts[0].StreakDays != 3 {
		t.Fatalf("checkin state not surfaced: %+v", payload.Accounts[0])
	}
	if payload.ModelCount != 1 || len(payload.ModelPreview) != 1 {
		t.Fatalf("model summary = %d / %v", payload.ModelCount, payload.ModelPreview)
	}
	// 管理页展示的注册 ID 用人类可读名（而不是上游 SKU）。
	if !strings.Contains(text, "qoder-Performance") {
		t.Fatalf("registered id should be shown: %s", text)
	}
	if strings.Contains(text, "qoder-gmodel") {
		t.Fatalf("管理页不应再展示裸 SKU 作为注册 ID: %s", text)
	}
}

func TestStatusPayloadWarnsWhenNoAccounts(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	payload := buildStatusPayload()
	if payload.Warning == "" {
		t.Fatal("empty account list should surface an actionable warning")
	}
	if !strings.Contains(payload.Warning, "qoder-") {
		t.Fatalf("warning should name the expected auth file: %q", payload.Warning)
	}
}

func TestManagementDispatchRoutes(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "qoder-main", AuthIndex: "1", Type: providerKey}}
	host.authJSON["1"] = `{"type":"qoder","token":"pt-test","region":"global"}`
	setupQoderUpstream(host, "")

	// 资源路由：静态页面
	response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/qoder2api/console"})
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "<html") {
		t.Fatalf("console page response = %d %s", response.StatusCode, truncateForMessage(string(response.Body), 80))
	}

	// 管理路由：状态
	response = dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/qoder2api/status"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status response = %d %s", response.StatusCode, response.Body)
	}
	var status statusPayload
	if errUnmarshal := json.Unmarshal(response.Body, &status); errUnmarshal != nil {
		t.Fatalf("decode status: %v", errUnmarshal)
	}
	if status.Plugin != pluginDisplayName {
		t.Fatalf("status = %+v", status)
	}

	// 未知路径
	response = dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/qoder2api/nope"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route should 404, got %d", response.StatusCode)
	}
}

func TestManagementSettingsValidation(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	response := dispatchManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/qoder2api/settings",
		Body:   []byte(`{"auto_checkin_at":"99:99"}`),
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid clock should be rejected, got %d %s", response.StatusCode, response.Body)
	}

	response = dispatchManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/qoder2api/settings",
		Body:   []byte(`{"auto_checkin":true,"auto_checkin_at":"07:30","model_mapping":{"my-model":"performance"}}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings update failed: %d %s", response.StatusCode, response.Body)
	}
	enabled, at := effectiveCheckinSettings()
	if !enabled || at != "07:30" {
		t.Fatalf("settings not applied: %v %q", enabled, at)
	}
	if got := stateModelMapping()["my-model"]; got != "performance" {
		t.Fatalf("model mapping not stored: %v", stateModelMapping())
	}

	// 设置必须落盘：重载 state 后仍然存在。
	cfg := loadedConfig()
	if _, errState := loadState(cfg); errState != nil {
		t.Fatalf("reload state: %v", errState)
	}
	if got := stateModelMapping()["my-model"]; got != "performance" {
		t.Fatalf("model mapping lost after reload: %v", stateModelMapping())
	}
}

func TestManagementCheckinRouteUsesAuthFiles(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	windowStart := nowUnixMinusHour()
	setupCheckinUpstream(host, campaignFixture("CLAIMABLE", windowStart, 100),
		`{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`, 200)

	response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/plugins/qoder2api/checkin", Body: []byte(`{}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("checkin route failed: %d %s", response.StatusCode, response.Body)
	}
	var payload struct {
		OK      bool            `json:"ok"`
		Total   int             `json:"total"`
		Claimed int             `json:"claimed"`
		Results []checkinResult `json:"results"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode checkin response: %v", errUnmarshal)
	}
	if !payload.OK || payload.Total != 1 || payload.Claimed != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(string(response.Body), "dt-test-token") {
		t.Fatalf("checkin response leaked a credential: %s", response.Body)
	}
}

func TestManagementBatchQuotaRouteNormalizesQuota(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/api/v2/user/plan"):
			return fakeUpstreamResponse{Status: 200, Header: jsonHeader(), Body: `{"plan_tier_name":"Pro"}`}
		case strings.Contains(url, "/api/v2/quota/usage"):
			return fakeUpstreamResponse{Status: 200, Header: jsonHeader(), Body: `{"isQuotaExceeded":false,"expiresAt":1790000000,"userQuota":{"used":250,"total":1000,"remaining":750,"resetTime":"2026-01-01"},"addOnQuota":{"used":0,"total":500,"remaining":500}}`}
		default:
			return fakeUpstreamResponse{Status: 404, Header: jsonHeader(), Body: `{}`}
		}
	}

	response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/plugins/qoder2api/quotas", Body: []byte(`{}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("quota route failed: %d %s", response.StatusCode, response.Body)
	}
	var payload struct {
		Accounts []struct {
			AccountID string                        `json:"account_id"`
			Error     string                        `json:"error"`
			Quota     *pluginapi.QuotaFetchResponse `json:"quota"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode quota response: %v", errUnmarshal)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].Error != "" {
		t.Fatalf("payload = %+v", payload.Accounts)
	}
	quota := payload.Accounts[0].Quota
	if quota == nil || quota.Subscription == nil || quota.Subscription.Plan != "Pro" {
		t.Fatalf("subscription = %+v", quota)
	}
	if len(quota.Groups) != 2 {
		t.Fatalf("groups = %+v（套餐额度与拓展包应分成两组）", quota.Groups)
	}
	if fraction := quota.Groups[0].Buckets[0].RemainingFraction; fraction < 0.74 || fraction > 0.76 {
		t.Fatalf("remaining fraction = %v, want ~0.75", fraction)
	}
	if len(quota.Summary) == 0 {
		t.Fatal("summary metrics missing")
	}
}

func TestConsolePageIsStaticAndSelfContained(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)
	if !strings.Contains(page, managementRoutePrefix) {
		t.Fatal("page must know the management base path")
	}
	if strings.Contains(page, "pt-") || strings.Contains(page, "dt-") {
		t.Fatal("page must not embed credentials")
	}
	if !strings.Contains(page, "localStorage") {
		t.Fatal("page should reuse the management key from the browser when available")
	}
	// 关键安全提醒：鉴权失败必须停止自动刷新（CPA 会按 IP 封禁管理接口）。
	if !strings.Contains(page, "停止自动刷新") {
		t.Fatal("page must stop polling after an auth failure")
	}
}

func TestParseClockRejectsGarbageForSettings(t *testing.T) {
	if _, _, errClock := parseClock("not-a-time"); errClock == nil {
		t.Fatal("parseClock should reject garbage")
	}
}

// TestSettingsRejectsModelPrefixChange 确认插件不会单方面改注册期参数。
// 宿主只在自身配置变更时重读插件模型清单，插件私自改前缀会让 /v1/models 与插件状态不一致。
func TestSettingsRejectsModelPrefixChange(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	current := loadedConfig().ModelPrefix

	// 同值（幂等重放）允许通过，并在响应里回报当前前缀。
	sameBody, errMarshal := json.Marshal(map[string]any{"model_prefix": current})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	same := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/plugins/qoder2api/settings", Body: sameBody})
	if same.StatusCode != http.StatusOK {
		t.Fatalf("same prefix should be accepted: %d %s", same.StatusCode, same.Body)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(same.Body, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if payload["model_prefix"] != current {
		t.Fatalf("response should report the current prefix, got %v", payload["model_prefix"])
	}

	// 改值：必须拒绝，并指向宿主的插件配置接口。
	changeBody, errMarshalChange := json.Marshal(map[string]any{"model_prefix": "zz-"})
	if errMarshalChange != nil {
		t.Fatalf("marshal: %v", errMarshalChange)
	}
	changed := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/plugins/qoder2api/settings", Body: changeBody})
	if changed.StatusCode != http.StatusConflict {
		t.Fatalf("prefix change must be refused, got %d %s", changed.StatusCode, changed.Body)
	}
	if !strings.Contains(string(changed.Body), "/config") {
		t.Fatalf("refusal must point at the host plugin config endpoint: %s", changed.Body)
	}
	if loadedConfig().ModelPrefix != current {
		t.Fatalf("in-memory prefix must stay unchanged, got %q", loadedConfig().ModelPrefix)
	}
}

// TestStatusReportsRegisteredModelCount 确认状态里区分“已注册模型”与“已缓存上游模型”。
func TestStatusReportsRegisteredModelCount(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	resp := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/qoder2api/status"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status failed: %d", resp.StatusCode)
	}
	var payload struct {
		RegisteredModels int `json:"registered_models"`
		ModelCount       int `json:"model_count"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	want := len(buildModelCatalog(loadedConfig()))
	if payload.RegisteredModels != want {
		t.Fatalf("registered_models = %d, want %d", payload.RegisteredModels, want)
	}
	if payload.RegisteredModels == 0 {
		t.Fatal("bundled catalog must always offer something")
	}
}

// TestConsolePagePathsAreDeclared 锁死一类真实 bug：宿主只转发注册时声明过的路由，
// 页面调了未声明的路径就会静默 404（例如 /quota 被宿主的插件额度路由遮蔽）。
func TestConsolePagePathsAreDeclared(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)
	declared := map[string]struct{}{}
	for _, route := range managementRegistration().Routes {
		declared[strings.TrimPrefix(route.Path, managementRoutePrefix)] = struct{}{}
	}
	// 只需要声明一次路径：页面里 GET/POST 混用同一路径时按路径集合校验。
	pattern := regexp.MustCompile(`api\('([^'?]+)`)
	matches := pattern.FindAllStringSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("no management calls found in the console page")
	}
	for _, match := range matches {
		path := match[1]
		if _, ok := declared[path]; !ok {
			t.Errorf("console page calls %q but the plugin never declares it", path)
		}
	}
}

// TestHostReservedQuotaPathNotShadowed 确保批量额度接口不使用宿主占用的 /quota 路径。
func TestHostReservedQuotaPathNotShadowed(t *testing.T) {
	for _, route := range managementRegistration().Routes {
		if strings.TrimPrefix(route.Path, managementRoutePrefix) == "/quota" {
			t.Fatal("host owns /plugins/<id>/quota; the plugin must use another path")
		}
	}
	resp := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRoutePrefix + "/quotas"})
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("batch quota route should be handled, got %d %s", resp.StatusCode, resp.Body)
	}
}

// TestConsolePageOffersBothOAuthRegions 固化用户反馈的需求：
// OAuth 登录不能只有国际版入口，国内版（qoder.com.cn）也要能从界面点到。
// 同时锁死实现路径：链接与轮询必须走宿主接口（宿主 savePluginLoginRecords 负责落盘），
// 插件不许自己在别的路由上完成会话 —— 否则会话被消耗掉却没有账号落盘。
func TestConsolePageOffersBothOAuthRegions(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	for _, want := range []string{
		`id="login-global"`, `id="login-cn"`,
		`/qoder-auth-url?region=`, `/get-auth-status?state=`,
		`qoder.com`, `qoder.com.cn`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("登录入口缺少 %q", want)
		}
	}
	// 不允许自建登录会话接口：会话与落盘都归宿主。
	for _, forbidden := range []string{`/login/start`, `login/poll`, `auth.login.poll`} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("页面不应出现自建登录接口 %q", forbidden)
		}
	}
}

// TestLoginEntryUsesQueryMetadata 确认 region 是靠 query metadata 传给插件的
// （宿主 ServePluginAuthURL 会 queryValuesToMetadata），所以页面里两个按钮
// 必须显式带 region，不能只依赖插件配置里的 region。
func TestLoginEntryUsesQueryMetadata(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)
	if !strings.Contains(page, "startLogin('global'") || !strings.Contains(page, "startLogin('cn'") {
		t.Fatal("两个区域按钮必须各自显式传 region")
	}
	if !strings.Contains(page, "encodeURIComponent(region)") {
		t.Fatal("region 必须做 URL 编码后再拼 query")
	}
}
