package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/qoder"
)

// exportEntry 构造导出文件里的一条账号（测试用最小字段）。
func exportEntry(id, name, email, region, secret string) map[string]any {
	return map[string]any{
		"id":        id,
		"name":      name,
		"email":     email,
		"plan":      "Free",
		"region":    region,
		"auth_mode": "oauth",
		"api_mode":  "openai",
		"secret":    secret,
	}
}

// exportFile 构造 qoder2api 导出文件。
func exportFile(includeSecrets bool, entries ...map[string]any) map[string]any {
	accounts := make([]any, 0, len(entries))
	for _, entry := range entries {
		accounts = append(accounts, entry)
	}
	return map[string]any{
		"format":          accountExportFormat,
		"version":         1,
		"include_secrets": includeSecrets,
		"accounts":        accounts,
	}
}

// callAuthParse 走完整的 RPC 信封调用 auth.parse，返回解析结果。
func callAuthParse(t *testing.T, fileName string, payload map[string]any) (*pluginapi.AuthParseResponse, error) {
	t.Helper()
	rawJSON, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	request, errRequest := json.Marshal(map[string]any{
		"Provider": "",
		"FileName": fileName,
		"Path":     "/auths/" + fileName,
		"RawJSON":  base64.StdEncoding.EncodeToString(rawJSON),
	})
	if errRequest != nil {
		t.Fatalf("marshal request: %v", errRequest)
	}
	raw, errHandle := handleAuthParse(request)
	if errHandle != nil {
		return nil, errHandle
	}
	var env struct {
		OK     bool                        `json:"ok"`
		Result pluginapi.AuthParseResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (%s)", errUnmarshal, raw)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return &env.Result, nil
}

// TestAuthParseImportsAccountExport 锁死“把 qoder2api 导出文件丢进 auths/ 就能用”的能力。
//
// 一个文件展开成多个 CPA 账号（宿主侧会把这些标成插件虚拟账号），
// 每条账号自己声明的 region 必须优先于插件默认区域。
func TestAuthParseImportsAccountExport(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t, func(cfg *pluginConfig) { cfg.Region = qoder.RegionCN })

	oauthSecret := `{"device_token":"dt-oauth","refresh_token":"drt-oauth"}`
	result, errParse := callAuthParse(t, "qoder2api-accounts-20260924.json", exportFile(true,
		exportEntry("uid-tester-0001", "Test User", "tester@example.com", "global", oauthSecret),
		exportEntry("pat-account", "pat user", "pat@example.com", "cn", "pt-plaintext"),
	))
	if errParse != nil {
		t.Fatalf("handleAuthParse: %v", errParse)
	}
	if !result.Handled {
		t.Fatal("export file must be handled")
	}
	if len(result.Auths) != 2 {
		t.Fatalf("auths = %d, want 2 (one per exported account)", len(result.Auths))
	}

	oauth := result.Auths[0]
	if oauth.Provider != providerKey {
		t.Fatalf("provider = %q", oauth.Provider)
	}
	if oauth.ID != "uid-tester-0001" {
		t.Fatalf("id = %q, want the exported account id", oauth.ID)
	}
	if oauth.Label != "Test User" {
		t.Fatalf("label = %q, want the exported account name", oauth.Label)
	}
	if oauth.Attributes["region"] != "global" {
		t.Fatalf("attributes = %v（账号自带的 region 必须优先于插件默认 cn）", oauth.Attributes)
	}
	if oauth.Attributes["auth_mode"] != "oauth" {
		t.Fatalf("oauth account should be marked as oauth: %v", oauth.Attributes)
	}
	cred, errCred := parseQoderCredential(oauth.StorageJSON, qoder.RegionCN)
	if errCred != nil {
		t.Fatalf("imported storage must be usable: %v (%s)", errCred, oauth.StorageJSON)
	}
	if cred.Token != "dt-oauth" || cred.RefreshToken != "drt-oauth" {
		t.Fatalf("cred = %+v, want device+refresh token preserved", cred)
	}
	if cred.Region != qoder.RegionGlobal {
		t.Fatalf("region = %q, want global", cred.Region)
	}

	pat := result.Auths[1]
	if pat.ID != "pat-account" || pat.Attributes["region"] != "cn" {
		t.Fatalf("second account = %+v", pat)
	}
	patCred, errPatCred := parseQoderCredential(pat.StorageJSON, qoder.RegionGlobal)
	if errPatCred != nil {
		t.Fatalf("plaintext secret must import: %v", errPatCred)
	}
	if patCred.Token != "pt-plaintext" || patCred.RefreshToken != "" {
		t.Fatalf("pat cred = %+v", patCred)
	}
}

// TestAuthParseSkipsExportEntriesWithoutSecret 确认坏条目不会拖垮整份导出：
// 部分可用就导入可用的，全都不可用才报错（并说明是导出时没带凭证）。
func TestAuthParseSkipsExportEntriesWithoutSecret(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result, errParse := callAuthParse(t, "export-partial.json", exportFile(false,
		exportEntry("good", "good", "good@example.com", "global", `{"device_token":"dt-good","refresh_token":"drt-good"}`),
		exportEntry("bad", "bad", "bad@example.com", "global", ""),
		exportEntry("broken-json", "broken", "broken@example.com", "global", `{"device_token":`),
	))
	if errParse != nil {
		t.Fatalf("partial export should still import: %v", errParse)
	}
	if len(result.Auths) != 1 || result.Auths[0].ID != "good" {
		t.Fatalf("auths = %+v, want only the usable account", result.Auths)
	}

	_, errEmpty := callAuthParse(t, "export-nosecrets.json", exportFile(false,
		exportEntry("x", "x", "x@example.com", "global", ""),
	))
	if errEmpty == nil {
		t.Fatal("export without any usable secret must fail loudly")
	}
	pluginErr, ok := errEmpty.(*pluginError)
	if !ok || pluginErr.Code != "qoder_credential_missing" {
		t.Fatalf("error = %#v, want qoder_credential_missing", errEmpty)
	}
	if !strings.Contains(pluginErr.Message, "include_secrets") {
		t.Fatalf("error message should explain the cause: %q", pluginErr.Message)
	}
}

// TestAuthParseIgnoresForeignAccountsKey 确认不会因为文件里有 accounts 字段就抢别人的文件。
func TestAuthParseIgnoresForeignAccountsKey(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	for _, payload := range []map[string]any{
		{"accounts": []any{map[string]any{"id": "u1", "email": "u1@example.com"}}},
		{"format": "other-tool-accounts", "accounts": []any{map[string]any{"id": "u2", "token": "x"}}},
	} {
		result, errParse := callAuthParse(t, "accounts.json", payload)
		if errParse != nil {
			t.Fatalf("foreign file must not error: %v", errParse)
		}
		if result.Handled {
			t.Fatalf("foreign file must not be claimed: %+v", payload)
		}
	}
}

// TestExportStorageKeepsDisplayMetadata 确认导入时保留了展示用信息（不含凭证）。
func TestExportStorageKeepsDisplayMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result, errParse := callAuthParse(t, "export-meta.json", exportFile(true,
		exportEntry("g1", "Test User", "bin@example.com", "global", `{"device_token":"dt-1","refresh_token":"drt-1"}`),
	))
	if errParse != nil {
		t.Fatalf("handleAuthParse: %v", errParse)
	}
	var storage map[string]any
	if errUnmarshal := json.Unmarshal(result.Auths[0].StorageJSON, &storage); errUnmarshal != nil {
		t.Fatalf("storage must be JSON: %v", errUnmarshal)
	}
	if storage["type"] != providerKey {
		t.Fatalf("storage.type = %v, want %s", storage["type"], providerKey)
	}
	if storage["label"] != "Test User" || storage["email"] != "bin@example.com" {
		t.Fatalf("storage = %v", storage)
	}
	if storage["qoder_account_id"] != "g1" || storage["plan"] != "Free" {
		t.Fatalf("storage should keep the exported account id/plan: %v", storage)
	}
	if storage["region"] != "global" {
		t.Fatalf("storage.region = %v", storage["region"])
	}
}

// TestAuthParseDeclaresRefreshability 锁死与宿主刷新调度的契约：
// 宿主靠 auth.Metadata 里的 refresh_token 判断“这个凭证能不能刷新”，靠 NextRefreshAfter 排期。
// 缺了它们，/auth-files/refresh 与自动刷新会直接跳过账号（OAuth device token 就永远不会续期）。
func TestAuthParseDeclaresRefreshability(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result, errParse := callAuthParse(t, "export-refresh.json", exportFile(true,
		exportEntry("acc-1", "acc", "acc@example.com", "global", `{"device_token":"dt-1","refresh_token":"drt-1"}`),
	))
	if errParse != nil {
		t.Fatalf("handleAuthParse: %v", errParse)
	}
	auth := result.Auths[0]
	if auth.Metadata["refresh_token"] != "drt-1" {
		t.Fatalf("metadata must expose refresh_token to the host: %v", auth.Metadata)
	}
	if auth.Metadata["auth_mode"] != "oauth" {
		t.Fatalf("metadata = %v", auth.Metadata)
	}
	if auth.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter must be set so the host schedules the refresh")
	}

	// PAT 账号没有 refresh token：不应凭空造一个（宿主会去调不存在的刷新）。
	patResult, errPat := callAuthParse(t, "pat.json", map[string]any{"type": providerKey, "token": "pt-1"})
	if errPat != nil {
		t.Fatalf("handleAuthParse: %v", errPat)
	}
	if _, ok := patResult.Auth.Metadata["refresh_token"]; ok {
		t.Fatalf("pat auth must not claim a refresh token: %v", patResult.Auth.Metadata)
	}
	if patResult.Auth.Attributes["auth_mode"] != "" {
		t.Fatalf("pat auth_mode attribute = %q", patResult.Auth.Attributes["auth_mode"])
	}
}

// TestQuotaFetchResolvesCredentialByAuthIndex 锁死“宿主只发 AuthIndex、不发 StorageJSON”的路径。
//
// 宿主管理端额度路由（internal/api/handlers/management/plugin_quota.go）只发
// AuthIndex/AuthID + Metadata/Attributes，完全不发 StorageJSON；插件又不把 token 放进
// metadata/attributes。只认 StorageJSON 的解析会把额度查询误报成“凭证缺失”（实测 502）。
func TestQuotaFetchResolvesCredentialByAuthIndex(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	// 账号在文件里是 qoder2api 多账号导出形态，且没有提供 StorageJSON。
	host.mu.Lock()
	host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "acc-1", AuthIndex: "idx-1", Type: providerKey, Label: "acc"}}
	exportRaw, errMarshal := json.Marshal(exportFile(true, exportEntry("acc-1", "acc", "acc@example.com", "global", `{"device_token":"dt-1"}`)))
	if errMarshal != nil {
		t.Fatalf("marshal export: %v", errMarshal)
	}
	host.authJSON["idx-1"] = string(exportRaw)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/quota/usage") {
			return fakeUpstreamResponse{Status: 200, Body: `{"userType":"personal_standard","usageType":"credits","totalUsagePercentage":0.0,"isQuotaExceeded":false,"userQuota":{"total":100.0,"used":10.0,"remaining":90.0,"unit":"credits"}}`}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected"}`}
	}
	host.mu.Unlock()

	ctx := newTestContext()
	payload, errPayload := marshalJSON(map[string]any{
		"auth_index":       "idx-1",
		"auth_id":          "acc-1",
		"attributes":       map[string]string{"region": "global"},
		"host_callback_id": httpx.CallbackID(ctx),
	})
	if errPayload != nil {
		t.Fatalf("marshal request: %v", errPayload)
	}
	raw, errHandle := handleQuotaFetch(payload)
	if errHandle != nil {
		t.Fatalf("handleQuotaFetch without storage: %v", errHandle)
	}
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.QuotaFetchResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (%s)", errUnmarshal, raw)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	resp := env.Result
	if len(resp.Groups) == 0 || len(resp.Groups[0].Buckets) == 0 {
		t.Fatalf("expected quota groups: %+v", resp)
	}
	// 上游 total=100 / remaining=90 → 剩余比例 0.9
	if got := resp.Groups[0].Buckets[0].RemainingFraction; got < 0.89 || got > 0.91 {
		t.Fatalf("bucket remainingFraction = %v, want ~0.9", got)
	}
}
