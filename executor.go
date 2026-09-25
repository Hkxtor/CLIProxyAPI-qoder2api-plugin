package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/httpx"
	"qoder2api-plugin/internal/logger"
	"qoder2api-plugin/internal/qoder"
)

// executorRPCRequest 与宿主 internal/pluginhost/rpcExecutorRequest 对齐。
// 注意：内嵌字段按 Go 字段名（无 json tag）传输，stream_id / host_callback_id 用小写键。
type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorHTTPRPCRequest 与宿主 rpcExecutorHTTPRequest 对齐。
type executorHTTPRPCRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// qoderCredential 是从 CPA auth 文件解析出的 Qoder 凭证。
//
// qoder2api 有三种凭证形态，这里都要认：
//   - PAT 明文（pt-…）：走 jobToken 交换；
//   - OAuth device token（dt-…）：直接调 userinfo；
//   - OAuth 导出：`{"device_token":"dt-…","refresh_token":"drt-…"}`（可能整段嵌在 `secret` 字段里）。
//
// RefreshToken 不能丢：上游会话会把 refresh_token 写进 payload，缺了它会话就无法续期。
type qoderCredential struct {
	Token        string
	RefreshToken string
	Region       qoder.Region
	Label        string
	Email        string
}

const (
	// bridgeCacheTTL 决定 Bridge（含 cosy 会话）最长复用时间。
	// 上游一旦改签名规则，最长这么多时间后会自动重建。
	bridgeCacheTTL = 30 * time.Minute
	// bridgeCreateTimeout 限制单次 Bridge 建连时间（含 jobToken 交换）。
	bridgeCreateTimeout = 30 * time.Second
	executorHTTPLimit   = int64(8 << 20)
)

// credentialTokenKeys 是可直接作为凭证的字段名（按优先级）。
// device_token 放在 token 之后：qoder2api 桌面端导出用前者，手写 auth 文件习惯用后者。
var credentialTokenKeys = []string{"token", "device_token", "access_token", "api_key", "personal_token", "pat", "qoder_token"}

// parseQoderCredential 从 auth 文件 JSON 解析凭证。
// 兼容多种字段名，避免用户手写 auth 文件时因为字段名不同而失败。
func parseQoderCredential(storageJSON []byte, fallbackRegion qoder.Region) (qoderCredential, error) {
	cred := qoderCredential{Region: fallbackRegion}
	if len(storageJSON) == 0 {
		return cred, fmt.Errorf("auth storage is empty: 该凭证没有 Qoder token（检查 auths/qoder-*.json 的 token 或 device_token 字段）")
	}
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(storageJSON, &raw); errUnmarshal != nil {
		return cred, fmt.Errorf("auth storage is not valid JSON: %w", errUnmarshal)
	}
	for _, key := range credentialTokenKeys {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			cred.Token = strings.TrimSpace(value)
			break
		}
	}
	if refresh, ok := raw["refresh_token"].(string); ok {
		cred.RefreshToken = strings.TrimSpace(refresh)
	}
	// qoder2api 导出格式把整对凭证放在 secret 字段的 JSON 字符串里；
	// 直接导入这类文件（或原样拷贝 export 条目）时必须能解出 dt- 与 drt-。
	if nested, ok := raw["secret"].(string); ok && strings.TrimSpace(nested) != "" {
		inner := map[string]interface{}{}
		if errInner := json.Unmarshal([]byte(strings.TrimSpace(nested)), &inner); errInner == nil {
			if cred.Token == "" {
				for _, key := range credentialTokenKeys {
					if value, okInner := inner[key].(string); okInner && strings.TrimSpace(value) != "" {
						cred.Token = strings.TrimSpace(value)
						break
					}
				}
			}
			if cred.RefreshToken == "" {
				if value, okInner := inner["refresh_token"].(string); okInner {
					cred.RefreshToken = strings.TrimSpace(value)
				}
			}
		} else if cred.Token == "" {
			// 非 JSON 的 secret 视作直接是凭证（与上游 ParseOAuthSecret 一致）。
			cred.Token = strings.TrimSpace(nested)
		}
	}
	if cred.Token == "" {
		return cred, fmt.Errorf("auth storage has no token field (supported: %s) and no secret blob", strings.Join(credentialTokenKeys, ", "))
	}
	if region, ok := raw["region"].(string); ok && strings.TrimSpace(region) != "" {
		cred.Region = qoder.NormalizeRegion(region)
	}
	if label, ok := raw["label"].(string); ok {
		cred.Label = strings.TrimSpace(label)
	}
	if email, ok := raw["email"].(string); ok {
		cred.Email = strings.TrimSpace(email)
	}
	return cred, nil
}

// bridgeSecret 返回要交给 bridge.NewBridge 的凭证串。
//
// 上游 NewBridge 内部会自己调 ParseOAuthSecret：
// 传 dt- 明文会丢掉 refresh_token（会话无法续期），所以有 refresh token 时
// 必须按 qoder2api 导出的 JSON 形态传，与上游桌面端行为保持一致。
func bridgeSecret(cred qoderCredential) string {
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// stripModelPrefix 去掉注册时加的模型前缀，得到 bridge 认识的名字。
func stripModelPrefix(prefix, model string) string {
	name := strings.TrimSpace(model)
	if prefix == "" {
		return name
	}
	return strings.TrimPrefix(name, prefix)
}

// ---- Bridge 缓存 ----
//
// 建 Bridge 需要与上游交互（userinfo 或 jobToken 交换），因此按账号缓存复用。
// 缓存键包含凭证指纹：宿主刷新 auth 文件（token 变化）后旧 Bridge 立即失效。

type bridgeCacheEntry struct {
	created   time.Time
	tokenHash string
	bridge    *bridge.Bridge
}

var (
	bridgeCacheMu sync.Mutex
	bridgeCache   = map[string]*bridgeCacheEntry{}
	bridgeCreates = map[string]*bridgeCreateCall{}
)

type bridgeCreateCall struct {
	wg  sync.WaitGroup
	b   *bridge.Bridge
	err error
}

func credentialFingerprint(cred qoderCredential) string {
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:])
}

// bridgeFor 返回该账号可用的 Bridge（缓存命中即复用，同账号并发请求只建一次）。
func bridgeFor(ctx context.Context, authID string, cred qoderCredential) (*bridge.Bridge, error) {
	key := strings.TrimSpace(authID)
	if key == "" {
		key = credentialFingerprint(cred)
	}
	fingerprint := credentialFingerprint(cred)

	bridgeCacheMu.Lock()
	if entry := bridgeCache[key]; entry != nil {
		if entry.tokenHash == fingerprint && time.Since(entry.created) < bridgeCacheTTL {
			cached := entry.bridge
			bridgeCacheMu.Unlock()
			return cached, nil
		}
		delete(bridgeCache, key)
	}
	if inFlight := bridgeCreates[key]; inFlight != nil {
		bridgeCacheMu.Unlock()
		inFlight.wg.Wait()
		return inFlight.b, inFlight.err
	}
	call := &bridgeCreateCall{}
	call.wg.Add(1)
	bridgeCreates[key] = call
	bridgeCacheMu.Unlock()

	call.b, call.err = createBridge(ctx, cred)
	if call.err == nil {
		bridgeCacheMu.Lock()
		bridgeCache[key] = &bridgeCacheEntry{created: time.Now(), tokenHash: fingerprint, bridge: call.b}
		bridgeCacheMu.Unlock()
	}
	call.wg.Done()

	bridgeCacheMu.Lock()
	delete(bridgeCreates, key)
	bridgeCacheMu.Unlock()

	return call.b, call.err
}

// invalidateBridge 丢弃某个账号的缓存（凭证失效或上游返回 401/403 时调用）。
func invalidateBridge(authID string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	bridgeCacheMu.Lock()
	delete(bridgeCache, authID)
	bridgeCacheMu.Unlock()
}

func createBridge(ctx context.Context, cred qoderCredential) (*bridge.Bridge, error) {
	createCtx, cancel := context.WithTimeout(ctx, bridgeCreateTimeout)
	defer cancel()

	templateBase, errTemplate := buildTemplateBase()
	if errTemplate != nil {
		return nil, errTemplate
	}
	b, errNew := bridge.NewBridge(createCtx, bridgeSecret(cred), cred.Region, templateBase)
	if errNew != nil {
		classified := classifyCredentialError(errNew)
		classified.Message = "建立 Qoder 会话失败：" + classified.Message
		return nil, classified
	}
	return b, nil
}

// ---- 执行器入口 ----

// preparedExecution 汇集同步校验结果。
type preparedExecution struct {
	rpc    executorRPCRequest
	cred   qoderCredential
	format string
	// outputFormat 是宿主期望的响应协议（rpc.format）；缺省回落到请求协议。
	// 它决定流分片的分帧方式，见 newStreamWriter 的说明。
	outputFormatValue string
	handler           bridgeHandler
}

// outputFormat 返回该请求的输出协议格式。
func (p preparedExecution) outputFormat() string {
	if p.outputFormatValue != "" {
		return p.outputFormatValue
	}
	return p.format
}

func handleExecutorExecute(request []byte) ([]byte, error) {
	prepared, errPrepare := prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	ctx := httpx.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID)

	b, errBridge := bridgeFor(ctx, prepared.rpc.AuthID, prepared.cred)
	if errBridge != nil {
		return nil, errBridge
	}
	model := stripModelPrefix(loadedConfig().ModelPrefix, prepared.rpc.Model)
	req, errRequest := newBridgeRequest(ctx, prepared.rpc.Payload, prepared.rpc.Headers, model)
	if errRequest != nil {
		return nil, errRequest
	}
	writer := newBufferedWriter()
	runBridgeHandler(prepared.rpc.AuthID, prepared.handler, b, writer, req)

	if writer.status >= http.StatusBadRequest {
		return nil, responseError(writer.status, writer.body.Bytes())
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: writer.body.Bytes(),
		Headers: responseHeaders(writer.header),
	})
}

func handleExecutorExecuteStream(request []byte) ([]byte, error) {
	prepared, errPrepare := prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if strings.TrimSpace(prepared.rpc.StreamID) == "" {
		return nil, newPluginError("invalid_request", "stream_id is required for streaming execution", http.StatusBadRequest)
	}

	// 头部必须同步返回：宿主用它们构造下游响应。三种 handler 的流式输出都是 SSE，
	// 因此这里固定声明 text/event-stream（claude/codex 的流内 event 帧由 handler 产出）。
	streamHeaders := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}

	streamCtx, cancelStream, finishStream := beginPluginStream()
	go func() {
		defer finishStream()

		ctx := httpx.WithCallbackID(streamCtx, prepared.rpc.HostCallbackID)
		writer := newStreamWriter(ctx, cancelStream, prepared.rpc.HostCallbackID, prepared.rpc.StreamID, prepared.outputFormat())
		defer writer.Close()

		b, errBridge := bridgeFor(ctx, prepared.rpc.AuthID, prepared.cred)
		if errBridge != nil {
			// 交给 writer.Close() 统一收尾：它只会发一次 host.stream.close，
			// 先 reportStreamFailure 再 Close 会给同一条流发两次关闭帧。
			writer.fail(streamFailureMessage(errBridge))
			return
		}
		model := stripModelPrefix(loadedConfig().ModelPrefix, prepared.rpc.Model)
		req, errRequest := newBridgeRequest(ctx, prepared.rpc.Payload, prepared.rpc.Headers, model)
		if errRequest != nil {
			writer.fail(streamFailureMessage(errRequest))
			return
		}
		runBridgeHandler(prepared.rpc.AuthID, prepared.handler, b, writer, req)
	}()

	return okEnvelope(rpcExecutorStreamResponse{Headers: streamHeaders})
}

// rpcExecutorStreamResponse 与宿主 rpcExecutorStreamResponse 对齐。
// 本插件走异步 emit（chunks 留空），因此只返回 headers。
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

// prepareExecution 完成同步校验：请求解码、凭证解析、格式与 handler 选择。
func prepareExecution(request []byte) (preparedExecution, error) {
	var prepared preparedExecution
	if errDecode := decodeStringRequest(request, &prepared.rpc); errDecode != nil {
		return prepared, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	cfg := loadedConfig()
	// 执行路径同样要能跨 bundle：宿主给的是单账号 storage，但多账号导出文件
	// 也可能以文件级 JSON 形式到达这里（参考 quota 路由的教训）。
	cred, errCred := parseQoderCredentialForAccount(prepared.rpc.StorageJSON, cfg.Region, prepared.rpc.AuthID, "", "")
	if errCred != nil {
		return prepared, newPluginError("qoder_credential_missing", errCred.Error(), http.StatusUnauthorized)
	}
	prepared.cred = cred
	if region := strings.TrimSpace(prepared.rpc.AuthAttributes["region"]); region != "" {
		prepared.cred.Region = qoder.NormalizeRegion(region)
	}
	// 请求解析/渲染遵循宿主指定的“输出格式优先、请求格式兜底”：
	// 两者在插件声明了同一组格式时一致；跨协议场景下以宿主声明为准才不会串帧。
	outputFormat, okOutputFormat := normalizeFormat(firstNonEmpty(prepared.rpc.Format, prepared.rpc.SourceFormat))
	format, okFormat := normalizeFormat(firstNonEmpty(prepared.rpc.SourceFormat, prepared.rpc.Format))
	if okOutputFormat {
		prepared.outputFormatValue = outputFormat
	}
	if !okFormat {
		return prepared, newPluginError("unsupported_format",
			fmt.Sprintf("unsupported protocol format %q (supported: %s, %s, %s)",
				prepared.rpc.Format, formatChatCompletions, formatClaude, formatCodex),
			http.StatusBadRequest)
	}
	prepared.format = format
	handler, errHandler := handlerForFormat(format)
	if errHandler != nil {
		return prepared, errHandler
	}
	prepared.handler = handler
	return prepared, nil
}

// runBridgeHandler 执行 handler，并把 panic 变成可诊断的错误而不是让宿主崩溃。
func runBridgeHandler(authID string, handler bridgeHandler, b *bridge.Bridge, writer http.ResponseWriter, req *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("bridge handler panic: %v", recovered)
			invalidateBridge(authID)
			if sw, ok := writer.(*streamWriter); ok {
				sw.fail(fmt.Sprintf("internal error: %v", recovered))
				return
			}
			if bw, ok := writer.(*bufferedWriter); ok {
				bw.fail(fmt.Sprintf("internal error: %v", recovered))
			}
		}
	}()
	handler(b, writer, req)
	if sw, ok := writer.(*streamWriter); ok && sw.hasEmitError() {
		// 中途转发失败（下游断开或上游异常）：丢弃缓存，下一次请求重建会话。
		invalidateBridge(authID)
	}
}

// streamFailureMessage 提取给客户端看的原因（插件错误的 Message 更干净）。
func streamFailureMessage(err error) string {
	if pe, ok := err.(*pluginError); ok && pe.Message != "" {
		return pe.Message
	}
	return err.Error()
}

// responseError 把 handler 的错误响应体转成插件错误（保留状态码）。
func responseError(status int, body []byte) error {
	message := streamErrorMessage(body)
	// 排队/服务未就绪：免费模型常见，必须与额度/凭证错误区分开。
	// （否则 403 会被宿主报成 insufficient_quota，把用户引向充值。）
	if _, queued := bridge.ParseQueueSignal(message); queued {
		return &pluginError{Code: "qoder_model_busy", Message: message, HTTPStatus: http.StatusServiceUnavailable}
	}
	// 到这一步 handler 已把错误友好化（原始排队载荷可能只剩 error.type 里的标记），
	// 所以再按 error.type 判一次，避免排队被误判成一般上游错误。
	if errorTypeOf(body) == bridge.ErrTypeModelBusy || (status == http.StatusServiceUnavailable && looksLikeBusyError(string(body))) {
		return &pluginError{Code: "qoder_model_busy", Message: message, HTTPStatus: http.StatusServiceUnavailable}
	}
	code := "qoder_request_failed"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = "qoder_credential_invalid"
	case status == http.StatusTooManyRequests:
		code = "qoder_rate_limited"
	case status >= 500:
		code = "qoder_upstream_error"
	}
	return &pluginError{Code: code, Message: message, HTTPStatus: status}
}

// looksLikeBusyError 识别 handler 已友好化的排队错误（正文含我们的中文提示）。
func looksLikeBusyError(body string) bool {
	return strings.Contains(body, "排队") && strings.Contains(body, "不是额度")
}

// responseHeaders 过滤逐跳头，保证至少有 Content-Type。
func responseHeaders(header http.Header) http.Header {
	out := http.Header{}
	for key, values := range header {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "transfer-encoding", "upgrade", "content-length":
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	if out.Get("Content-Type") == "" {
		out.Set("Content-Type", "application/json")
	}
	return out
}

func handleExecutorCountTokens(request []byte) ([]byte, error) {
	var rpc executorRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	// 上游没有 token 计数接口：用字节数做保守估算，并显式标注 estimated，
	// 避免客户端把它当成上游精确值。宁可高估也不能低估（客户端用它做上下文管理）。
	estimate := estimateTokens(rpc.OriginalRequest, rpc.Payload)
	payload, errMarshal := json.Marshal(map[string]any{
		"total_tokens":  estimate,
		"input_tokens":  estimate,
		"output_tokens": 0,
		"estimated":     true,
	})
	if errMarshal != nil {
		return nil, newPluginError("internal_error", errMarshal.Error(), http.StatusInternalServerError)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// estimateTokens 以 UTF-8 字节数估算 token（约 3 字节/token，向上取整）。
func estimateTokens(raw []byte, fallback []byte) int {
	if len(raw) == 0 {
		raw = fallback
	}
	if len(raw) == 0 {
		return 0
	}
	return (len(raw) + 2) / 3
}

func handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	var rpc executorHTTPRPCRequest
	if errDecode := decodeStringRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	if strings.TrimSpace(rpc.URL) == "" {
		return nil, newPluginError("invalid_request", "url is required", http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), rpc.HostCallbackID)
	method := strings.ToUpper(strings.TrimSpace(rpc.Method))
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if len(rpc.Body) > 0 {
		bodyReader = strings.NewReader(string(rpc.Body))
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, rpc.URL, bodyReader)
	if errRequest != nil {
		return nil, newPluginError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	if rpc.Headers != nil {
		req.Header = rpc.Headers.Clone()
	}
	resp, errDo := httpx.Client(ctx, 60*time.Second).Do(req)
	if errDo != nil {
		return nil, newPluginError("qoder_upstream_error", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := readAllLimited(resp.Body, executorHTTPLimit)
	if errRead != nil {
		return nil, newPluginError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    responseHeaders(resp.Header),
		Body:       body,
	})
}

// readAllLimited 读取全部正文并限制大小（超限返回错误，避免把内存打满）。
func readAllLimited(body io.Reader, limit int64) ([]byte, error) {
	raw, errRead := io.ReadAll(io.LimitReader(body, limit+1))
	if errRead != nil {
		if errors.Is(errRead, io.EOF) {
			return raw, nil
		}
		return raw, errRead
	}
	if int64(len(raw)) > limit {
		return raw, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return raw, nil
}
