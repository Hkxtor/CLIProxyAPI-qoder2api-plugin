package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/qoder"
)

// campaignFixture 生成活动列表响应。
func campaignFixture(claimStatus string, startAt int64, amount int64) string {
	return `{"campaigns":[{"campaignId":"camp-1","campaignKey":"cn_daily_check_in","actionType":"CLAIM_BENEFIT","claimStatus":"` + claimStatus + `","startAt":` + itoa(startAt) + `,"endAt":` + itoa(startAt+86400) + `,"benefit":{"kind":"CREDITS","amount":` + itoa(amount) + `}}]}`
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// setupCheckinUpstream 让假宿主像上游签到服务那样应答。
func setupCheckinUpstream(host *fakeHost, campaigns string, claimBody string, claimStatus int) *[]string {
	claimCalls := &[]string{}
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.HasSuffix(url, "/sash/api/v1/me/campaigns") && method == "GET":
			return fakeUpstreamResponse{Status: 200, Header: jsonHeader(), Body: campaigns}
		case strings.Contains(url, "/claim") && method == "POST":
			*claimCalls = append(*claimCalls, url)
			if claimStatus != 200 {
				return fakeUpstreamResponse{Status: claimStatus, Header: jsonHeader(), Body: claimBody}
			}
			return fakeUpstreamResponse{Status: 200, Header: jsonHeader(), Body: claimBody}
		case strings.Contains(url, "/daily-check-in/status"):
			return fakeUpstreamResponse{Status: 404, Header: jsonHeader(), Body: `{}`}
		default:
			return fakeUpstreamResponse{Status: 404, Header: jsonHeader(), Body: `{"error":"unexpected ` + url + `"}`}
		}
	}
	return claimCalls
}

func jsonHeader() map[string][]string {
	return map[string][]string{"Content-Type": {"application/json"}}
}

// nowUnixMinusHour 返回"一小时前"的时间戳（用于构造已开放的活动窗口）。
func nowUnixMinusHour() int64 {
	return time.Now().Add(-time.Hour).Unix()
}

func setupCheckinAccount(host *fakeHost, token string) {
	host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "qoder-main", AuthIndex: "idx-1", Name: "qoder-main.json", Type: providerKey, Label: "测试账号"}}
	host.authJSON["idx-1"] = `{"type":"qoder","token":"` + token + `","region":"global"}`
}

func TestCheckinClaimsCredits(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	windowStart := time.Now().Add(-2 * time.Hour).Unix()
	claimCalls := setupCheckinUpstream(host, campaignFixture("CLAIMABLE", windowStart, 100),
		`{"grantId":"g-1","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100},"expiresAt":"2026-12-31"}`, 200)

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	result := results[0]
	if result.Status != checkinStatusClaimed || result.Amount != 100 {
		t.Fatalf("result = %+v", result)
	}
	if len(*claimCalls) != 1 {
		t.Fatalf("claim called %d times, want 1", len(*claimCalls))
	}

	// 本地记录：窗口日期 + 累计额度。
	record := snapshotState().Checkin["qoder-main"]
	if len(record.Dates) != 1 || record.Dates[0] != time.Unix(windowStart, 0).Format("2006-01-02") {
		t.Fatalf("record dates = %v（应记录活动窗口日期）", record.Dates)
	}
	if record.TotalCredits != 100 || record.Streak != 1 {
		t.Fatalf("record = %+v", record)
	}
}

func TestCheckinAlreadyClaimedDoesNotCallClaim(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	claimCalls := setupCheckinUpstream(host, campaignFixture("CLAIMED", time.Now().Add(-time.Hour).Unix(), 100), `{}`, 200)

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if results[0].Status != checkinStatusAlreadyClaimed {
		t.Fatalf("status = %q", results[0].Status)
	}
	if len(*claimCalls) != 0 {
		t.Fatalf("已领取时不应再调用 claim（%v）", *claimCalls)
	}
}

// TestCheckinIsIdempotentAcrossRuns 固化幂等：上游返回 replayed 时不重复累计额度与天数。
func TestCheckinIsIdempotentAcrossRuns(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	windowStart := time.Now().Add(-time.Hour).Unix()

	setupCheckinUpstream(host, campaignFixture("CLAIMABLE", windowStart, 100),
		`{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`, 200)
	if _, errRun := runCheckin(newTestContext(), "", nil); errRun != nil {
		t.Fatalf("first run: %v", errRun)
	}

	// 第二次：上游已标记 CLAIMED（或返回 replayed），不能重复计数。
	setupCheckinUpstream(host, campaignFixture("CLAIMED", windowStart, 100), `{}`, 200)
	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("second run: %v", errRun)
	}
	if results[0].Status != checkinStatusAlreadyClaimed {
		t.Fatalf("status = %q", results[0].Status)
	}
	record := snapshotState().Checkin["qoder-main"]
	if len(record.Dates) != 1 || record.TotalCredits != 100 || record.TotalDays != 1 {
		t.Fatalf("record = %+v（重复运行不应重复计数）", record)
	}
}

func TestCheckinReplayIsTreatedAsAlreadyClaimed(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	setupCheckinUpstream(host, campaignFixture("CLAIMABLE", time.Now().Add(-time.Hour).Unix(), 100),
		`{"status":"CLAIMED","replayed":true,"benefit":{"kind":"CREDITS","amount":100}}`, 200)

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if results[0].Status != checkinStatusAlreadyClaimed {
		t.Fatalf("status = %q（replayed 说明这次没有真实发放）", results[0].Status)
	}
}

func TestCheckinNoCampaign(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	setupCheckinUpstream(host, `{"campaigns":[]}`, `{}`, 200)

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if results[0].Status != checkinStatusNoCampaign {
		t.Fatalf("status = %q", results[0].Status)
	}
}

func TestCheckinUpstreamFailureIsReportedNotSilentlySkipped(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupCheckinAccount(host, "dt-test-token")
	setupCheckinUpstream(host, `{"campaigns":[]}`, `{"message":"boom"}`, 500)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		return fakeUpstreamResponse{Status: 500, Header: jsonHeader(), Body: `{"message":"boom"}`}
	}

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if results[0].Status != checkinStatusError {
		t.Fatalf("status = %q", results[0].Status)
	}
	if !strings.Contains(results[0].Message, "500") {
		t.Fatalf("message should include the upstream status: %q", results[0].Message)
	}
}

func TestCheckinWithoutAccounts(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	if _, errRun := runCheckin(newTestContext(), "", nil); errRun == nil {
		t.Fatal("no accounts must produce a clear error")
	}
}

func TestComputeStreak(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	dayBefore := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	twoDaysAgo := time.Now().AddDate(0, 0, -3).Format("2006-01-02")

	cases := []struct {
		name       string
		dates      []string
		wantStreak int
		wantTotal  int
	}{
		{"empty", nil, 0, 0},
		{"single", []string{today}, 1, 1},
		{"consecutive", []string{dayBefore, yesterday, today}, 3, 3},
		{"gap breaks streak", []string{twoDaysAgo, yesterday, today}, 2, 3},
		{"duplicates counted once", []string{today, today, yesterday}, 2, 2},
		{"invalid ignored", []string{today, "not-a-date"}, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streak, total := computeStreak(tc.dates)
			if streak != tc.wantStreak || total != tc.wantTotal {
				t.Fatalf("computeStreak(%v) = (%d,%d), want (%d,%d)", tc.dates, streak, total, tc.wantStreak, tc.wantTotal)
			}
		})
	}
}

func TestEffectiveCheckinSettingsPrefersPageOverride(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t, func(cfg *pluginConfig) {
		cfg.AutoCheckin = false
		cfg.AutoCheckinAt = "10:00"
	})
	if enabled, at := effectiveCheckinSettings(); enabled || at != "10:00" {
		t.Fatalf("config baseline = %v %q", enabled, at)
	}
	if errMutate := mutateState(func(state *pluginState) {
		value := true
		state.AutoCheckin = &value
		state.AutoCheckinAt = "07:05"
	}); errMutate != nil {
		t.Fatalf("mutateState: %v", errMutate)
	}
	enabled, at := effectiveCheckinSettings()
	if !enabled || at != "07:05" {
		t.Fatalf("page override not applied: %v %q", enabled, at)
	}
}

func TestAutoCheckinDue(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	// 固定“现在”，让判定与真实时钟（尤其跨午夜）无关。
	withFixedNow(t, time.Date(2025, 3, 4, 12, 0, 0, 0, time.Local))

	// 未来时间：不到点
	if autoCheckinDue("14:00") {
		t.Fatal("future schedule must not be due")
	}
	// 过去时间且今天没跑过：到点
	if !autoCheckinDue("11:00") {
		t.Fatal("past schedule should be due")
	}
	// 今天已跑过：不再触发
	if errMutate := mutateState(func(state *pluginState) {
		state.LastAutoCheckinOn = "2025-03-04"
	}); errMutate != nil {
		t.Fatalf("mutateState: %v", errMutate)
	}
	if autoCheckinDue("11:00") {
		t.Fatal("already executed today must not run again")
	}
	// 跨午夜：昨天的 23:00 到点，在今天 00:30 不该补跑。
	withFixedNow(t, time.Date(2025, 3, 5, 0, 30, 0, 0, time.Local))
	if autoCheckinDue("23:00") {
		t.Fatal("previous-day schedule must not fire after midnight")
	}
}

// withFixedNow 把 nowFunc 固定到给定时刻，测试结束自动恢复。
func withFixedNow(t *testing.T, at time.Time) {
	t.Helper()
	previous := nowFunc
	nowFunc = func() time.Time { return at }
	t.Cleanup(func() { nowFunc = previous })
}

func TestCheckinBearerPrefersDeviceToken(t *testing.T) {
	if bearer := checkinBearer(`{"device_token":"dt-abc"}`); bearer != "dt-abc" {
		t.Fatalf("bearer = %q", bearer)
	}
	if bearer := checkinBearer("dt-direct"); bearer != "dt-direct" {
		t.Fatalf("bearer = %q", bearer)
	}
	// PAT 没有 device token：按 qoder2api 的行为原样使用（上游是否接受由上游决定）。
	if bearer := checkinBearer("pt-personal"); bearer != "pt-personal" {
		t.Fatalf("bearer = %q", bearer)
	}
}

// TestCheckinUsesRegionSpecificHost 锁死签到域名必须跟着账号区域走。
//
// 上游 qoder2api 写死了国内域名 https://openapi.qoder.com.cn：国际版账号的 device token
// 打到国内域名会被上游判成 401 TOKEN_EXPIRE（实测），签到必然失败。
func TestCheckinUsesRegionSpecificHost(t *testing.T) {
	cases := []struct {
		region   qoder.Region
		wantHost string
	}{
		{qoder.RegionGlobal, "https://openapi.qoder.sh"},
		{qoder.RegionCN, "https://openapi.qoder.com.cn"},
	}

	for _, tc := range cases {
		t.Run(string(tc.region), func(t *testing.T) {
			host := installFakeHost(t)
			setupTestPlugin(t)
			host.mu.Lock()
			host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "acc-" + string(tc.region), AuthIndex: "idx-" + string(tc.region), Type: providerKey, Label: "acc"}}
			host.authJSON["idx-"+string(tc.region)] = `{"type":"qoder","device_token":"dt-1","region":"` + string(tc.region) + `"}`
			host.mu.Unlock()

			results, errRun := runCheckin(newTestContext(), "", nil)
			if errRun != nil {
				t.Fatalf("runCheckin: %v", errRun)
			}
			if len(results) != 1 {
				t.Fatalf("results = %+v", results)
			}

			urls := host.requestURLs()
			if len(urls) == 0 {
				t.Fatal("no upstream request was made")
			}
			for _, url := range urls {
				if !strings.HasPrefix(url, tc.wantHost+"/sash/") {
					t.Fatalf("request %q must go to %s (region=%s)", url, tc.wantHost, tc.region)
				}
			}
		})
	}
}

// TestCheckinGlobalRegionExplainsMissingProgram 锁死国际版的签到提示。
//
// 实测 openapi.qoder.sh 上 /sash/api/v1/me/daily-check-in* 全是 404，
// 国际版没有每日签到计划（只有 VIEW_DETAILS 促销活动）。此时要明确告诉用户，
// 而不是笼统的“无可用签到活动”让人反复排查。
func TestCheckinGlobalRegionExplainsMissingProgram(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.mu.Lock()
	host.authFiles = []pluginapi.HostAuthFileEntry{{ID: "acc-global", AuthIndex: "idx-global", Type: providerKey, Label: "acc"}}
	host.authJSON["idx-global"] = `{"type":"qoder","device_token":"dt-1","region":"global"}`
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/me/campaigns") {
			// 只有促销活动，没有 CLAIM_BENEFIT 类签到活动。
			return fakeUpstreamResponse{Status: 200, Body: `{"claimable":true,"campaigns":[{"campaignId":"p1","campaignKey":"act-1","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"errorCode":"NotFound"}`}
	}
	host.mu.Unlock()

	results, errRun := runCheckin(newTestContext(), "", nil)
	if errRun != nil {
		t.Fatalf("runCheckin: %v", errRun)
	}
	if results[0].Status != checkinStatusNoCampaign {
		t.Fatalf("status = %q, want %q", results[0].Status, checkinStatusNoCampaign)
	}
	if !strings.Contains(results[0].Message, "国际版") {
		t.Fatalf("message should explain the region has no check-in program: %q", results[0].Message)
	}

	// 促销活动不能被误领（那是广告位，不是每日签到）。
	for _, url := range host.requestURLs() {
		if strings.Contains(url, "/claim") {
			t.Fatalf("promo campaign must not be claimed: %s", url)
		}
	}
}
