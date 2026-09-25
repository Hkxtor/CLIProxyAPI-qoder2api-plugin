package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/internal/bridge"
	"qoder2api-plugin/internal/logger"
)

// 本文件负责"协议 → handler"的路由与 http.ResponseWriter 适配。
//
// 复用移植过来的 qoder2api handler 是刻意的：三个协议（chat/completions、Claude
// messages、Codex responses）的报文拼装、流式增量、工具调用合并、错误帧语义都已
// 在上游项目中验证过。插件这边只提供等价的 http.ResponseWriter，把 handler 的
// 输出接到 CPA 的流式桥（host.stream.emit）上。

// CPA 宿主的协议格式名（见 sdk/translator/formats.go 与 pluginhost 的归一化规则）。
const (
	formatChatCompletions = "chat-completions"
	formatClaude          = "claude"
	formatCodex           = "codex"
)

// normalizeFormat 把宿主传入的 format 归一到本插件支持的三选一。
func normalizeFormat(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "openai", "chat-completions", "chat_completions", "openai-chat-completions":
		return formatChatCompletions, true
	case "claude", "anthropic", "messages":
		return formatClaude, true
	case "codex", "responses", "openai-responses", "openai_response":
		return formatCodex, true
	default:
		return "", false
	}
}

type bridgeHandler func(*bridge.Bridge, http.ResponseWriter, *http.Request)

// handlerForFormat 按格式选择移植过来的 handler。
func handlerForFormat(format string) (bridgeHandler, error) {
	switch format {
	case formatChatCompletions:
		return func(b *bridge.Bridge, w http.ResponseWriter, r *http.Request) { b.HandleChatCompletions(w, r) }, nil
	case formatClaude:
		return func(b *bridge.Bridge, w http.ResponseWriter, r *http.Request) { b.HandleClaudeMessages(w, r) }, nil
	case formatCodex:
		return func(b *bridge.Bridge, w http.ResponseWriter, r *http.Request) { b.HandleCodexResponses(w, r) }, nil
	default:
		return nil, newPluginError("unsupported_format", "unsupported protocol format: "+format, http.StatusBadRequest)
	}
}

// newBridgeRequest 构造交给 handler 的请求。
// body 是宿主翻译后的载荷；model 是 CPA 解析出的目标模型（已剥离插件前缀）。
func newBridgeRequest(ctx context.Context, payload []byte, headers http.Header, model string) (*http.Request, error) {
	body, errRewrite := rewritePayloadModel(payload, model)
	if errRewrite != nil {
		return nil, errRewrite
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "http://qoder.local/v1/messages", bytes.NewReader(body))
	if errRequest != nil {
		return nil, newPluginError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	for key, values := range headers {
		// 只透传与协议语义相关的头（Accept 影响流式判定），其余头由 bridge 自行生成。
		if strings.EqualFold(key, "Accept") || strings.EqualFold(key, "Accept-Language") {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// rewritePayloadModel 把载荷里的 model 字段改成上游可识别的名字。
//
// CPA 路由用的模型 ID 带插件前缀（qoder-xxx），而 bridge 需要的是客户端侧名字
// （claude-sonnet-4-5 之类），再由 bridge 的 MapModel 映射为上游 SKU key。
func rewritePayloadModel(payload []byte, model string) ([]byte, error) {
	target := strings.TrimSpace(model)
	if len(payload) == 0 {
		if target == "" {
			return payload, nil
		}
		return []byte(fmt.Sprintf(`{"model":%q}`, target)), nil
	}
	var body map[string]interface{}
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		// 载荷不是 JSON 对象：原样交给 handler，由它按协议报错。
		return payload, nil
	}
	bodyModel, _ := body["model"].(string)
	if target == "" {
		target = bodyModel
	}
	if target == "" {
		return payload, nil
	}
	if bodyModel == target {
		return payload, nil
	}
	body["model"] = target
	rewritten, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, newPluginError("invalid_request", "rewrite model field: "+errMarshal.Error(), http.StatusBadRequest)
	}
	return rewritten, nil
}

// bufferedWriter 是流式之外场景的 ResponseWriter：完整捕获状态码与响应体。
type bufferedWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newBufferedWriter() *bufferedWriter {
	return &bufferedWriter{header: http.Header{}, status: http.StatusOK}
}

func (w *bufferedWriter) Header() http.Header { return w.header }

func (w *bufferedWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *bufferedWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

// Flush 让 handler 里的 http.Flusher 类型断言成立（非流式路径无需真正 flush）。
func (w *bufferedWriter) Flush() {}

// fail 把 handler 级失败（如 panic）变成确定的 500 错误响应。
//
// 必须显式重置已缓冲的内容：状态码一旦写出就无法用 WriteHeader 覆盖，
// 否则 handler 已经写过的 200 与残缺正文会被当成成功响应回给宿主，
// CPA 会误判这次执行成功（不冷却凭证、不做故障转移）。
func (w *bufferedWriter) fail(message string) {
	if strings.TrimSpace(message) == "" {
		message = "internal error"
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    "plugin_internal_error",
		},
	})
	if errMarshal != nil {
		payload = []byte(`{"error":{"message":"internal error","type":"server_error","code":"plugin_internal_error"}}`)
	}
	w.header = http.Header{}
	w.header.Set("Content-Type", "application/json")
	w.body.Reset()
	w.wroteHeader = true
	w.status = http.StatusInternalServerError
	_, _ = w.body.Write(payload)
}

// streamWriter 把 handler 的输出实时转发到 CPA 的流式桥。
//
// 关键语义（都对应用户可观察的行为）：
//   - 首次 Write 之前若 handler 写了 >=400 状态码，说明请求在开流前就失败了
//     （例如载荷不合法）：此时不转发正文，而是把正文作为流错误上报，
//     否则客户端会把错误 JSON 当成正常 SSE 数据；
//   - emit 失败（宿主报 "stream is not open"）表示下游已断开：立即取消上游请求；
//   - Close 恰好执行一次 host.stream.close，错误信息随关闭帧下发。
type streamWriter struct {
	ctx        context.Context
	cancel     context.CancelFunc
	callbackID string
	streamID   string

	mu          sync.Mutex
	header      http.Header
	status      int
	wroteHeader bool
	errBody     bytes.Buffer
	failed      bool
	emitErr     error
	closed      bool
	// emitted 统计已投递的分片数；emittedContent 表示已投递过内容（非错误帧）。
	emitted        int
	emittedContent bool
	// outputFormat 是宿主期望我们输出的协议格式（rpc.format），决定流分片的分帧方式：
	// chat-completions 由宿主自己补 `data: ` 前缀，插件必须发裸 JSON 分片；
	// claude / codex 由宿主原样写出，插件必须发完整 SSE 帧。
	outputFormat string
}

func newStreamWriter(ctx context.Context, cancel context.CancelFunc, callbackID, streamID, outputFormat string) *streamWriter {
	return &streamWriter{
		ctx:          ctx,
		cancel:       cancel,
		callbackID:   callbackID,
		streamID:     streamID,
		header:       http.Header{},
		status:       http.StatusOK,
		outputFormat: outputFormat,
	}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if status >= 400 {
		w.failed = true
	}
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	if w.failed {
		// 开流前已判定失败：只缓存正文，等 Close 时作为流错误上报。
		w.errBody.Write(p)
		w.mu.Unlock()
		return len(p), nil
	}
	if w.emitErr != nil {
		err := w.emitErr
		w.mu.Unlock()
		return 0, err
	}
	// 尚未向客户端投递过任何内容时，错误帧要按"执行失败"上报（带 message 的
	// 关闭帧），而不是当成正常载荷转发：否则 CPA 会认为这次请求成功，
	// 既不会冷却坏凭证，也不会故障转移到其它账号。
	if w.emitted == 0 && !w.emittedContent {
		if message, isError := bridgeErrorFrame(p); isError {
			w.failed = true
			w.errBody.WriteString(message)
			w.mu.Unlock()
			return len(p), nil
		}
	}
	callbackID := w.callbackID
	streamID := w.streamID
	outputFormat := w.outputFormat
	w.mu.Unlock()

	// 分帧归一化：chat-completions 的宿主处理器会把分片包成 `data: <分片>\n\n`，
	// 所以插件必须发裸 JSON；claude/codex 的处理器原样写出，插件必须发完整 SSE 帧。
	// （历史 bug：三种格式统一加 `data: ` 前缀，OpenAI 客户端收到 `data: data: {...}`
	// → JSON 解析失败 → 流式内容全空。）
	frames := normalizeStreamFrames(p, outputFormat)
	emitted := 0
	for _, frame := range frames {
		payload := append([]byte(nil), frame...)
		if errEmit := emitStreamChunk(callbackID, streamID, payload); errEmit != nil {
			// 下游关闭或转发失败：记录并取消上游，避免继续烧配额。
			w.mu.Lock()
			if emitted > 0 {
				w.emitted += emitted
				w.emittedContent = true
			}
			w.emitErr = errEmit
			w.mu.Unlock()
			if w.cancel != nil {
				w.cancel()
			}
			return 0, errEmit
		}
		emitted++
	}
	if emitted > 0 {
		w.mu.Lock()
		w.emitted += emitted
		w.emittedContent = true
		w.mu.Unlock()
	}
	return len(p), nil
}

// normalizeStreamFrames 把 handler 写出的分片转成宿主期望的分帧：
//   - chat-completions：去掉 `data: ` 前缀与空行，只留裸 JSON；跳过 `[DONE]`
//     （宿主在流结束时自己补 `data: [DONE]`）。
//   - claude / codex：原样透传完整 SSE 帧（含 event:/data: 行）。
func normalizeStreamFrames(payload []byte, outputFormat string) [][]byte {
	if canonical, ok := normalizeFormat(outputFormat); !ok || canonical != formatChatCompletions {
		return [][]byte{payload}
	}
	body := string(payload)
	frames := make([][]byte, 0, 2)
	for _, block := range splitSSEBlocks(body) {
		data := sseDataPayload(block)
		if data == "" || data == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(data)) {
			// 非 JSON（例如上游原样转发的注释行）不投递：宿主要的是可解析分片。
			continue
		}
		frames = append(frames, []byte(data))
	}
	return frames
}

// splitSSEBlocks 按空行切开 SSE 块；没有空行时返回整段（单帧写法）。
func splitSSEBlocks(body string) []string {
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	blocks := make([]string, 0, 2)
	for _, block := range strings.Split(normalized, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		blocks = append(blocks, normalized)
	}
	return blocks
}

// sseDataPayload 取 SSE 块里所有 data 行拼接后的内容。
func sseDataPayload(block string) string {
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		lines = append(lines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Flush 让 handler 里的 http.Flusher 类型断言成立；emit 本身已是推送语义。
func (w *streamWriter) Flush() {}

// fail 把 handler 级失败（如 panic）转成随关闭帧下发的错误。
func (w *streamWriter) fail(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if w.errBody.Len() == 0 {
		w.errBody.WriteString(message)
	}
	w.failed = true
}

// hasEmitError 报告是否发生过转发失败。
func (w *streamWriter) hasEmitError() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.emitErr != nil
}

// Close 结束宿主流：必要时带上错误信息，且只执行一次。
func (w *streamWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	callbackID := w.callbackID
	streamID := w.streamID
	failure := ""
	switch {
	case w.failed && w.errBody.Len() > 0:
		failure = streamErrorMessage(w.errBody.Bytes())
	case w.failed:
		failure = fmt.Sprintf("upstream request failed with status %d", w.status)
	case w.emitErr != nil:
		// 已向客户端报过 emit 错误，不再重复上报。
		failure = ""
	}
	w.mu.Unlock()

	closeStream(callbackID, streamID, failure)
}

// bridgeErrorFrame 识别 SSE 错误帧并取出错误消息。
//
// 三种协议的失败帧形态：
//   - chat-completions：data: {"error":{"message":...,"type":...}}
//   - claude / codex ：event: error + data: {"type":"error","error":{...}}
//
// 判定要求 JSON 里存在 error 对象且不带 choices，避免把正常内容帧误判为错误
// （否则会把一次成功请求记成失败，触发不必要的凭证冷却）。
func bridgeErrorFrame(payload []byte) (string, bool) {
	body := strings.TrimSpace(string(payload))
	if body == "" {
		return "", false
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
			Choices []any `json:"choices"`
		}
		if errUnmarshal := json.Unmarshal([]byte(data), &frame); errUnmarshal != nil {
			continue
		}
		if frame.Error == nil || len(frame.Choices) > 0 {
			continue
		}
		message := strings.TrimSpace(frame.Error.Message)
		if message == "" {
			message = strings.TrimSpace(frame.Error.Type)
		}
		if message == "" {
			message = "上游返回错误帧"
		}
		return message, true
	}
	return "", false
}

// streamErrorMessage 从 handler 的错误响应体里取出人类可读消息。
//
// 注意：handler 已经做了友好化，原始排队载荷可能已不在 message 里，
// 所以另外提供 errorTypeOf 供调用方按 error.type 做机器可读判定。
func errorTypeOf(body []byte) string {
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal == nil {
		return strings.TrimSpace(payload.Error.Type)
	}
	return ""
}

func streamErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		return "upstream request failed"
	}
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

// emitStreamChunk 把一段载荷推给 CPA 宿主流。
func emitStreamChunk(callbackID, streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("stream id is missing")
	}
	_, errCall := callHostScoped(callbackID, pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if errCall != nil {
		logger.Debug("stream emit failed: %v", errCall)
	}
	return errCall
}

// closeStream 关闭 CPA 宿主流；failure 非空时由宿主转为错误帧。
func closeStream(callbackID, streamID, failure string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID}
	if failure != "" {
		payload["error"] = failure
	}
	if _, errCall := callHostScoped(callbackID, pluginabi.MethodHostStreamClose, payload); errCall != nil {
		logger.Debug("stream close failed: %v", errCall)
	}
}

// pluginErrorFromBridge 把 bridge 的错误映射为带 HTTP 状态的插件错误。
//
// 状态码语义（决定 CPA 的重试与冷却行为）：
//   - 400：内容审核 / 客户端参数问题（重试无效，不惩罚凭证）
//   - 401/403：凭证失效或权限不足 → 交给 CPA 走凭证刷新/冷却
//   - 429：上游限流 → 交给 CPA 限流与故障转移
//   - 502：上游瞬时故障（已在本插件内退避重试过）
func pluginErrorFromBridge(err error) *pluginError {
	if err == nil {
		return nil
	}
	message, errType := bridge.FriendlyError(err)
	status := bridge.ErrorStatus(err)
	if status <= 0 {
		status = http.StatusBadGateway
	}
	code := "qoder_upstream_error"
	switch errType {
	case bridge.ErrTypeContentPolicy:
		code = "qoder_content_policy"
	case bridge.ErrTypeTransient:
		code = "qoder_upstream_transient"
	case bridge.ErrTypeModelBusy:
		// 排队/服务未就绪：不是额度也不是凭证问题，别让用户去充值或换号。
		code = "qoder_model_busy"
	}
	if code == "qoder_model_busy" {
		return &pluginError{Code: code, Message: message, HTTPStatus: status}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = "qoder_credential_invalid"
	} else if status == http.StatusTooManyRequests {
		code = "qoder_rate_limited"
	}
	return &pluginError{Code: code, Message: message, HTTPStatus: status}
}
