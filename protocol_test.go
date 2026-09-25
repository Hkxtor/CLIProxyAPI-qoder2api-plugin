package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeFormat(t *testing.T) {
	cases := map[string]string{
		"":                 formatChatCompletions,
		"openai":           formatChatCompletions,
		"chat-completions": formatChatCompletions,
		"CHAT_COMPLETIONS": formatChatCompletions,
		"claude":           formatClaude,
		"anthropic":        formatClaude,
		"codex":            formatCodex,
		"responses":        formatCodex,
	}
	for input, want := range cases {
		got, ok := normalizeFormat(input)
		if !ok || got != want {
			t.Errorf("normalizeFormat(%q) = (%q,%v), want %q", input, got, ok, want)
		}
	}
	if _, ok := normalizeFormat("gemini"); ok {
		t.Error("gemini should not be accepted（宿主会先把 Gemini 客户端请求翻译成 chat-completions）")
	}
}

func TestRewritePayloadModel(t *testing.T) {
	rewritten, errRewrite := rewritePayloadModel([]byte(`{"model":"qoder-gmodel","stream":true}`), "gmodel")
	if errRewrite != nil {
		t.Fatalf("rewritePayloadModel: %v", errRewrite)
	}
	if !strings.Contains(string(rewritten), `"model":"gmodel"`) {
		t.Fatalf("model not rewritten: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"stream":true`) {
		t.Fatalf("other fields must be preserved: %s", rewritten)
	}

	// 非 JSON 载荷原样交给 handler 报错，不在这里伪造内容。
	raw := []byte("not-json")
	out, errOut := rewritePayloadModel(raw, "gmodel")
	if errOut != nil || string(out) != "not-json" {
		t.Fatalf("non-JSON payload should pass through: %q %v", out, errOut)
	}

	// 载荷没有 model 时用宿主给出的模型补齐（Claude 客户端一定有 model，这里只是兜底）。
	out, errOut = rewritePayloadModel([]byte(`{"messages":[]}`), "claude-sonnet")
	if errOut != nil || !strings.Contains(string(out), `"model":"claude-sonnet"`) {
		t.Fatalf("missing model should be filled from request: %q %v", out, errOut)
	}
}

func TestBufferedWriterCapturesStatusAndBody(t *testing.T) {
	writer := newBufferedWriter()
	writer.WriteHeader(http.StatusTooManyRequests)
	_, _ = writer.Write([]byte(`{"error":{"message":"slow down"}}`))
	if writer.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d", writer.status)
	}
	if got := writer.body.String(); got != `{"error":{"message":"slow down"}}` {
		t.Fatalf("body = %q", got)
	}
}

// TestBufferedWriterFailOverwritesPartialSuccess 锁死 panic 收尾的回归：
// handler 可能已经写了 200 与部分正文；fail 必须把响应覆盖成确定的 500 错误，
// 否则宿主会把残缺正文当成功（于是不冷却凭证、不故障转移）。
func TestBufferedWriterFailOverwritesPartialSuccess(t *testing.T) {
	writer := newBufferedWriter()
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(`data: partial`))

	writer.fail("internal error: boom")

	if writer.status != http.StatusInternalServerError {
		t.Fatalf("status after fail = %d, want 500", writer.status)
	}
	body := writer.body.String()
	if strings.Contains(body, "partial") {
		t.Fatalf("partial success body must be discarded, got %q", body)
	}
	if !strings.Contains(body, "boom") {
		t.Fatalf("failure reason must be reported, got %q", body)
	}
	// 走一遍非流式入口的判定：>=400 会被翻译成插件错误而不是成功响应。
	if writer.status < http.StatusBadRequest {
		t.Fatal("fail 后的状态码必须让调用方走错误分支")
	}
}

// TestBufferedWriterFailUsesFallbackMessage 确认空消息不会产生空错误体。
func TestBufferedWriterFailUsesFallbackMessage(t *testing.T) {
	writer := newBufferedWriter()
	writer.fail("   ")
	if !strings.Contains(writer.body.String(), "internal error") {
		t.Fatalf("fallback message missing: %q", writer.body.String())
	}
}

// TestStreamWriterEmitsAndClosesOnce 覆盖流式写入的基本契约：
// 每个 Write 立刻转发，Close 只执行一次 host.stream.close。
//
// chat-completions 的分片必须是**裸 JSON**：宿主的 OpenAI 处理器会自己包
// `data: <分片>\n\n`，插件再加前缀就会变成 `data: data: {...}`，客户端解析失败。
func TestStreamWriterEmitsAndClosesOnce(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	writer := newStreamWriter(context.Background(), func() {}, "", "stream-x", formatChatCompletions)
	if _, errWrite := writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	// 上游结束标记由宿主补发，插件转发会变成 `data: data: [DONE]`，必须丢弃。
	if _, errWrite := writer.Write([]byte("data: [DONE]\n\n")); errWrite != nil {
		t.Fatalf("write done: %v", errWrite)
	}
	writer.Close()
	writer.Close() // 幂等：第二次不应再触发 host.stream.close

	got := host.emittedChunks()
	if len(got) != 1 {
		t.Fatalf("emitted = %q, want exactly the JSON chunk", got)
	}
	if !json.Valid([]byte(got[0])) {
		t.Fatalf("chat-completions chunk must be bare JSON for the host to wrap: %q", got[0])
	}
	if strings.HasPrefix(got[0], "data:") || strings.Contains(got[0], "data: ") {
		t.Fatalf("chat-completions chunk must not carry an SSE prefix: %q", got[0])
	}
	if !strings.Contains(got[0], `"content":"one"`) {
		t.Fatalf("unexpected chunk payload: %q", got[0])
	}
	closed, errs := host.streamCloses()
	if len(closed) != 1 {
		t.Fatalf("close called %d times, want 1", len(closed))
	}
	if errs[0] != "" {
		t.Fatalf("unexpected close error: %q", errs[0])
	}
}

// TestStreamWriterCancelsOnEmitFailure 覆盖"下游断开"：emit 失败必须
// 取消上游请求（否则会继续烧账号配额），并且不再重复上报错误。
func TestStreamWriterCancelsOnEmitFailure(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	// 让 host.stream.emit 失败：模拟宿主判定流已关闭（下游断开）。
	previous := hostCallScopedImpl
	hostCallScopedImpl = func(callbackID, method string, payload any) (json.RawMessage, error) {
		if strings.Contains(method, "stream.emit") {
			return nil, context.Canceled
		}
		return host.call(callbackID, method, payload)
	}
	defer func() { hostCallScopedImpl = previous }()

	var canceled atomic.Bool
	writer := newStreamWriter(context.Background(), func() { canceled.Store(true) }, "", "stream-y", formatChatCompletions)
	if _, errWrite := writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")); errWrite == nil {
		t.Fatal("expected emit failure to propagate to the writer")
	}
	if !canceled.Load() {
		t.Fatal("emit failure must cancel the upstream request")
	}
	if !writer.hasEmitError() {
		t.Fatal("writer should remember the emit failure")
	}
	writer.Close()
	_, errs := host.streamCloses()
	if len(errs) != 1 {
		t.Fatalf("close should still happen exactly once, got %v", errs)
	}
	if errs[0] != "" {
		t.Fatalf("emit failure already reported through the chunk error; close must not repeat it: %q", errs[0])
	}
}

// TestStreamWriterFailsBeforeAnyContent 覆盖"开流前就失败"：
// handler 写了 4xx JSON（例如载荷不合法）时，正文要作为流错误上报，
// 而不是当成正常 SSE 数据发给客户端。
func TestStreamWriterFailsBeforeAnyContent(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	writer := newStreamWriter(context.Background(), func() {}, "", "stream-z", formatChatCompletions)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write([]byte(`{"error":{"message":"invalid payload","type":"qoder_error"}}`))
	writer.Close()

	if emitted := host.emittedChunks(); len(emitted) != 0 {
		t.Fatalf("error body must not be forwarded as stream payload: %q", emitted)
	}
	_, errs := host.streamCloses()
	if len(errs) != 1 || !strings.Contains(errs[0], "invalid payload") {
		t.Fatalf("close error = %v, want the handler message", errs)
	}
}

// TestStreamWriterKeepsErrorFrameAfterContent 覆盖"流中途失败"：
// 已经投递过内容后，错误帧按普通载荷转发（客户端能读到失败原因），
// 不再当成整体失败上报——CPA 此时无法安全地重放这一轮。
func TestStreamWriterKeepsErrorFrameAfterContent(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	writer := newStreamWriter(context.Background(), func() {}, "", "stream-w", formatChatCompletions)
	_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = writer.Write([]byte("data: {\"error\":{\"message\":\"mid-stream failure\"}}\n\n"))
	writer.Close()

	emitted := host.emittedChunks()
	if len(emitted) != 2 {
		t.Fatalf("emitted = %q", emitted)
	}
	for _, chunk := range emitted {
		if !json.Valid([]byte(chunk)) {
			t.Fatalf("each chat-completions chunk must be bare JSON: %q", chunk)
		}
	}
	if !strings.Contains(emitted[1], "mid-stream failure") {
		t.Fatalf("error frame should reach the client: %q", emitted[1])
	}
	_, errs := host.streamCloses()
	if errs[0] != "" {
		t.Fatalf("mid-stream error frame should not fail the whole stream: %q", errs[0])
	}
}

func TestBridgeErrorFrameDetection(t *testing.T) {
	if _, isError := bridgeErrorFrame([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")); isError {
		t.Error("content frame must not be treated as an error")
	}
	message, isError := bridgeErrorFrame([]byte("data: {\"error\":{\"message\":\"boom\",\"type\":\"qoder_error\"}}\n\n"))
	if !isError || message != "boom" {
		t.Errorf("chat error frame: %q %v", message, isError)
	}
	message, isError = bridgeErrorFrame([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"))
	if !isError || message != "overloaded" {
		t.Errorf("claude error frame: %q %v", message, isError)
	}
	if _, isError := bridgeErrorFrame([]byte("data: [DONE]\n\n")); isError {
		t.Error("[DONE] is not an error frame")
	}
	if _, isError := bridgeErrorFrame([]byte("not sse at all")); isError {
		t.Error("non-SSE payload is not an error frame")
	}
}

func TestWriteResponseHeadersFiltersHopByHop(t *testing.T) {
	headers := responseHeaders(http.Header{
		"Content-Type":      []string{"text/event-stream"},
		"Connection":        []string{"keep-alive"},
		"Content-Length":    []string{"123"},
		"Transfer-Encoding": []string{"chunked"},
	})
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type lost: %v", headers)
	}
	for _, banned := range []string{"Connection", "Content-Length", "Transfer-Encoding"} {
		if headers.Get(banned) != "" {
			t.Fatalf("hop-by-hop header %s must be dropped: %v", banned, headers)
		}
	}
	if headers.Get("Content-Type") == "" {
		t.Fatal("content type must always be present")
	}
}

func TestResponseErrorMapsStatusToCode(t *testing.T) {
	cases := []struct {
		status   int
		wantCode string
	}{
		{http.StatusUnauthorized, "qoder_credential_invalid"},
		{http.StatusForbidden, "qoder_credential_invalid"},
		{http.StatusTooManyRequests, "qoder_rate_limited"},
		{http.StatusBadGateway, "qoder_upstream_error"},
		{http.StatusBadRequest, "qoder_request_failed"},
	}
	for _, tc := range cases {
		err := responseError(tc.status, []byte(`{"error":{"message":"x"}}`))
		pluginErr, ok := err.(*pluginError)
		if !ok {
			t.Fatalf("status %d: type %T", tc.status, err)
		}
		if pluginErr.HTTPStatus != tc.status || pluginErr.Code != tc.wantCode {
			t.Errorf("status %d -> %+v", tc.status, pluginErr)
		}
	}
}

func TestWaitPluginStreamsCancelsInflight(t *testing.T) {
	resetPluginGlobals(t)
	ctx, cancel, finish := beginPluginStream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		finish()
	}()
	_ = cancel

	start := time.Now()
	waitPluginStreams(2 * time.Second)
	if time.Since(start) > time.Second {
		t.Fatal("waitPluginStreams should return as soon as the stream finishes")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream goroutine did not observe cancellation")
	}
}

// TestStreamFramesAreProtocolSpecific 锁死分帧契约（线上踩过的 bug）：
//   - chat-completions：插件发裸 JSON（宿主自己补 `data: ` 前缀），且丢弃 [DONE]；
//   - claude / codex：插件发完整 SSE 帧（宿主原样写出）。
func TestStreamFramesAreProtocolSpecific(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	jsonChunk := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	claudeFrame := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
	codexFrame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")

	cases := []struct {
		name         string
		outputFormat string
		payload      []byte
		want         []byte
	}{
		{"chat-completions strips framing", formatChatCompletions, jsonChunk, []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)},
		{"chat-completions drops done", formatChatCompletions, []byte("data: [DONE]\n\n"), nil},
		{"claude passes frames through", formatClaude, claudeFrame, claudeFrame},
		{"codex passes frames through", formatCodex, codexFrame, codexFrame},
		{"unknown format passes through", "weird-format", claudeFrame, claudeFrame},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			frames := normalizeStreamFrames(testCase.payload, testCase.outputFormat)
			if testCase.want == nil {
				if len(frames) != 0 {
					t.Fatalf("frames = %q, want none", frames)
				}
				return
			}
			if len(frames) != 1 || !bytes.Equal(frames[0], testCase.want) {
				t.Fatalf("frames = %q, want %q", frames, testCase.want)
			}
		})
	}
}

// TestStreamWriterKeepsClaudeFramesIntact 端到端：claude 格式的流分片必须保持
// `event:`/`data:` 双行结构，宿主才会原样转发给 Claude 客户端。
func TestStreamWriterKeepsClaudeFramesIntact(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	writer := newStreamWriter(context.Background(), func() {}, "", "stream-claude", formatClaude)
	frame := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n"
	if _, errWrite := writer.Write([]byte(frame)); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	writer.Close()

	emitted := host.emittedChunks()
	if len(emitted) != 1 || emitted[0] != frame {
		t.Fatalf("claude frames must pass through verbatim: %q", emitted)
	}
	if !strings.HasPrefix(emitted[0], "event:") {
		t.Fatalf("claude frame lost its event line: %q", emitted[0])
	}
}
