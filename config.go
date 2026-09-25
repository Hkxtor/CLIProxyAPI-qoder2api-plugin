package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/cosy"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// pluginConfig 是 plugins.configs.<pluginID> 解析出的有效配置。
//
// 与 CPA 其它插件一致，宿主传进来的是一段标准化 YAML（enabled/priority 由宿主追加），
// 这里只解析本插件声明的字段，未知字段忽略，保证宿主新增字段时不会让插件失效。
type pluginConfig struct {
	// Region 选择 Qoder 站点：cn（国内）或 global（国际，默认）。
	Region qoder.Region
	// StateDir 是插件状态目录（机器盐、模型缓存、签到记录、日志）。
	StateDir string
	// ModelPrefix 是注册到 CPA 的模型前缀，避免与 CPA 原生 provider 的模型 ID 冲突。
	ModelPrefix string
	// ExtraModels 是额外注册的模型 ID（不含前缀），用于手工暴露上游 SKU。
	ExtraModels []string
	// LogLevel 控制插件日志级别：debug / info / error。
	LogLevel string
	// LogToFile 为 true 时把日志写入 <StateDir>/logs。
	LogToFile bool
	// AutoCheckin 开启每日自动签到。
	AutoCheckin bool
	// AutoCheckinAt 是自动签到时间（本地时区 HH:MM），默认 10:00。
	AutoCheckinAt string
	// MachineSalt 允许覆盖自动生成的设备指纹盐（从 qoder2api 迁移时保持指纹一致）。
	MachineSalt string
	// ModelMappingRules 是"客户端模型名=上游 SKU key"形式的映射覆盖表。
	ModelMappingRules map[string]string
	// QueueWaitSeconds 强制单次排队等待时长（秒）；0（默认）表示按上游建议值等待。
	QueueWaitSeconds int
	// QueueMaxWaits 是一次请求内最多等待几次上游排队；0 表示完全不等待。
	QueueMaxWaits int
}

const (
	// defaultQueueWaitSeconds / defaultQueueMaxWaits：上游排队（免费模型常见）时的默认等待策略。
	// 等待时长默认跟随上游建议值（0 = 不强制），次数默认 2 次。
	defaultQueueWaitSeconds = 0
	defaultQueueMaxWaits    = 2
	defaultModelPrefix      = "qoder-"
	defaultCheckinAt        = "10:00"
	defaultLogLevel         = "info"
	stateDirEnvOverride     = "QODER2API_PLUGIN_HOME"
	stateFileName           = "state.json"
	saltFileName            = "machine_salt"
	defaultStateDirName     = ".qoder2api-plugin"
	configKeyEnabled        = "enabled"
	configKeyModelPrefix    = "model_prefix"
)

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Region:           qoder.RegionGlobal,
		StateDir:         defaultStateDir(),
		ModelPrefix:      defaultModelPrefix,
		LogLevel:         defaultLogLevel,
		AutoCheckin:      false,
		AutoCheckinAt:    defaultCheckinAt,
		QueueWaitSeconds: defaultQueueWaitSeconds,
		QueueMaxWaits:    defaultQueueMaxWaits,
	}
}

// defaultStateDir 推断默认状态目录：环境变量 > 用户主目录 > 进程临时目录。
func defaultStateDir() string {
	if env := strings.TrimSpace(os.Getenv(stateDirEnvOverride)); env != "" {
		return env
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, defaultStateDirName)
	}
	return filepath.Join(os.TempDir(), defaultStateDirName)
}

var currentConfig atomic.Pointer[pluginConfig]

func loadedConfig() pluginConfig {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func storeConfig(cfg pluginConfig) { currentConfig.Store(&cfg) }

// decodeConfig 解析宿主传入的配置 YAML。
// 支持 `key: value` 标量行、# 注释、单双引号字符串；嵌套块暂不需要。
func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	for lineNo, line := range strings.Split(string(raw), "\n") {
		key, value, ok := splitConfigLine(line)
		if !ok {
			continue
		}
		switch key {
		case "region":
			region := qoder.NormalizeRegion(value)
			if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" &&
				trimmed != "cn" && trimmed != "global" {
				return cfg, fmt.Errorf("config line %d: unsupported region %q (want cn or global)", lineNo+1, value)
			}
			cfg.Region = region
		case "state_dir":
			if strings.TrimSpace(value) != "" {
				cfg.StateDir = strings.TrimSpace(value)
			}
		case configKeyModelPrefix:
			cfg.ModelPrefix = strings.TrimSpace(value)
		case "extra_models":
			cfg.ExtraModels = splitList(value)
		case "log_level":
			if strings.TrimSpace(value) != "" {
				cfg.LogLevel = strings.ToLower(strings.TrimSpace(value))
			}
		case "log_to_file":
			cfg.LogToFile = parseBool(value, cfg.LogToFile)
		case "auto_checkin":
			cfg.AutoCheckin = parseBool(value, cfg.AutoCheckin)
		case "auto_checkin_at":
			if strings.TrimSpace(value) != "" {
				cfg.AutoCheckinAt = strings.TrimSpace(value)
			}
		case "machine_salt":
			cfg.MachineSalt = strings.TrimSpace(value)
		case "queue_wait_seconds":
			seconds, errSeconds := parseQueueCount(value, "queue_wait_seconds")
			if errSeconds != nil {
				return cfg, fmt.Errorf("config line %d: %w", lineNo+1, errSeconds)
			}
			cfg.QueueWaitSeconds = seconds
		case "queue_max_waits":
			waits, errWaits := parseQueueCount(value, "queue_max_waits")
			if errWaits != nil {
				return cfg, fmt.Errorf("config line %d: %w", lineNo+1, errWaits)
			}
			cfg.QueueMaxWaits = waits
		case "model_mapping":
			rules, errRules := parseMappingRules(value)
			if errRules != nil {
				return cfg, fmt.Errorf("config line %d: %w", lineNo+1, errRules)
			}
			cfg.ModelMappingRules = rules
		case configKeyEnabled:
			// 宿主追加的插件启用开关：由宿主负责，这里只确认字段存在。
		}
	}
	if err := validateConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func validateConfig(cfg pluginConfig) error {
	if _, _, err := parseClock(cfg.AutoCheckinAt); err != nil {
		return fmt.Errorf("auto_checkin_at: %w", err)
	}
	switch cfg.LogLevel {
	case "debug", "info", "error":
	default:
		return fmt.Errorf("log_level %q is not supported (want debug, info or error)", cfg.LogLevel)
	}
	if cfg.QueueWaitSeconds < 0 {
		return fmt.Errorf("queue_wait_seconds must be >= 0 (got %d)", cfg.QueueWaitSeconds)
	}
	if cfg.QueueMaxWaits < 0 {
		return fmt.Errorf("queue_max_waits must be >= 0 (got %d)", cfg.QueueMaxWaits)
	}
	return nil
}

// parseQueueCount 解析排队策略里的非负整数（秒数/次数）。空值表示回到默认。
func parseQueueCount(value, key string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		if key == "queue_wait_seconds" {
			return defaultQueueWaitSeconds, nil
		}
		return defaultQueueMaxWaits, nil
	}
	parsed, errParse := strconv.Atoi(trimmed)
	if errParse != nil {
		return 0, fmt.Errorf("%s %q is not a number", key, value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be >= 0 (got %d)", key, parsed)
	}
	return parsed, nil
}

// splitConfigLine 拆出 `key: value`；跳过空行、注释与非标量行。
func splitConfigLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimRight(line, "\r")
	trimmed = strings.TrimLeft(trimmed, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = strings.TrimSpace(trimmed[idx+1:])
	// 去掉行尾注释（简单处理：不在引号内的 " #"）
	if !strings.HasPrefix(value, "\"") && !strings.HasPrefix(value, "'") {
		if hash := strings.Index(value, " #"); hash >= 0 {
			value = strings.TrimSpace(value[:hash])
		}
	}
	value = strings.Trim(value, `"'`)
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

func parseBool(value string, fallback bool) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

// parseMappingRules 解析 "name=sku,name2=sku2" 形式的模型映射表。
func parseMappingRules(value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	rules := map[string]string{}
	for _, item := range splitList(strings.ReplaceAll(value, ",", " ")) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid model_mapping entry %q (want name=sku)", item)
		}
		name := strings.TrimSpace(parts[0])
		sku := strings.TrimSpace(parts[1])
		if name == "" || sku == "" {
			return nil, fmt.Errorf("invalid model_mapping entry %q (want name=sku)", item)
		}
		rules[name] = sku
	}
	return rules, nil
}

// splitList 解析逗号/空格分隔的列表值。
func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		item := strings.TrimSpace(field)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// parseClock 解析 HH:MM（本地时区）。
func parseClock(value string) (hour, minute int, err error) {
	trimmed := strings.TrimSpace(value)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a valid HH:MM time", value)
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a valid HH:MM time", value)
	}
	return hour, minute, nil
}

// checkinTime 返回当天的自动签到时刻。
func checkinTime(cfg pluginConfig) time.Time {
	hour, minute, err := parseClock(cfg.AutoCheckinAt)
	if err != nil {
		hour, minute = 10, 0
	}
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
}

// applyConfig 应用配置：更新全局配置、初始化日志与状态目录、设置指纹盐。
func applyConfig(cfg pluginConfig) error {
	if err := ensureStateDir(cfg); err != nil {
		return err
	}
	logger.SetLevel(cfg.LogLevel)
	if err := logger.InitFile(cfg.LogToFile, filepath.Join(cfg.StateDir, "logs")); err != nil {
		logger.Error("init log file sink failed: %v", err)
	}
	salt, err := ensureMachineSalt(cfg)
	if err != nil {
		return err
	}
	cosy.SetInstallSalt(salt)
	// 排队策略是运行时策略（宿主热重载插件配置时会重新应用）：
	// 免费模型被上游要求排队时，插件按这里的策略等待/重试或直接失败。
	bridge.SetQueuePolicy(cfg.QueueMaxWaits, cfg.QueueWaitSeconds)
	storeConfig(cfg)
	return nil
}
