package main

import (
	"encoding/json"
	"strings"
	"testing"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/qoder"
)

func TestParseQoderCredentialAcceptsCommonFieldNames(t *testing.T) {
	cases := []struct {
		name     string
		storage  string
		wantToke string
	}{
		{"token", `{"type":"qoder","token":"pt-1"}`, "pt-1"},
		{"access_token", `{"type":"qoder","access_token":"dt-2"}`, "dt-2"},
		{"api_key", `{"type":"qoder","api_key":"k-3"}`, "k-3"},
		{"personal_token", `{"type":"qoder","personal_token":"pt-4"}`, "pt-4"},
		{"pat", `{"type":"qoder","pat":"pt-5"}`, "pt-5"},
		{"qoder_token", `{"type":"qoder","qoder_token":"pt-6"}`, "pt-6"},
		{"device_token", `{"type":"qoder","device_token":"dt-7"}`, "dt-7"},
		// token 优先于 device_token（同一文件同时存在时以显式 token 为准）。
		{"token_wins", `{"token":"pt-8","device_token":"dt-9"}`, "pt-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred, errParse := parseQoderCredential([]byte(tc.storage), qoder.RegionGlobal)
			if errParse != nil {
				t.Fatalf("parseQoderCredential: %v", errParse)
			}
			if cred.Token != tc.wantToke {
				t.Fatalf("token = %q, want %q", cred.Token, tc.wantToke)
			}
		})
	}
}

func TestParseQoderCredentialRegionsAndMetadata(t *testing.T) {
	cred, errParse := parseQoderCredential([]byte(`{"type":"qoder","token":"pt-1","region":"CN","label":"主号","email":"a@b.c"}`), qoder.RegionGlobal)
	if errParse != nil {
		t.Fatalf("parseQoderCredential: %v", errParse)
	}
	if cred.Region != qoder.RegionCN {
		t.Errorf("region = %q, want cn", cred.Region)
	}
	if cred.Label != "主号" || cred.Email != "a@b.c" {
		t.Errorf("cred = %+v", cred)
	}

	cred, errParse = parseQoderCredential([]byte(`{"type":"qoder","token":"pt-1"}`), qoder.RegionCN)
	if errParse != nil {
		t.Fatalf("parseQoderCredential: %v", errParse)
	}
	if cred.Region != qoder.RegionCN {
		t.Errorf("fallback region = %q, want cn", cred.Region)
	}
}

// TestParseQoderCredentialAcceptsOAuthExportShape 锁死 qoder2api 导出格式的导入能力。
//
// 桌面端导出的是 {"secret":"{\"device_token\":\"dt-…\",\"refresh_token\":\"drt-…\"}"}：
// 之前只认扁平 token，导致 OAuth 账号根本无法导入；而且即使拿到 dt- 也会丢掉 refresh_token，
// 上游会话无法续期。
func TestParseQoderCredentialAcceptsOAuthExportShape(t *testing.T) {
	t.Run("flat device+refresh", func(t *testing.T) {
		cred, errParse := parseQoderCredential([]byte(`{"type":"qoder","device_token":"dt-abc","refresh_token":"drt-xyz","region":"global"}`), qoder.RegionCN)
		if errParse != nil {
			t.Fatalf("parseQoderCredential: %v", errParse)
		}
		if cred.Token != "dt-abc" || cred.RefreshToken != "drt-xyz" {
			t.Fatalf("cred = %+v", cred)
		}
		if cred.Region != qoder.RegionGlobal {
			t.Fatalf("region = %q, want global（账号自己声明的区域优先）", cred.Region)
		}
	})

	t.Run("nested secret json", func(t *testing.T) {
		storage := `{"type":"qoder","name":"Test User","region":"global","secret":"{\"device_token\":\"dt-abc\",\"refresh_token\":\"drt-xyz\"}"}`
		cred, errParse := parseQoderCredential([]byte(storage), qoder.RegionGlobal)
		if errParse != nil {
			t.Fatalf("parseQoderCredential: %v", errParse)
		}
		if cred.Token != "dt-abc" || cred.RefreshToken != "drt-xyz" {
			t.Fatalf("cred = %+v", cred)
		}
	})

	t.Run("nested plain secret", func(t *testing.T) {
		cred, errParse := parseQoderCredential([]byte(`{"secret":"dt-raw"}`), qoder.RegionGlobal)
		if errParse != nil {
			t.Fatalf("parseQoderCredential: %v", errParse)
		}
		if cred.Token != "dt-raw" || cred.RefreshToken != "" {
			t.Fatalf("cred = %+v", cred)
		}
	})

	t.Run("flat field wins over nested secret", func(t *testing.T) {
		storage := `{"token":"pt-flat","secret":"{\"device_token\":\"dt-nested\",\"refresh_token\":\"drt-nested\"}"}`
		cred, errParse := parseQoderCredential([]byte(storage), qoder.RegionGlobal)
		if errParse != nil {
			t.Fatalf("parseQoderCredential: %v", errParse)
		}
		if cred.Token != "pt-flat" {
			t.Fatalf("token = %q, want pt-flat", cred.Token)
		}
		// token 走扁平字段，但 refresh_token 仍要从 secret 里补上。
		if cred.RefreshToken != "drt-nested" {
			t.Fatalf("refresh token = %q, want drt-nested", cred.RefreshToken)
		}
	})
}

// TestBridgeSecretKeepsRefreshToken 锁死交给上游 NewBridge 的凭证形态：
// 有 refresh token 时必须传 JSON（上游 ParseOAuthSecret 会自己拆），否则会话无法续期。
func TestBridgeSecretKeepsRefreshToken(t *testing.T) {
	plain := bridgeSecret(qoderCredential{Token: "dt-only"})
	if plain != "dt-only" {
		t.Fatalf("without refresh token the raw credential must be passed through, got %q", plain)
	}

	withRefresh := bridgeSecret(qoderCredential{Token: "dt-abc", RefreshToken: "drt-xyz"})
	device, refresh := bridge.ParseOAuthSecret(withRefresh)
	if device != "dt-abc" || refresh != "drt-xyz" {
		t.Fatalf("upstream must parse both parts, got device=%q refresh=%q (raw=%s)", device, refresh, withRefresh)
	}
}

// TestCredentialFingerprintCoversRefreshToken 确保 refresh token 变化会让 Bridge 缓存失效。
func TestCredentialFingerprintCoversRefreshToken(t *testing.T) {
	base := qoderCredential{Token: "dt-abc", RefreshToken: "drt-1", Region: qoder.RegionGlobal}
	same := qoderCredential{Token: "dt-abc", RefreshToken: "drt-1", Region: qoder.RegionGlobal}
	changed := qoderCredential{Token: "dt-abc", RefreshToken: "drt-2", Region: qoder.RegionGlobal}

	if credentialFingerprint(base) != credentialFingerprint(same) {
		t.Fatal("identical credentials must share a fingerprint")
	}
	if credentialFingerprint(base) == credentialFingerprint(changed) {
		t.Fatal("refresh token change must invalidate the cached bridge")
	}
}

// TestParseQoderCredentialForAccountResolvesBundleEntry 锁死多账号导出文件的账号定位：
// 宿主文件级接口只能拿到整份导出文件，必须按账号 id / email / label 挑对那一条。
func TestParseQoderCredentialForAccountResolvesBundleEntry(t *testing.T) {
	bundle, errMarshal := json.Marshal(exportFile(true,
		exportEntry("acc-1", "first", "first@example.com", "global", `{"device_token":"dt-1","refresh_token":"drt-1"}`),
		exportEntry("acc-2", "second", "second@example.com", "cn", `{"device_token":"dt-2","refresh_token":"drt-2"}`),
	))
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}

	cases := []struct {
		name      string
		hint      string
		wantToken string
		wantMode  string
	}{
		{"by id", "acc-2", "dt-2", "drt-2"},
		{"by email", "first@example.com", "dt-1", "drt-1"},
		{"by label", "second", "dt-2", "drt-2"},
		{"unknown hint falls back to first usable", "nope", "dt-1", "drt-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred, errParse := parseQoderCredentialForAccount(bundle, qoder.RegionGlobal, tc.hint)
			if errParse != nil {
				t.Fatalf("parseQoderCredentialForAccount: %v", errParse)
			}
			if cred.Token != tc.wantToken || cred.RefreshToken != tc.wantMode {
				t.Fatalf("cred = %+v, want token=%s refresh=%s", cred, tc.wantToken, tc.wantMode)
			}
		})
	}
}

// TestParseQoderCredentialForAccountSkipsBrokenLeadingEntry 确认第一条坏时不会整体失败。
func TestParseQoderCredentialForAccountSkipsBrokenLeadingEntry(t *testing.T) {
	bundle, errMarshal := json.Marshal(exportFile(true,
		exportEntry("broken", "broken", "broken@example.com", "global", ""),
		exportEntry("good", "good", "good@example.com", "global", `{"device_token":"dt-good"}`),
	))
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	cred, errParse := parseQoderCredentialForAccount(bundle, qoder.RegionGlobal, "missing")
	if errParse != nil {
		t.Fatalf("should fall back to the usable entry: %v", errParse)
	}
	if cred.Token != "dt-good" {
		t.Fatalf("cred = %+v", cred)
	}
}

// TestParseQoderCredentialForAccountKeepsFlatErrors 确认普通文件的报错不被导出逻辑掩盖。
func TestParseQoderCredentialForAccountKeepsFlatErrors(t *testing.T) {
	if _, errParse := parseQoderCredentialForAccount([]byte(`{"type":"qoder"}`), qoder.RegionGlobal, "x"); errParse == nil {
		t.Fatal("plain file without credentials must still fail")
	}
}

// TestCredentialForAuthCachesHostLookups 确认管理页轮询不会把 host.auth.get 打满。
func TestCredentialForAuthCachesHostLookups(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	entry := pluginapi.HostAuthFileEntry{ID: "acc-1", AuthIndex: "idx-1", Type: providerKey, Label: "acc"}
	host.mu.Lock()
	host.authJSON["idx-1"] = `{"type":"qoder","device_token":"dt-cached","region":"global"}`
	host.mu.Unlock()

	for i := 0; i < 3; i++ {
		cred, errResolve := credentialForAuth(newTestContext(), "", entry)
		if errResolve != nil {
			t.Fatalf("credentialForAuth: %v", errResolve)
		}
		if cred.Token == "" {
			t.Fatal("credential token must resolve")
		}
	}

	gets := 0
	for _, method := range host.recordCalls() {
		if method == pluginabi.MethodHostAuthGet {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("host.auth.get called %d times, want 1 (cached)", gets)
	}
}

func TestParseQoderCredentialFailures(t *testing.T) {
	if _, errParse := parseQoderCredential(nil, qoder.RegionGlobal); errParse == nil {
		t.Error("empty storage must fail")
	}
	if _, errParse := parseQoderCredential([]byte("not-json"), qoder.RegionGlobal); errParse == nil {
		t.Error("non-JSON storage must fail")
	}
	if _, errParse := parseQoderCredential([]byte(`{"type":"qoder"}`), qoder.RegionGlobal); errParse == nil {
		t.Error("storage without token must fail")
	}
}

func TestLooksLikeQoderAuth(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		fileName string
		provider string
		want     bool
	}{
		{"type field", `{"type":"qoder","token":"pt-1"}`, "some.json", "", true},
		{"host provider", `{"token":"pt-1"}`, "weird.json", "qoder", true},
		{"file name prefix", `{"token":"pt-1"}`, "qoder-main.json", "", true},
		{"file name infix", `{"token":"pt-1"}`, "my-qoder-token.json", "", true},
		{"qoder_token key", `{"qoder_token":"pt-1"}`, "x.json", "", true},
		{"other provider", `{"type":"codex","access_token":"x"}`, "codex-a.json", "codex", false},
		{"unrelated file", `{"token":"x"}`, "unknown.json", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]interface{}
			if errUnmarshal := json.Unmarshal([]byte(tc.raw), &raw); errUnmarshal != nil {
				t.Fatalf("bad test fixture: %v", errUnmarshal)
			}
			if got := looksLikeQoderAuth(raw, tc.fileName, tc.provider); got != tc.want {
				t.Fatalf("looksLikeQoderAuth = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuthParseHandlesQoderFiles(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := handleAuthParse([]byte(`{"Provider":"qoder","FileName":"qoder-main.json","Path":"/auths/qoder-main.json","RawJSON":"eyJ0eXBlIjoicW9kZXIiLCJ0b2tlbiI6InB0LTEiLCJyZWdpb24iOiJjbiJ9"}`))
	if errHandle != nil {
		t.Fatalf("handleAuthParse: %v", errHandle)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool               `json:"Handled"`
			Auth    pluginapi.AuthData `json:"Auth"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.Result.Handled {
		t.Fatal("qoder auth file should be handled")
	}
	if env.Result.Auth.Provider != providerKey {
		t.Fatalf("provider = %q", env.Result.Auth.Provider)
	}
	if env.Result.Auth.ID != "qoder-main" {
		t.Fatalf("id = %q, want qoder-main（去掉 .json 后缀）", env.Result.Auth.ID)
	}
	if env.Result.Auth.Attributes["region"] != "cn" {
		t.Fatalf("attributes = %v", env.Result.Auth.Attributes)
	}
	if len(env.Result.Auth.StorageJSON) == 0 {
		t.Fatal("storage JSON must be preserved for the executor")
	}
}

func TestAuthParseIgnoresOtherProviders(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := handleAuthParse([]byte(`{"Provider":"codex","FileName":"codex-a.json","RawJSON":"eyJ0eXBlIjoiY29kZXgiLCJhY2Nlc3NfdG9rZW4iOiJ4In0="}`))
	if errHandle != nil {
		t.Fatalf("handleAuthParse: %v", errHandle)
	}
	var env struct {
		Result struct {
			Handled bool `json:"Handled"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.Result.Handled {
		t.Fatal("codex auth file must not be claimed by this plugin")
	}
}

func TestAuthRefreshValidatesAndSchedulesNextRefresh(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/user/jobToken"):
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token","refreshToken":"rt-new"}`}
		case strings.Contains(url, "/api/v1/userinfo"):
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester"}`}
		default:
			return fakeUpstreamResponse{Status: 404, Body: `{}`}
		}
	}

	raw, errHandle := handleAuthRefresh(authRefreshFixture("pt-test-token"))
	if errHandle != nil {
		t.Fatalf("handleAuthRefresh: %v", errHandle)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Auth             pluginapi.AuthData `json:"Auth"`
			NextRefreshAfter string             `json:"NextRefreshAfter"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("refresh failed: %s", raw)
	}
	if env.Result.Auth.Provider != providerKey {
		t.Fatalf("auth = %+v", env.Result.Auth)
	}
	// jobToken 交换返回的新 refreshToken 应写回存储，供下一轮刷新使用。
	if !strings.Contains(string(env.Result.Auth.StorageJSON), "rt-new") {
		t.Fatalf("refreshed storage should carry the new refresh token: %s", env.Result.Auth.StorageJSON)
	}
	if env.Result.NextRefreshAfter == "" {
		t.Fatal("next refresh time must be scheduled")
	}
}

func TestAuthRefreshSurfacesRejectedCredential(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		return fakeUpstreamResponse{Status: 401, Body: `{"message":"token expired"}`}
	}
	_, errHandle := handleAuthRefresh(authRefreshFixture("pt-dead-token"))
	if errHandle == nil {
		t.Fatal("rejected credential must surface as an error so CPA can mark it")
	}
	pluginErr, ok := errHandle.(*pluginError)
	if !ok {
		t.Fatalf("error type = %T", errHandle)
	}
	if pluginErr.HTTPStatus != 401 {
		t.Fatalf("status = %d, want 401", pluginErr.HTTPStatus)
	}
}

// TestAuthRefreshDefersOnTransientFailure 固化"不要把好账号标坏"：
// 上游抖动时保持凭证不变，只安排一次较早的重试。
func TestAuthRefreshDefersOnTransientFailure(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		return fakeUpstreamResponse{Status: 502, Body: `{"code":"provider_error","message":"upstream exploded"}`}
	}
	raw, errHandle := handleAuthRefresh(authRefreshFixture("pt-good-token"))
	if errHandle != nil {
		t.Fatalf("transient failure must not be reported as a credential problem: %v", errHandle)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Auth             pluginapi.AuthData `json:"Auth"`
			NextRefreshAfter string             `json:"NextRefreshAfter"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("refresh should degrade gracefully: %s", raw)
	}
	if env.Result.NextRefreshAfter == "" {
		t.Fatal("retry must be scheduled")
	}
	if !strings.Contains(string(env.Result.Auth.StorageJSON), "pt-good-token") {
		t.Fatalf("storage must be preserved on transient failure: %s", env.Result.Auth.StorageJSON)
	}
}

func TestListQoderAuthFilesFiltersOtherProviders(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.authFiles = []pluginapi.HostAuthFileEntry{
		{ID: "qoder-main", AuthIndex: "1", Type: "qoder"},
		{ID: "qoder-second", AuthIndex: "2", Provider: "qoder"},
		{ID: "codex-a", AuthIndex: "3", Type: "codex"},
		{ID: "runtime-only", AuthIndex: "4", Type: "qoder", RuntimeOnly: true},
	}
	accounts, errList := listQoderAuthFiles(newTestContext(), "")
	if errList != nil {
		t.Fatalf("listQoderAuthFiles: %v", errList)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want only the two qoder file credentials", accounts)
	}
}

func authRefreshFixture(token string) []byte {
	return []byte(`{"AuthID":"qoder-main","AuthProvider":"qoder","StorageJSON":"` + base64Encode(`{"type":"qoder","token":"`+token+`","region":"global"}`) + `"}`)
}

// TestJobTokenRefreshPreservesUserMaintainedFields 锁死“重写 auth 文件丢字段”的回归：
// 上游刷新 token 时必须基于原文件合并，保留 disabled / prefix / proxy_url / note / weight 等。
func TestJobTokenRefreshPreservesUserMaintainedFields(t *testing.T) {
	original := []byte(`{
  "type": "qoder",
  "token": "pt-old",
  "region": "cn",
  "label": "主账号",
  "email": "me@example.com",
  "disabled": true,
  "prefix": "work",
  "proxy_url": "socks5://127.0.0.1:1080",
  "note": "备用账号",
  "weight": 3
}`)
	cred := qoderCredential{Token: "pt-old", Region: qoderRegionCN(), Label: "主账号", Email: "me@example.com"}

	refreshed := jobTokenRefreshStorage(cred, original, map[string]interface{}{
		"refreshToken":       "rt-new",
		"securityOauthToken": "so-new",
	})
	if len(refreshed) == 0 {
		t.Fatal("refresh should produce updated storage")
	}
	var got map[string]interface{}
	if errUnmarshal := json.Unmarshal(refreshed, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal refreshed storage: %v", errUnmarshal)
	}

	// 上游下发的字段被写入。
	for key, want := range map[string]interface{}{
		"token":                "pt-old",
		"refresh_token":        "rt-new",
		"security_oauth_token": "so-new",
		"type":                 "qoder",
	} {
		if got[key] != want {
			t.Errorf("refreshed[%q] = %v, want %v", key, got[key], want)
		}
	}
	// 用户维护的字段必须原样保留。
	for key, want := range map[string]interface{}{
		"disabled":  true,
		"prefix":    "work",
		"proxy_url": "socks5://127.0.0.1:1080",
		"note":      "备用账号",
		"weight":    float64(3),
		"region":    "cn",
		"label":     "主账号",
		"email":     "me@example.com",
	} {
		if got[key] != want {
			t.Errorf("refreshed[%q] = %v, want %v (用户字段被丢失)", key, got[key], want)
		}
	}
}

// TestJobTokenRefreshWithoutOriginalStorageStillWorks 确认原文不可用时仍能产出可用凭证。
func TestJobTokenRefreshWithoutOriginalStorageStillWorks(t *testing.T) {
	cred := qoderCredential{Token: "pt-old", Region: qoderRegionCN()}
	refreshed := jobTokenRefreshStorage(cred, []byte("not-json"), map[string]interface{}{"securityOauthToken": "so-new"})
	var got map[string]interface{}
	if errUnmarshal := json.Unmarshal(refreshed, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if got["token"] != "pt-old" || got["security_oauth_token"] != "so-new" {
		t.Fatalf("unexpected storage: %v", got)
	}
}

// TestJobTokenRefreshSkipsWhenNoNewToken 确认上游没下发新 token 时不改写凭证。
func TestJobTokenRefreshSkipsWhenNoNewToken(t *testing.T) {
	cred := qoderCredential{Token: "pt-old"}
	if refreshed := jobTokenRefreshStorage(cred, []byte(`{"type":"qoder","token":"pt-old"}`), map[string]interface{}{}); refreshed != nil {
		t.Fatalf("no new token should not rewrite storage, got %s", refreshed)
	}
}

func qoderRegionCN() qoder.Region { return qoder.NormalizeRegion("cn") }

func base64Encode(value string) string {
	raw, _ := json.Marshal([]byte(value))
	return strings.Trim(string(raw), `"`)
}
