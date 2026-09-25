package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/internal/logger"
)

// 本文件负责插件自己的落盘状态：
//   - 状态目录（机器指纹盐、state.json、日志）；
//   - 上游模型清单缓存；
//   - 签到记录与页面可改的设置覆盖（页面设置优先于 YAML）。
//
// 状态里不保存任何凭证：Qoder 凭证只存在于 CPA 的 auth 文件中。

// cachedModel 是缓存下来的上游模型条目（来自 /algo/api/v2/model/list）。
type cachedModel struct {
	Key             string `json:"key"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	IsReasoning     bool   `json:"is_reasoning,omitempty"`
	IsDefault       bool   `json:"is_default,omitempty"`
	Enable          bool   `json:"enable"`
}

// checkinRecord 是单账号签到记录（按 auth 文件 ID 索引）。
type checkinRecord struct {
	// Dates 是成功领取过的活动窗口日期（YYYY-MM-DD），用于计算连续天数。
	Dates []string `json:"dates,omitempty"`
	// LastDate 是最近一次成功签到的窗口日期（YYYY-MM-DD）。
	LastDate string `json:"last_date,omitempty"`
	// LastAmount 是最近一次成功领取的额度。
	LastAmount int `json:"last_amount,omitempty"`
	// Streak 是连续签到天数（本地统计，来自上游返回值优先）。
	Streak int `json:"streak,omitempty"`
	// TotalDays / TotalCredits 是本地累计统计。
	TotalDays    int `json:"total_days,omitempty"`
	TotalCredits int `json:"total_credits,omitempty"`
	// LastAttempt / LastStatus / LastMessage 记录最近一次尝试（含跳过与失败）。
	LastAttempt string `json:"last_attempt,omitempty"`
	LastStatus  string `json:"last_status,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// pluginState 是 state.json 的结构。
type pluginState struct {
	Version         int                      `json:"version"`
	Models          []cachedModel            `json:"models,omitempty"`
	ModelsFetchedAt time.Time                `json:"models_fetched_at,omitempty"`
	ModelsRegion    string                   `json:"models_region,omitempty"`
	Checkin         map[string]checkinRecord `json:"checkin,omitempty"`
	// ModelMapping 是页面维护的模型映射表（客户端模型名 → 上游 SKU key）。
	ModelMapping map[string]string `json:"model_mapping,omitempty"`
	// 页面可改设置（指针表示"已显式设置"）。
	AutoCheckin       *bool  `json:"auto_checkin,omitempty"`
	AutoCheckinAt     string `json:"auto_checkin_at,omitempty"`
	LastAutoCheckinOn string `json:"last_auto_checkin_on,omitempty"`
}

const stateVersion = 1

var (
	stateMu    sync.Mutex
	stateCache *pluginState
	stateDirty bool
)

func stateFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, stateFileName)
}

func saltFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, saltFileName)
}

// ensureStateDir 创建状态目录。
func ensureStateDir(cfg pluginConfig) error {
	dir := strings.TrimSpace(cfg.StateDir)
	if dir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return nil
}

// ensureMachineSalt 返回本机指纹盐：配置覆盖 > 已有盐文件 > 首次生成后落盘。
//
// 盐一旦生成必须保持不变：变更会让全部账号的派生机器码漂移（等价于换设备）。
func ensureMachineSalt(cfg pluginConfig) (string, error) {
	if override := strings.TrimSpace(cfg.MachineSalt); override != "" {
		return override, nil
	}
	path := saltFilePath(cfg)
	if raw, err := os.ReadFile(path); err == nil {
		if salt := strings.TrimSpace(string(raw)); salt != "" {
			return salt, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate machine salt: %w", err)
	}
	salt := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(salt+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write machine salt: %w", err)
	}
	logger.Info("machine salt generated at %s (do not change: it defines this deployment's device fingerprints)", path)
	return salt, nil
}

// loadState 从磁盘读取状态；文件缺失时返回空状态。
func loadState(cfg pluginConfig) (*pluginState, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	state := &pluginState{Version: stateVersion, Checkin: map[string]checkinRecord{}}
	raw, errRead := os.ReadFile(stateFilePath(cfg))
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			return state, fmt.Errorf("read state file: %w", errRead)
		}
		stateCache = state
		return state, nil
	}
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, state); errUnmarshal != nil {
			return state, fmt.Errorf("decode state file: %w", errUnmarshal)
		}
	}
	if state.Checkin == nil {
		state.Checkin = map[string]checkinRecord{}
	}
	if state.Version == 0 {
		state.Version = stateVersion
	}
	stateCache = state
	return state, nil
}

// snapshotState 返回状态副本（调用方可安全读取/序列化）。
func snapshotState() pluginState {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil {
		return pluginState{Version: stateVersion, Checkin: map[string]checkinRecord{}}
	}
	out := *stateCache
	out.Models = append([]cachedModel(nil), stateCache.Models...)
	out.ModelMapping = make(map[string]string, len(stateCache.ModelMapping))
	for key, value := range stateCache.ModelMapping {
		out.ModelMapping[key] = value
	}
	out.Checkin = make(map[string]checkinRecord, len(stateCache.Checkin))
	for key, value := range stateCache.Checkin {
		out.Checkin[key] = value
	}
	if stateCache.AutoCheckin != nil {
		enabled := *stateCache.AutoCheckin
		out.AutoCheckin = &enabled
	}
	return out
}

// mutateState 在锁内修改状态并立即落盘。
func mutateState(mutate func(*pluginState)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil {
		stateCache = &pluginState{Version: stateVersion, Checkin: map[string]checkinRecord{}}
	}
	mutate(stateCache)
	return saveStateLocked()
}

// saveStateLocked 原子写入状态文件。调用方必须持有 stateMu。
func saveStateLocked() error {
	if stateCache == nil {
		return nil
	}
	stateCache.Version = stateVersion
	raw, errMarshal := json.MarshalIndent(stateCache, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal state: %w", errMarshal)
	}
	path := stateFilePath(loadedConfig())
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write state: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state file: %w", errRename)
	}
	stateDirty = false
	return nil
}

// stateModelMapping 返回页面维护的模型映射表副本。
func stateModelMapping() map[string]string {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil || len(stateCache.ModelMapping) == 0 {
		return nil
	}
	out := make(map[string]string, len(stateCache.ModelMapping))
	for key, value := range stateCache.ModelMapping {
		out[key] = value
	}
	return out
}

// cachedModels 返回当前缓存的上游模型清单（可能为空）。
func cachedModels() []cachedModel {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil {
		return nil
	}
	return append([]cachedModel(nil), stateCache.Models...)
}

// storeCachedModels 写入上游模型清单缓存。
func storeCachedModels(models []cachedModel, region string) error {
	return mutateState(func(state *pluginState) {
		state.Models = models
		state.ModelsFetchedAt = time.Now()
		state.ModelsRegion = region
	})
}

// tokenPrefix 安全截取凭证前缀用于日志；绝不输出完整凭证。
func tokenPrefix(token string, n int) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) <= n {
		return trimmed[:len(trimmed)/2]
	}
	return trimmed[:n]
}
