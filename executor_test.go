package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/cpasdk/pluginapi"
)

// qoderEnvelopeFrame 生成上游信封帧（hub iter_inner_sse 形态）：
// 上游把内层 chunk 放在 body 字符串里，并带 statusCodeValue。
func qoderEnvelopeFrame(status int, inner string) string {
	return "data: {\"body\":" + strconv.Quote(inner) + ",\"statusCodeValue\":" + strconv.Itoa(status) + "}\n\n"
}

func qoderContentFrame(text string) string {
	return qoderEnvelopeFrame(200, `{"choices":[{"delta":{"content":`+strconv.Quote(text)+`}}]}`)
}

func qoderFinishFrame() string {
	return qoderEnvelopeFrame(200, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`)
}

// setupQoderUpstream 让假宿主像真实 Qoder 上游一样应答：
// jobToken 交换、SSE chat、模型清单、签到活动。
func setupQoderUpstream(host *fakeHost, sse string) {
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/user/jobToken"):
			return fakeUpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   `{"id":"uid-1","name":"tester","userType":"personal_standard","securityOauthToken":"so-token","refreshToken":"rt-token"}`,
			}
		case strings.Contains(url, "/api/v1/userinfo"):
			return fakeUpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   `{"id":"uid-1","name":"tester","organization_id":"org-1"}`,
			}
		case strings.Contains(url, "agent_chat_generation"):
			return fakeUpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:   sse,
			}
		case strings.Contains(url, "/model/list"):
			return fakeUpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   `{"assistant":[{"key":"gmodel","display_name":"Performance","enable":true,"is_default":true,"is_reasoning":true,"context_window":180000,"max_output_tokens":32768}]}`,
			}
		case strings.Contains(url, "/campaigns"):
			return fakeUpstreamResponse{Status: 200, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: `{"campaigns":[]}`}
		default:
			return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected upstream url ` + url + `"}`}
		}
	}
}

func qoderAuthStorage(token string) []byte {
	return []byte(`{"type":"qoder","token":"` + token + `","region":"global","label":"测试账号"}`)
}

func executorStreamPayload(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
}

func executorChatPayload(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"你好"}]}`)
}

func executorRequest(t *testing.T, req executorRPCRequest) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal executor request: %v", errMarshal)
	}
	return raw
}

// handleRPC 走与 ABI 相同的转换路径：handler 返回的 error 由 errorEnvelopeFromError
// 变成错误信封（含 http_status），因此测试断言的就是宿主真正看到的东西。
func handleRPC(t *testing.T, handler func([]byte) ([]byte, error), request []byte) map[string]any {
	t.Helper()
	raw, errHandle := handler(request)
	if errHandle != nil {
		raw = errorEnvelopeFromError(errHandle)
	}
	return decodeExecutorEnvelope(t, raw)
}

// decodeExecutorEnvelope 把信封解成统一形态：
// 成功 → {"ok": true, "result": {...}}；失败 → {"ok": false, "code","message","status"}。
func decodeExecutorEnvelope(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if !env.OK {
		message := ""
		code := ""
		status := 0
		if env.Error != nil {
			message, code, status = env.Error.Message, env.Error.Code, env.Error.HTTPStatus
		}
		return map[string]any{"ok": false, "message": message, "code": code, "status": status}
	}
	result := map[string]any{}
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &result); errUnmarshal != nil {
			t.Fatalf("decode result: %v (raw=%s)", errUnmarshal, env.Result)
		}
	}
	result["ok"] = true
	return result
}

// resultOf 要求信封成功并取出结果对象。
func resultOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	if envelope["ok"] != true {
		t.Fatalf("rpc failed: %+v", envelope)
	}
	return envelope
}

func errorStatus(envelope map[string]any) int {
	switch value := envelope["status"].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func decodeBase64String(value any) (string, error) {
	text, _ := value.(string)
	raw, errMarshal := json.Marshal(text)
	if errMarshal != nil {
		return "", errMarshal
	}
	var decoded []byte
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		return "", errUnmarshal
	}
	return string(decoded), nil
}

func firstHeaderValue(headers map[string]any, name string) string {
	for key, value := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		list, ok := value.([]any)
		if !ok || len(list) == 0 {
			return ""
		}
		text, _ := list[0].(string)
		return text
	}
	return ""
}

// waitForStreamClose 等待异步转发结束时写下的 host.stream.close。
func waitForStreamClose(t *testing.T, host *fakeHost) {
	t.Helper()
	for attempt := 0; attempt < 400; attempt++ {
		if closed, _ := host.streamCloses(); len(closed) > 0 {
			return
		}
		sleepShort()
	}
	t.Fatalf("timed out waiting for host.stream.close; calls=%v", host.recordCalls())
}

func streamRPCRequestModel(model string) executorRPCRequest {
	return executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			AuthProvider: providerKey,
			Model:        model,
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Stream:       true,
			Payload:      executorStreamPayload(model),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
		StreamID: "host-stream-1",
	}
}

// TestExecutorStreamEndToEnd 覆盖完整链路：
// 插件 → 宿主 HTTP 桥（出站）→ 上游 SSE → delta 解析 → chat handler → host.stream.emit。
func TestExecutorStreamEndToEnd(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupQoderUpstream(host, qoderContentFrame("你")+qoderContentFrame("好")+qoderFinishFrame())

	result := resultOf(t, handleRPC(t, handleExecutorExecuteStream, executorRequest(t, streamRPCRequestModel("qoder-claude-sonnet"))))
	headers, _ := result["headers"].(map[string]any)
	if contentType := firstHeaderValue(headers, "Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("stream headers missing SSE content type: %+v", result["headers"])
	}

	waitForStreamClose(t, host)

	// 线上踩过的坑：分片必须按宿主期望的分帧投递。这里断言 chat-completions
	// 分片是**裸 JSON**（宿主补 `data: `），而不是插件自己加前缀的 SSE 帧
	// ——否则客户端收到 `data: data: {...}`，JSON 解析失败，流式内容全空。
	chunks := host.emittedChunks()
	if len(chunks) == 0 {
		t.Fatal("no stream chunks were emitted")
	}
	joined := strings.Join(chunks, "\n")
	for _, want := range []string{`"content":"你"`, `"content":"好"`, `"finish_reason":"stop"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("emitted chunks missing %q: %q", want, joined)
		}
	}
	for _, chunk := range chunks {
		if !json.Valid([]byte(chunk)) {
			t.Fatalf("chat-completions chunk must be bare JSON (host adds the data: prefix): %q", chunk)
		}
		if strings.Contains(chunk, "data: ") {
			t.Fatalf("chunk carries an SSE prefix the host will duplicate: %q", chunk)
		}
	}
	if strings.Contains(joined, "[DONE]") {
		t.Fatal("plugin must not forward [DONE]; the host appends it itself")
	}

	closed, closeErrs := host.streamCloses()
	if len(closed) != 1 {
		t.Fatalf("stream close called %d times, want exactly 1 (%v)", len(closed), closed)
	}
	if closeErrs[0] != "" {
		t.Fatalf("successful stream closed with error: %q", closeErrs[0])
	}

	// 出站请求确实走了宿主桥（否则 CPA 的请求日志里看不到上游流量）。
	calls := strings.Join(host.recordCalls(), ",")
	if !strings.Contains(calls, pluginabi.MethodHostHTTPDoStream) {
		t.Fatalf("upstream stream did not go through host http bridge: %v", calls)
	}
	if !strings.Contains(calls, pluginabi.MethodHostHTTPDo) {
		t.Fatalf("jobToken exchange did not go through host http bridge: %v", calls)
	}
}

func TestExecutorStreamReportsUpstreamFailure(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/user/jobToken") {
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token"}`}
		}
		// 403 而非 5xx：上游权限/凭证类失败不参与瞬时重试，
		// 这样测试断言的是"失败如何上报"，而不是退避重试的次数。
		return fakeUpstreamResponse{Status: 403, Body: `{"message":"forbidden"}`}
	}

	result := resultOf(t, handleRPC(t, handleExecutorExecuteStream, executorRequest(t, streamRPCRequestModel("qoder-claude-sonnet"))))
	if result["headers"] == nil {
		t.Fatalf("stream response missing headers: %+v", result)
	}
	waitForStreamClose(t, host)

	closed, closeErrs := host.streamCloses()
	if len(closed) != 1 || closeErrs[0] == "" {
		t.Fatalf("upstream failure must surface as a stream error, got %v", closeErrs)
	}
	if !strings.Contains(closeErrs[0], "forbidden") && !strings.Contains(closeErrs[0], "upstream") {
		t.Fatalf("stream error should carry the upstream reason: %q", closeErrs[0])
	}
	// 还没有投递过任何内容时，错误帧不作为载荷转发：改由关闭帧上报，
	// CPA 才能把这次执行当作失败（冷却凭证 / 故障转移）。
	if emitted := host.emittedChunks(); len(emitted) != 0 {
		t.Fatalf("error-only stream must not forward payload chunks, got %q", strings.Join(emitted, ""))
	}
}

// TestExecutorStreamPreparationFailureClosesStreamOnce 锁死“建连失败被关闭两次”的回归：
// 早期实现先 reportStreamFailure 再由 defer Close 收尾，同一条流会收到两次 host.stream.close，
// 第二次不带错误的关闭帧会把真正的失败原因冲掉。
func TestExecutorStreamPreparationFailureClosesStreamOnce(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	// jobToken 交换失败 ⇒ bridgeFor 失败 ⇒ 在刚开流、尚未建连时就失败。
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		return fakeUpstreamResponse{Status: 401, Body: `{"message":"token is not active"}`}
	}

	result := resultOf(t, handleRPC(t, handleExecutorExecuteStream, executorRequest(t, streamRPCRequestModel("qoder-claude-sonnet"))))
	if result["headers"] == nil {
		t.Fatalf("stream response missing headers: %+v", result)
	}
	waitForStreamClose(t, host)
	// 异步转发可能还没跑完第二次关闭（如果有），稍等一会再断言。
	sleepShort()

	closed, closeErrs := host.streamCloses()
	if len(closed) != 1 {
		t.Fatalf("stream close called %d times, want exactly 1 (%v)", len(closed), closeErrs)
	}
	if closeErrs[0] == "" {
		t.Fatal("preparation failure must be reported as a stream error")
	}
	if emitted := host.emittedChunks(); len(emitted) != 0 {
		t.Fatalf("failed preparation must not emit payload, got %q", strings.Join(emitted, ""))
	}
}

func TestExecutorExecuteNonStream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupQoderUpstream(host, qoderContentFrame("你")+qoderContentFrame("好")+qoderFinishFrame())

	result := resultOf(t, handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			Model:        "qoder-claude-sonnet",
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Payload:      executorChatPayload("qoder-claude-sonnet"),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
	})))
	payload, errDecode := decodeBase64String(result["Payload"])
	if errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}
	var completion map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &completion); errUnmarshal != nil {
		t.Fatalf("payload is not JSON: %v (%s)", errUnmarshal, payload)
	}
	choices, _ := completion["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", completion["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if content, _ := message["content"].(string); content != "你好" {
		t.Fatalf("content = %q, want 你好", content)
	}
}

func TestExecutorExecuteNonStreamSurfacesUpstreamStatus(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/user/jobToken") {
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token"}`}
		}
		return fakeUpstreamResponse{Status: 401, Body: `{"message":"token expired"}`}
	}

	envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			Model:        "qoder-claude-sonnet",
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Payload:      executorChatPayload("qoder-claude-sonnet"),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
	}))
	if envelope["ok"] != false {
		t.Fatalf("expected an error envelope, got %+v", envelope)
	}
	if status := errorStatus(envelope); status != 401 {
		t.Fatalf("error http_status = %d, want 401（决定 CPA 是否冷却凭证）", status)
	}
}

func TestExecutorValidationErrors(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupQoderUpstream(host, qoderContentFrame("hi"))

	t.Run("unsupported format", func(t *testing.T) {
		envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthID:      "qoder-main",
				Model:       "qoder-claude-sonnet",
				Format:      "gemini",
				Payload:     executorChatPayload("qoder-claude-sonnet"),
				StorageJSON: qoderAuthStorage("pt-test-token"),
			},
		}))
		if envelope["ok"] != false || envelope["code"] != "unsupported_format" {
			t.Fatalf("envelope = %+v", envelope)
		}
		if status := errorStatus(envelope); status != 400 {
			t.Fatalf("http_status = %d, want 400", status)
		}
	})

	t.Run("missing credential", func(t *testing.T) {
		envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthID:  "qoder-main",
				Model:   "qoder-claude-sonnet",
				Format:  formatChatCompletions,
				Payload: executorChatPayload("qoder-claude-sonnet"),
			},
		}))
		if envelope["ok"] != false || envelope["code"] != "qoder_credential_missing" {
			t.Fatalf("envelope = %+v", envelope)
		}
		if status := errorStatus(envelope); status != 401 {
			t.Fatalf("http_status = %d, want 401", status)
		}
	})

	t.Run("stream without stream id", func(t *testing.T) {
		request := streamRPCRequestModel("qoder-claude-sonnet")
		request.StreamID = ""
		envelope := handleRPC(t, handleExecutorExecuteStream, executorRequest(t, request))
		if envelope["ok"] != false || envelope["code"] != "invalid_request" {
			t.Fatalf("envelope = %+v", envelope)
		}
	})
}

func TestExecutorReusesCachedBridgePerAccount(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupQoderUpstream(host, qoderContentFrame("hi")+qoderFinishFrame())

	request := executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			Model:        "qoder-claude-sonnet",
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Payload:      executorChatPayload("qoder-claude-sonnet"),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
	}
	for attempt := 0; attempt < 3; attempt++ {
		if envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, request)); envelope["ok"] != true {
			t.Fatalf("attempt %d failed: %+v", attempt, envelope)
		}
	}
	bufferedCalls := 0
	for _, call := range host.recordCalls() {
		if call == pluginabi.MethodHostHTTPDo {
			bufferedCalls++
		}
	}
	// 第一次请求做一次 jobToken 交换（buffered do），后续请求复用缓存 Bridge。
	if bufferedCalls != 1 {
		t.Fatalf("buffered host http calls = %d, want 1（Bridge 应按账号缓存）", bufferedCalls)
	}
}

func TestExecutorCountTokensEstimates(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	result := resultOf(t, handleRPC(t, handleExecutorCountTokens, []byte(`{"Model":"qoder-gmodel","OriginalRequest":"aGVsbG8gd29ybGQ="}`)))
	payload, errDecode := decodeBase64String(result["Payload"])
	if errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}
	var counted map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &counted); errUnmarshal != nil {
		t.Fatalf("payload is not JSON: %v", errUnmarshal)
	}
	if estimated, _ := counted["estimated"].(bool); !estimated {
		t.Fatalf("token estimate must be flagged as estimated: %v", counted)
	}
	if tokens, _ := counted["total_tokens"].(float64); int(tokens) <= 0 {
		t.Fatalf("total_tokens = %v", counted["total_tokens"])
	}
}

// TestExecutorQueueSignalIsRetriedThenSucceeds 锁死免费模型的排队行为：
// 上游对排队中的模型回 HTTP 403 + isQueued/serviceAvailable=false 载荷，
// 插件应按上游建议等待后重试，而不是把请求判死（旧实现会报成 insufficient_quota）。
func TestExecutorQueueSignalIsRetriedThenSucceeds(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	var attempts int
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/user/jobToken"):
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token"}`}
		case strings.Contains(url, "/model/list"):
			return fakeUpstreamResponse{Status: 200, Body: `{"models":[{"key":"qfmodel","displayName":"Qwen3.8-Flash"}]}`}
		case strings.Contains(url, "chat_generation"):
			attempts++
			if attempts == 1 {
				// 与实测一致：403 + 三层嵌套的排队载荷，retryAfterSeconds 用 1 秒以免测试变慢。
				return fakeUpstreamResponse{Status: 403, Body: `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":1,\\\"serviceAvailable\\\":false}\"}"}`}
			}
			return fakeUpstreamResponse{Status: 200, Body: qoderContentFrame("排队后成功")}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected"}`}
	}

	// 排队等待在 stream 打开路径上发生：用流式执行验证。
	streamReq := streamRPCRequestModel("qoder-qfmodel")
	streamReq.Model = "qoder-qfmodel"
	envelope := handleRPC(t, handleExecutorExecuteStream, executorRequest(t, streamReq))
	if envelope["ok"] != true {
		t.Fatalf("queue wait should have led to a successful stream: %+v", envelope)
	}
	// 流式执行是异步的：先等它收尾，再断言上游尝试次数。
	waitForStreamClose(t, host)
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (one queue rejection + one retry)", attempts)
	}
	emitted := strings.Join(host.emittedChunks(), "\n")
	if !strings.Contains(emitted, "排队后成功") {
		t.Fatalf("stream should carry the successful content: %s", emitted)
	}
	if len(host.streamErrs) != 0 {
		t.Fatalf("no stream error expected after a successful retry: %v", host.streamErrs)
	}
}

// TestExecutorQueueBudgetExhaustedReportsBusy 等待预算耗尽后必须报“排队”而不是额度/凭证错误。
func TestExecutorQueueBudgetExhaustedReportsBusy(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/user/jobToken"):
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token"}`}
		case strings.Contains(url, "chat_generation"):
			return fakeUpstreamResponse{Status: 403, Body: `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"retryAfterSeconds\\\":1,\\\"serviceAvailable\\\":false}\"}"}`}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected"}`}
	}

	envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			Model:        "qoder-qfmodel",
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Payload:      executorChatPayload("qoder-qfmodel"),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
	}))
	if envelope["ok"] != false {
		t.Fatalf("expected an error envelope once the queue budget is exhausted: %+v", envelope)
	}
	if status := errorStatus(envelope); status != 503 {
		t.Fatalf("error http_status = %d, want 503（403 会被宿主标成 insufficient_quota）", status)
	}
	if code, _ := envelope["code"].(string); code != "qoder_model_busy" {
		t.Fatalf("error code = %q, want qoder_model_busy (envelope=%v)", code, envelope)
	}
	if message, _ := envelope["message"].(string); !strings.Contains(message, "排队") {
		t.Fatalf("message should explain the queue: %q", message)
	}
}

// TestExecutorQueuePolicyDisabledFailsFast 锁死 queue_max_waits=0 的行为：
// 运维想让免费模型“快速失败”时不等待，但仍然必须报“排队”而不是额度错误。
func TestExecutorQueuePolicyDisabledFailsFast(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t, func(cfg *pluginConfig) {
		cfg.QueueMaxWaits = 0
		cfg.QueueWaitSeconds = 0
	})
	var attempts int
	host.upstream = func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, "/user/jobToken"):
			return fakeUpstreamResponse{Status: 200, Body: `{"id":"uid-1","name":"tester","securityOauthToken":"so-token"}`}
		case strings.Contains(url, "chat_generation"):
			attempts++
			return fakeUpstreamResponse{Status: 403, Body: `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":false}\"}"}`}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"error":"unexpected"}`}
	}

	started := time.Now()
	envelope := handleRPC(t, handleExecutorExecute, executorRequest(t, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "qoder-main",
			Model:        "qoder-qfmodel",
			Format:       formatChatCompletions,
			SourceFormat: formatChatCompletions,
			Payload:      executorChatPayload("qoder-qfmodel"),
			StorageJSON:  qoderAuthStorage("pt-test-token"),
		},
	}))
	elapsed := time.Since(started)
	if envelope["ok"] != false {
		t.Fatalf("expected an error envelope: %+v", envelope)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1 (no waiting configured)", attempts)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fail-fast took %s, want no upstream-suggested wait", elapsed)
	}
	if code, _ := envelope["code"].(string); code != "qoder_model_busy" {
		t.Fatalf("error code = %q, want qoder_model_busy", code)
	}
}

// TestExecutorStreamClaudeFrames 端到端：Claude 客户端请求经插件流式输出后，
// 分片仍是完整的 `event:`/`data:` SSE 帧（宿主对 Claude 是原样写出）。
func TestExecutorStreamClaudeFrames(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	setupQoderUpstream(host, qoderContentFrame("你")+qoderFinishFrame())

	req := streamRPCRequestModel("qoder-claude-sonnet")
	req.Format = formatClaude
	req.SourceFormat = formatClaude
	req.StreamID = "host-stream-claude"

	envelope := handleRPC(t, handleExecutorExecuteStream, executorRequest(t, req))
	if envelope["ok"] != true {
		t.Fatalf("claude stream should start: %+v", envelope)
	}
	waitForStreamClose(t, host)

	chunks := host.emittedChunks()
	if len(chunks) == 0 {
		t.Fatal("no claude chunks emitted")
	}
	joined := strings.Join(chunks, "")
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"text":"你"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claude stream missing %q: %q", want, joined)
		}
	}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(chunk), "event:") && !strings.HasPrefix(strings.TrimSpace(chunk), "data:") {
			t.Fatalf("claude chunk must be an SSE frame: %q", chunk)
		}
	}
}
