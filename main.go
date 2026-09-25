package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
)

// pluginVersion 是插件版本，会在注册/状态接口里回给宿主。
//
// 故意写成 var：CI 用 `-ldflags -X main.pluginVersion=<tag>` 把发行版号注入产物，
// 本地直接 `./build.sh` 则保留 defaultPluginVersion。
var pluginVersion = defaultPluginVersion

// defaultPluginVersion 是未注入时的版本号。
const defaultPluginVersion = "0.1.5"

// effectivePluginVersion 返回对外上报的版本号。
//
// 防御 `-X main.pluginVersion=` 传成空值（或构建脚本变量为空）的情况：
// 此时宁可显示默认版本，也不要让管理端与注册信息里出现空版本号。
func effectivePluginVersion() string {
	if version := strings.TrimSpace(pluginVersion); version != "" {
		return version
	}
	return defaultPluginVersion
}

const (
	// pluginID 必须与动态库文件名一致（qoder2api.so → plugins.configs.qoder2api）。
	pluginID = "qoder2api"
	// pluginDisplayName 是管理端展示名。
	pluginDisplayName = "Qoder 2API"
	pluginAuthor      = "Zhengyuuuui"
	pluginRepository  = "https://github.com/Zhengyuuuui/qoder2api"

	// providerKey 是本插件在 CPA 里占用的 provider 键。
	// 它同时是三处的取值：模型注册 provider、执行器 identifier、auth provider identifier。
	// CPA 会跳过与原生 provider 重名的插件 provider，qoder 目前不与任何原生 provider 冲突。
	providerKey = "qoder"

	managementRoutePrefix = "/plugins/" + pluginID
	jsonContentType       = "application/json; charset=utf-8"
	htmlContentType       = "text/html; charset=utf-8"
)

// lifecycleRequest 是 plugin.register / plugin.reconfigure 的请求体。
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// registration 是 plugin.register / plugin.reconfigure 的响应体，
// 结构与宿主 internal/pluginhost/rpcRegistration 对齐。
type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

// registrationCapabilities 与宿主 rpcCapabilities 对齐（只声明本插件实现的能力）。
type registrationCapabilities struct {
	ModelProvider bool `json:"model_provider"`
	AuthProvider  bool `json:"auth_provider"`
	Executor      bool `json:"executor"`
	QuotaProvider bool `json:"quota_provider"`
	ManagementAPI bool `json:"management_api"`
	// ExecutorModelScope=both：静态模型与按账号模型都由本执行器承担。
	ExecutorModelScope string `json:"executor_model_scope"`
	// 输入/输出协议：插件直接复用 qoder2api 的三套原生 handler，
	// 因此声明这三种格式可以让宿主跳过翻译，保留原始报文保真度。
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          effectivePluginVersion(),
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "region", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"global", "cn"},
					Description: "Qoder 站点区域：global（国际，默认）或 cn（国内）。账号可自带 region 覆盖。"},
				{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString,
					Description: "插件状态目录（机器盐、模型缓存、签到记录、日志），默认 ~/.qoder2api-plugin。"},
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString,
					Description: "注册到 CPA 的模型 ID 前缀，默认 qoder-；用于避免与 CPA 原生 provider 的模型名冲突。"},
				{Name: "extra_models", Type: pluginapi.ConfigFieldTypeString,
					Description: "额外注册的模型 ID（不含前缀），逗号分隔。"},
				{Name: "log_level", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"debug", "info", "error"},
					Description: "插件日志级别，默认 info。"},
				{Name: "log_to_file", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否把日志写入 <state_dir>/logs，默认关闭。"},
				{Name: "auto_checkin", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否开启每日自动签到，默认关闭。"},
				{Name: "auto_checkin_at", Type: pluginapi.ConfigFieldTypeString,
					Description: "自动签到时间（本地时区 HH:MM），默认 10:00。"},
				{Name: "machine_salt", Type: pluginapi.ConfigFieldTypeString,
					Description: "设备指纹盐覆盖项；从 qoder2api 迁移且希望指纹不变时填原 settings.json 的 machine_salt。"},
				{Name: "model_mapping", Type: pluginapi.ConfigFieldTypeString,
					Description: "模型映射覆盖表，形如 sonnet=qmodel_38max,gpt=dmodel（逗号或空格分隔）。"},
			},
		},
		Capabilities: registrationCapabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			QuotaProvider:         true,
			ManagementAPI:         true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{formatChatCompletions, formatClaude, formatCodex},
			ExecutorOutputFormats: []string{formatChatCompletions, formatClaude, formatCodex},
		},
	}
}

// init 把宿主回调注入出站 HTTP 层。
// httpx 属于 internal 包，不能反向依赖 package main，因此在这里做一次性接线。
func init() {
	httpx.Configure(callHostScoped)
}

// handleMethod 是插件 ABI 的总入口。
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return handlePluginRegister(request)
	case pluginabi.MethodPluginReconfigure:
		return handlePluginReconfigure(request)
	case pluginabi.MethodPluginQuiesce:
		// 宿主要求停止接收新工作：停掉后台循环即可，在途请求由宿主等待。
		stopBackgroundWork()
		return okEnvelope(struct{}{})

	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return handleExecutorHTTPRequest(request)

	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodQuotaDescribe:
		return handleQuotaDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return handleQuotaReset(request)

	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// handlePluginRegister 首次注册：读取配置、初始化状态、启动后台循环。
func handlePluginRegister(request []byte) ([]byte, error) {
	pluginLifecycleMu.Lock()
	defer pluginLifecycleMu.Unlock()

	cfg, errConfig := decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.register: %w", errConfig)
	}
	if errApply := applyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.register: %w", errApply)
	}
	if _, errState := loadState(cfg); errState != nil {
		logger.Error("load state failed: %v", errState)
	}
	applyModelMappings(cfg)
	if !pluginRegistered {
		pluginRegistered = true
		startBackgroundWork()
	}
	logger.Info("registered: region=%s prefix=%q state_dir=%s", cfg.Region, cfg.ModelPrefix, cfg.StateDir)
	return okEnvelope(pluginRegistration())
}

// handlePluginReconfigure 配置热更新：不重新加载状态文件，避免覆盖内存中的设置。
func handlePluginReconfigure(request []byte) ([]byte, error) {
	pluginLifecycleMu.Lock()
	defer pluginLifecycleMu.Unlock()

	cfg, errConfig := decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errConfig)
	}
	previous := loadedConfig()
	if errApply := applyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errApply)
	}
	if previous.StateDir != cfg.StateDir {
		// 状态目录变了：重新加载，但不动内存中的设置覆盖。
		if _, errState := loadState(cfg); errState != nil {
			logger.Error("reload state failed: %v", errState)
		}
	}
	applyModelMappings(cfg)
	logger.Info("reconfigured: region=%s prefix=%q", cfg.Region, cfg.ModelPrefix)
	return okEnvelope(pluginRegistration())
}

func configYAMLFrom(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil
	}
	return req.ConfigYAML
}

// decodeStringRequest 用于只带字符串字段的请求（如 identifier 类调用）。
func decodeStringRequest(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		return fmt.Errorf("decode request: %w", errUnmarshal)
	}
	return nil
}

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: "GET", Path: managementRoutePrefix + "/status", Description: "账号、额度与签到状态。"},
			{Method: "POST", Path: managementRoutePrefix + "/checkin", Description: "对指定或全部账号执行签到。"},
			{Method: "POST", Path: managementRoutePrefix + "/settings", Description: "更新插件设置（自动签到、模型前缀）。"},
			{Method: "POST", Path: managementRoutePrefix + "/quotas", Description: "批量查询账号额度（单账号用宿主 /plugins/<id>/quota）。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/refresh", Description: "从上游拉取模型清单并缓存。"},
			{Method: "GET", Path: managementRoutePrefix + "/logs", Description: "读取插件日志。"},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/console",
				Menu:        pluginDisplayName,
				Description: "Qoder 账号、额度与签到管理页。",
			},
		},
	}
}

// mustString 返回去掉首尾空白的字符串。
func mustString(value string) string { return strings.TrimSpace(value) }
