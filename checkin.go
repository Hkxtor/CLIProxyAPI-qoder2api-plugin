package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// 本文件实现「每日签到领 100 Credits」。
//
// 移植自 qoder2api/checkin.go，保留其结论性的抓包事实：
//   - 真实发放走 campaigns 流程：GET /sash/api/v1/me/campaigns → POST .../{id}/claim（空 body）；
//   - legacy 的 /daily-check-in/claim 已 DISABLED 且对未领取日也返回 409，只能读 status 做统计，
//     绝不能用它判断"是否已领取"；
//   - 请求只需要 device token + cosy-clienttype: 10（无需 cosy 签名）；
//   - 活动窗口是 10:00→次日 10:00，因此本地记录用「窗口开始日期」而不是自然日。
//
// 与 qoder2api 的差异：账号来自 CPA auth 文件（host.auth.list/get），
// 出站请求经宿主 HTTP 桥；签到历史并入插件 state.json。

const (
	checkinStatusClaimed        = "claimed"
	checkinStatusAlreadyClaimed = "already_claimed"
	checkinStatusNoCampaign     = "no_campaign"
	checkinStatusNoToken        = "no_token"
	checkinStatusError          = "error"

	checkinHTTPTimeout   = 60 * time.Second
	checkinMaxHistory    = 400
	checkinSchedulePolls = time.Minute
)

// checkinResult 是单个账号的签到结果（同时用于管理接口返回）。
type checkinResult struct {
	Account   string `json:"account"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
	Amount    int    `json:"amount,omitempty"`
	Message   string `json:"message"`

	StreakDays         int    `json:"streak_days,omitempty"`
	TotalClaimDays     int    `json:"total_claim_days,omitempty"`
	TotalRewardCredits int    `json:"total_reward_credits,omitempty"`
	RewardCredits      int    `json:"reward_credits,omitempty"`
	WindowDate         string `json:"window_date,omitempty"`
}

// campaignInfo 是活动列表条目。
type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// claimResponse 是领取响应。
type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
	ExpiresAt string `json:"expiresAt"`
}

// dailyCheckinStatus 是 legacy 接口的只读统计。
type dailyCheckinStatus struct {
	CampaignKey        string `json:"campaignKey"`
	Status             string `json:"status"`
	RewardCredits      int    `json:"rewardCredits"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// checkinHeaders 是桌面端签到所需的请求头（抓包确认）。
func checkinHeaders(bearer string) map[string]string {
	return map[string]string{
		"authorization":   "Bearer " + bearer,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
}

// checkinBase 返回该区域签到/活动接口的域名。
//
// 必须按账号区域选：国际版 device token 打到国内域名会得到 401 TOKEN_EXPIRE
// （上游 qoder2api 写死了国内域名，因此国际版账号签到必然失败）。
func checkinBase(region qoder.Region) string {
	return qoder.GetEndpoints(region).SashBase
}

// doCheckinRequest 发送签到相关请求，返回 (状态码, 解析后的 JSON, 原始正文)。
func doCheckinRequest(ctx context.Context, region qoder.Region, method, path, bearer string, reqBody interface{}) (int, interface{}, string) {
	base := checkinBase(region)
	url := base + path
	var body io.Reader
	if reqBody != nil {
		encoded, _ := json.Marshal(reqBody)
		body = bytes.NewReader(encoded)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, body)
	if errRequest != nil {
		return 0, nil, fmt.Sprintf("build request: %v", errRequest)
	}
	for key, value := range checkinHeaders(bearer) {
		req.Header.Set(key, value)
	}
	if method == http.MethodPost {
		req.Header.Set("origin", base)
		if reqBody == nil {
			req.ContentLength = 0 // 抓包确认 claim 无 body
		}
	}
	resp, errDo := httpx.Client(ctx, checkinHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Sprintf("do request: %v", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed interface{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, string(data)
}

// runCheckin 对指定账号（为空表示全部）执行签到，并把结果写入 state。
func runCheckin(ctx context.Context, callbackID string, accountIDs []string) ([]checkinResult, error) {
	accounts, errList := listQoderAuthFiles(ctx, callbackID)
	if errList != nil {
		return nil, newPluginError("auth_list_failed", "读取账号列表失败："+errList.Error(), http.StatusBadGateway)
	}
	if len(accounts) == 0 {
		return nil, newPluginError("no_account", "没有可用的 Qoder 账号（auths/qoder-*.json）", http.StatusBadRequest)
	}
	wanted := map[string]struct{}{}
	for _, id := range accountIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			wanted[trimmed] = struct{}{}
		}
	}

	results := make([]checkinResult, 0, len(accounts))
	for _, account := range accounts {
		if len(wanted) > 0 {
			if _, ok := wanted[account.ID]; !ok {
				continue
			}
		}
		cred, errCred := credentialForAuth(ctx, callbackID, account)
		if errCred != nil {
			results = append(results, recordCheckinOutcome(account.ID, checkinResult{
				Account:   accountLabel(account),
				AccountID: account.ID,
				Status:    checkinStatusError,
				Message:   "读取凭证失败：" + errCred.Error(),
			}))
			continue
		}
		result := checkinOneAccount(ctx, account, cred)
		results = append(results, recordCheckinOutcome(account.ID, result))
	}
	return results, nil
}

// checkinOneAccount 执行单账号签到（campaigns 权威流程 + 只读统计）。
func checkinOneAccount(ctx context.Context, account authFileEntry, cred qoderCredential) checkinResult {
	result := checkinResult{
		Account:   accountLabel(account),
		AccountID: account.ID,
		Status:    checkinStatusError,
	}
	bearer := checkinBearer(cred.Token)
	if bearer == "" {
		result.Status = checkinStatusNoToken
		result.Message = "凭证里没有可用 token"
		return result
	}

	// 权威领取：campaigns 流程
	result = campaignsCheckin(ctx, cred.Region, bearer, result)
	// 只读补充 legacy 统计（DISABLED 时恒为 0，不会覆盖本地统计）
	readDailyCheckinStats(ctx, cred.Region, bearer, &result)
	return result
}

// checkinBearer 取签到接口使用的 Bearer：OAuth JSON 凭据取 device_token，
// 其余（device token / PAT）原样使用——与 qoder2api 的行为一致。
func checkinBearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	deviceToken, _ := bridge.ParseOAuthSecret(trimmed)
	return firstNonEmpty(deviceToken, trimmed)
}

// campaignsCheckin 是真实发放积分的领取流程。
func campaignsCheckin(ctx context.Context, region qoder.Region, bearer string, result checkinResult) checkinResult {
	status, body, raw := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/campaigns", bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncateForMessage(raw, 300))
		return result
	}
	list, ok := body.(map[string]interface{})
	if !ok {
		result.Message = fmt.Sprintf("活动列表格式异常: %s", truncateForMessage(raw, 300))
		return result
	}
	campaignsRaw, _ := list["campaigns"].([]interface{})
	campaigns := make([]campaignInfo, 0, len(campaignsRaw))
	for _, item := range campaignsRaw {
		encoded, _ := json.Marshal(item)
		var parsed campaignInfo
		if json.Unmarshal(encoded, &parsed) == nil {
			campaigns = append(campaigns, parsed)
		}
	}

	var target *campaignInfo
	var benefitCampaign *campaignInfo
	alreadyClaimed := false
	for index := range campaigns {
		campaign := &campaigns[index]
		if campaign.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		benefitCampaign = campaign
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			target = campaign
		case "CLAIMED":
			alreadyClaimed = true
		}
	}

	// 窗口日期取活动 startAt：每日窗口 10:00 → 次日 10:00，
	// 用窗口日期可避免 10:00 前把昨日窗口误记到今天。
	windowSource := target
	if windowSource == nil {
		windowSource = benefitCampaign
	}
	if windowSource != nil && windowSource.StartAt > 0 {
		result.WindowDate = time.Unix(windowSource.StartAt, 0).Format("2006-01-02")
	}

	if target == nil {
		if alreadyClaimed {
			result.Status = checkinStatusAlreadyClaimed
			result.Message = windowHint(result, "今日已领取")
		} else {
			result.Status = checkinStatusNoCampaign
			if benefitCampaign == nil {
				// 实测：国际版（openapi.qoder.sh）根本没有每日签到计划
				// （/sash/api/v1/me/daily-check-in* 返回 404 NotFound），
				// 只有 VIEW_DETAILS 类促销活动。不像国内那样报“无可用活动”会让人反复查。
				if region == qoder.RegionGlobal {
					result.Message = "该区域无每日签到活动（国际版没有签到计划）"
				} else {
					result.Message = "无可用签到活动"
				}
			} else {
				result.Message = "无可用签到活动"
			}
		}
		return result
	}

	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest(ctx, region, http.MethodPost, claimPath, bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, truncateForMessage(raw, 300))
		return result
	}
	var claim claimResponse
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &claim) != nil {
		result.Message = fmt.Sprintf("领取响应格式异常: %s", truncateForMessage(raw, 300))
		return result
	}
	switch claim.Status {
	case "CLAIMED":
		if claim.Replayed {
			result.Status = checkinStatusAlreadyClaimed
			result.Message = windowHint(result, "今日已领取")
			return result
		}
		result.Status = checkinStatusClaimed
		if claim.Benefit != nil {
			result.Amount = claim.Benefit.Amount
		}
		if result.Amount == 0 {
			result.Amount = 100
		}
		result.Message = fmt.Sprintf("领取成功 +%d (%s)", result.Amount, target.CampaignKey)
		return result
	default:
		result.Message = fmt.Sprintf("未知状态: %s", claim.Status)
		return result
	}
}

// readDailyCheckinStats 只读 legacy 统计（绝不通过它领取）。
func readDailyCheckinStats(ctx context.Context, region qoder.Region, bearer string, result *checkinResult) {
	status, body, raw := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/daily-check-in/status", bearer, nil)
	if status != http.StatusOK {
		if status != http.StatusNotFound && status != http.StatusUnauthorized {
			logger.Info("[checkin] daily-check-in/status HTTP %d: %s", status, truncateForMessage(raw, 150))
		}
		return
	}
	var parsed dailyCheckinStatus
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &parsed) != nil || parsed.Status == "" {
		return
	}
	if parsed.CurrentStreakDays > result.StreakDays {
		result.StreakDays = parsed.CurrentStreakDays
	}
	if parsed.TotalClaimDays > result.TotalClaimDays {
		result.TotalClaimDays = parsed.TotalClaimDays
	}
	if parsed.TotalRewardCredits > result.TotalRewardCredits {
		result.TotalRewardCredits = parsed.TotalRewardCredits
	}
	if result.RewardCredits == 0 && parsed.RewardCredits > 0 {
		result.RewardCredits = parsed.RewardCredits
	}
}

// windowHint 生成更准确的"已领取"提示：10:00 前点击时说明新窗口开放时间。
func windowHint(result checkinResult, fallback string) string {
	if result.WindowDate == "" {
		return fallback
	}
	if result.WindowDate == todayString() {
		return fallback
	}
	windowDate, errParse := time.Parse("2006-01-02", result.WindowDate)
	if errParse != nil {
		return fallback
	}
	next := windowDate.AddDate(0, 0, 1).Format("01-02")
	return fmt.Sprintf("已领取 %s 窗口额度（新窗口 %s 10:00 开放）", result.WindowDate, next)
}

// recordCheckinOutcome 把结果写进 state（含本地连续天数统计）。
func recordCheckinOutcome(accountID string, result checkinResult) checkinResult {
	if errMutate := mutateState(func(state *pluginState) {
		if state.Checkin == nil {
			state.Checkin = map[string]checkinRecord{}
		}
		record := state.Checkin[accountID]
		attemptedAt := nowFunc().Format(time.RFC3339)
		record.LastAttempt = attemptedAt
		record.LastStatus = result.Status
		record.LastMessage = result.Message

		claimed := result.Status == checkinStatusClaimed || result.Status == checkinStatusAlreadyClaimed
		if claimed {
			windowDate := firstNonEmpty(result.WindowDate, todayString())
			amount := firstNonZero(result.Amount, result.RewardCredits, 100)
			if !containsString(record.Dates, windowDate) {
				record.Dates = append(record.Dates, windowDate)
				record.TotalDays++
				record.TotalCredits += amount
				record.LastDate = windowDate
				record.LastAmount = amount
			}
			if len(record.Dates) > checkinMaxHistory {
				record.Dates = record.Dates[len(record.Dates)-checkinMaxHistory:]
			}
			if result.Status == checkinStatusClaimed {
				result.Amount = amount
			}
		}
		streak, totalDays := computeStreak(record.Dates)
		if streak > record.Streak {
			record.Streak = streak
		}
		if totalDays > record.TotalDays {
			record.TotalDays = totalDays
		}
		state.Checkin[accountID] = record

		result.StreakDays = record.Streak
		result.TotalClaimDays = record.TotalDays
		result.TotalRewardCredits = record.TotalCredits
	}); errMutate != nil {
		logger.Error("persist checkin state failed: %v", errMutate)
	}
	if result.StreakDays > 0 || result.TotalClaimDays > 0 {
		result.Message = result.Message + streakSuffix(result)
	}
	return result
}

// streakSuffix 追加连续签到统计。
func streakSuffix(result checkinResult) string {
	if result.StreakDays <= 0 && result.TotalClaimDays <= 0 {
		return ""
	}
	return fmt.Sprintf("（连续 %d 天 · 累计 %d 天 · 共 %d 积分）",
		result.StreakDays, result.TotalClaimDays, result.TotalRewardCredits)
}

// computeStreak 从窗口日期列表计算连续天数与总天数。
func computeStreak(dates []string) (streak int, total int) {
	if len(dates) == 0 {
		return 0, 0
	}
	parsed := make([]time.Time, 0, len(dates))
	seen := map[string]struct{}{}
	for _, date := range dates {
		if _, exists := seen[date]; exists {
			continue
		}
		value, errParse := time.Parse("2006-01-02", date)
		if errParse != nil {
			continue
		}
		seen[date] = struct{}{}
		parsed = append(parsed, value)
	}
	if len(parsed) == 0 {
		return 0, 0
	}
	// 升序排序
	for i := 1; i < len(parsed); i++ {
		for j := i; j > 0 && parsed[j].Before(parsed[j-1]); j-- {
			parsed[j], parsed[j-1] = parsed[j-1], parsed[j]
		}
	}
	total = len(parsed)
	streak = 1
	for i := len(parsed) - 1; i > 0; i-- {
		diff := parsed[i].Sub(parsed[i-1])
		if diff == 24*time.Hour {
			streak++
			continue
		}
		break
	}
	return streak, total
}

// ---- 自动签到调度 ----

// effectiveCheckinSettings 返回生效的自动签到设置（页面设置优先于 YAML 配置）。
func effectiveCheckinSettings() (bool, string) {
	cfg := loadedConfig()
	enabled := cfg.AutoCheckin
	at := cfg.AutoCheckinAt
	state := snapshotState()
	if state.AutoCheckin != nil {
		enabled = *state.AutoCheckin
	}
	if strings.TrimSpace(state.AutoCheckinAt) != "" {
		at = state.AutoCheckinAt
	}
	if strings.TrimSpace(at) == "" {
		at = defaultCheckinAt
	}
	return enabled, at
}

// runCheckinScheduler 每天在设定时间执行一次全量签到。
//
// 设计取舍：以 1 分钟粒度轮询"是否到点且今天没跑过"，而不是精确 sleep 到目标时刻。
// 这样配置热更新、时钟调整、休眠唤醒都不会让调度停摆，代价是最多晚 1 分钟执行。
func runCheckinScheduler(ctx context.Context) {
	ticker := time.NewTicker(checkinSchedulePolls)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enabled, at := effectiveCheckinSettings()
			if !enabled || !autoCheckinDue(at) {
				continue
			}
			today := todayString()
			// 先记为已执行，避免签到过程较慢时被下一轮重复触发。
			if errMark := mutateState(func(state *pluginState) {
				state.LastAutoCheckinOn = today
			}); errMark != nil {
				logger.Error("mark auto checkin failed: %v", errMark)
				continue
			}
			results, errRun := runCheckin(ctx, "", nil)
			if errRun != nil {
				logger.Error("auto checkin failed: %v", errRun)
				continue
			}
			claimed := 0
			for _, result := range results {
				if result.Status == checkinStatusClaimed {
					claimed++
				}
			}
			logger.Info("auto checkin done: %d account(s), %d claimed", len(results), claimed)
		}
	}
}

// nowFunc 允许测试注入固定时间：签到判定与“今天”的记录都依赖它。
var nowFunc = time.Now

// todayString 返回当前本地日期（YYYY-MM-DD），用于签到窗口与幂等判定。
func todayString() string { return nowFunc().Format("2006-01-02") }

// autoCheckinDue 判断当前是否已到今天的签到时间且今天还没跑过。
func autoCheckinDue(at string) bool {
	hour, minute, errParse := parseClock(at)
	if errParse != nil {
		hour, minute = 10, 0
	}
	now := nowFunc()
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if now.Before(target) {
		return false
	}
	return snapshotState().LastAutoCheckinOn != todayString()
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func accountLabel(account authFileEntry) string {
	return firstNonEmpty(account.Label, account.Name, account.ID)
}
