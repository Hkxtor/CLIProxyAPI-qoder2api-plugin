// Package httpx —— 把插件内的出站 HTTP 请求接到 CPA 宿主的 HTTP 桥上。
//
// 为什么必须走宿主：
//   - 宿主负责代理（全局 proxy / 账号级 proxy）、传输策略与连接复用；
//   - 宿主会把出站请求与上游原始响应写进请求日志（request-log），
//     否则 CPA 管理端的日志里看不到任何上游流量，排障只能靠插件自己的日志。
//
// 两个客户端对应宿主的两种桥：
//   - Client：host.http.do，一次调用拿到完整响应体（查询、token 交换等小响应）；
//   - StreamClient：host.http.do_stream + host.http.stream_read，用于 SSE 长流。
//
// 每个客户端都绑定调用方 context 中的 host_callback_id（见 WithCallbackID）：
// 宿主靠它把出站请求归属到具体请求作用域，并在请求结束时回收流。
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoder2api-plugin/cpasdk/pluginabi"
)

// HostCaller 是宿主回调实现，由 package main 注入（避免 internal 包反向依赖 main）。
type HostCaller func(callbackID, method string, payload any) (json.RawMessage, error)

var (
	callerMu sync.RWMutex
	caller   HostCaller
)

// Configure 注册宿主回调实现。插件初始化时调用一次。
func Configure(fn HostCaller) {
	callerMu.Lock()
	caller = fn
	callerMu.Unlock()
}

func hostCaller() HostCaller {
	callerMu.RLock()
	defer callerMu.RUnlock()
	return caller
}

type callbackIDKey struct{}

// WithCallbackID 把宿主回调 ID 绑定到 context，后续出站请求会带上它。
func WithCallbackID(ctx context.Context, callbackID string) context.Context {
	if strings.TrimSpace(callbackID) == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callbackIDKey{}, strings.TrimSpace(callbackID))
}

// CallbackID 读取 context 中的宿主回调 ID。
func CallbackID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(callbackIDKey{}).(string)
	return id
}

// Client 返回经宿主 host.http.do 出站的客户端。
func Client(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx}, Timeout: timeout}
}

// StreamClient 返回经宿主 host.http.do_stream 出站的客户端（用于 SSE）。
func StreamClient(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx, stream: true}, Timeout: timeout}
}

type transport struct {
	ctx    context.Context
	stream bool
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	caller := hostCaller()
	if caller == nil {
		return nil, fmt.Errorf("host http bridge is unavailable")
	}
	// 请求上下文优先：http.Client 的 Timeout 与调用方取消都会体现在这里。
	ctx := t.ctx
	if reqCtx := req.Context(); reqCtx != nil {
		ctx = reqCtx
	}
	// 两侧上下文都要生效：调用方取消（请求 ctx）与客户端生命周期（t.ctx）
	// 任一结束都必须能中断流式读取，否则请求无 ctx 时取消信号会丢失。
	if t.ctx != nil && ctx != t.ctx {
		ctx = mergeContexts(ctx, t.ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, errBody := readRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}
	payload := hostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
		Body:    body,
	}
	callbackID := CallbackID(ctx)
	if t.stream {
		return t.openStream(caller, callbackID, ctx, req, payload)
	}
	raw, errCall := caller(callbackID, pluginabi.MethodHostHTTPDo, payload)
	if errCall != nil {
		return nil, errCall
	}
	var resp rpcHostHTTPResponse
	resp.statusMissing = true
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
	}
	if resp.statusMissing {
		// 不能静默当 0：那会让上游 200 也被判成失败，错误体里又带着真实响应，
		// 看起来像是凭证被拒（真实踩过的坑）。宁可直接报协议不匹配。
		return nil, fmt.Errorf("host http response has no status field (host ABI mismatch): %s", truncateForError(raw))
	}
	return &http.Response{
		StatusCode:    resp.StatusCode,
		Status:        fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		Header:        http.Header(resp.Headers),
		Body:          io.NopCloser(bytes.NewReader(resp.Body)),
		ContentLength: int64(len(resp.Body)),
		Request:       req,
	}, nil
}

func (t *transport) openStream(caller HostCaller, callbackID string, ctx context.Context, req *http.Request, payload hostHTTPRequest) (*http.Response, error) {
	raw, errCall := caller(callbackID, pluginabi.MethodHostHTTPDoStream, payload)
	if errCall != nil {
		return nil, errCall
	}
	var resp rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream response: %w", errUnmarshal)
	}
	body := &streamBody{ctx: ctx, callbackID: callbackID, streamID: resp.StreamID}
	if len(resp.Chunks) > 0 {
		// 宿主已把响应体缓冲在本次 RPC 结果里（小响应或不支持流桥的宿主）。
		buffered := make([]byte, 0, 4096)
		for _, chunk := range resp.Chunks {
			if chunk.Error != "" {
				_ = body.Close()
				return nil, fmt.Errorf("upstream stream error: %s", chunk.Error)
			}
			buffered = append(buffered, chunk.Payload...)
		}
		body.acceptBuffered(buffered)
	}
	if body.streamID == "" && !body.isDrained() {
		return nil, fmt.Errorf("host http stream bridge returned no stream id")
	}
	return &http.Response{
		StatusCode:    resp.StatusCode,
		Status:        fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		Header:        http.Header(resp.Headers),
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}, nil
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return body, nil
}

// streamBody 按需从 host.http.stream_read 拉取上游分片。
type streamBody struct {
	ctx        context.Context
	callbackID string
	streamID   string

	mu       sync.Mutex
	pending  []byte
	drained  bool
	closed   bool
	closeErr error
}

func (b *streamBody) acceptBuffered(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, data...)
	b.streamID = ""
	b.drained = true
}

func (b *streamBody) isDrained() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drained
}

// emptyReadBackoff 是宿主返回空分片时的让出间隔，避免桥接异常时读取循环空转。
const emptyReadBackoff = time.Millisecond

func (b *streamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// 循环而非递归：宿主可能返回空分片，此时必须继续读，
	// 既不能向调用方返回 (0, nil)，也不能无限递归。
	for {
		b.mu.Lock()
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			b.mu.Unlock()
			return n, nil
		}
		if b.drained {
			err := b.closeErr
			b.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		streamID := b.streamID
		callbackID := b.callbackID
		ctx := b.ctx
		b.mu.Unlock()

		if errContext := contextErr(ctx); errContext != nil {
			_ = b.Close()
			return 0, errContext
		}
		caller := hostCaller()
		if caller == nil {
			return 0, fmt.Errorf("host http bridge is unavailable")
		}
		raw, errCall := caller(callbackID, pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID})
		if errCall != nil {
			b.markDrained(errCall)
			return 0, errCall
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			errDecode := fmt.Errorf("decode host http stream read: %w", errUnmarshal)
			b.markDrained(errDecode)
			return 0, errDecode
		}
		if chunk.Error != "" {
			errStream := fmt.Errorf("upstream stream error: %s", chunk.Error)
			b.markDrained(errStream)
			return 0, errStream
		}
		b.mu.Lock()
		b.pending = append(b.pending, chunk.Payload...)
		if chunk.Done {
			b.drained = true
			b.streamID = ""
		}
		b.mu.Unlock()

		if len(chunk.Payload) == 0 && !chunk.Done {
			// 宿主暂时没有数据：主动让出 CPU 再继续读，避免空转。
			time.Sleep(emptyReadBackoff)
		}
	}
}

// mergeContexts 返回一个在 primary 或 secondary 任一结束时即结束的上下文。
func mergeContexts(primary, secondary context.Context) context.Context {
	if primary == nil {
		return secondary
	}
	if secondary == nil || primary == secondary {
		return primary
	}
	ctx, cancel := context.WithCancel(primary)
	stopPrimary := context.AfterFunc(primary, cancel)
	stopSecondary := context.AfterFunc(secondary, cancel)
	// ctx 结束时（正常读完或调用方取消）回收两个监听，避免残留。
	context.AfterFunc(ctx, func() {
		stopPrimary()
		stopSecondary()
	})
	return ctx
}

func (b *streamBody) markDrained(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drained = true
	b.streamID = ""
	if err != nil {
		b.closeErr = err
	}
}

// Close 关闭宿主侧的上游流（幂等：只会调用一次 host.http.stream_close）。
func (b *streamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		err := b.closeErr
		b.mu.Unlock()
		return err
	}
	b.closed = true
	streamID := b.streamID
	callbackID := b.callbackID
	b.streamID = ""
	b.mu.Unlock()

	if streamID == "" {
		return nil
	}
	caller := hostCaller()
	if caller == nil {
		return nil
	}
	if _, errCall := caller(callbackID, pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID}); errCall != nil {
		b.mu.Lock()
		b.closeErr = errCall
		b.mu.Unlock()
		return errCall
	}
	return nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// ---- 宿主 HTTP 桥的 JSON 结构（与宿主 internal/pluginhost 对齐）----

type hostHTTPRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type rpcHostHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`

	// statusMissing 记录宿主没给出状态码（协议不匹配或旧版本宿主的信号）。
	statusMissing bool
}

// UnmarshalJSON 兼容宿主的两种序列化形态。
//
// 宿主 `host.http.do` 直接把 pluginapi.HTTPResponse 丢进 RPC 信封，而该结构体**没有 json tag**，
// 因此线上键名是 Go 字段名（StatusCode / Headers / Body）；宿主少数地方又会用带 tag 的
// 中间结构体（status_code / headers / body）。只认一种的话状态码会静默变成 0，
// 后果是所有非流式上游请求（模型清单、额度、签到、非流式对话）全部被判成失败——
// 而错误体里带着上游真实响应，看起来很像“凭证被拒”，极难排查。
func (r *rpcHostHTTPResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return fmt.Errorf("nil host http response")
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &fields); errUnmarshal != nil {
		return errUnmarshal
	}
	for key, value := range fields {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "statuscode":
			r.statusMissing = false
			if errDecode := json.Unmarshal(value, &r.StatusCode); errDecode != nil {
				return fmt.Errorf("decode host http status: %w", errDecode)
			}
		case "headers":
			if errDecode := json.Unmarshal(value, &r.Headers); errDecode != nil {
				return fmt.Errorf("decode host http headers: %w", errDecode)
			}
		case "body":
			if errDecode := json.Unmarshal(value, &r.Body); errDecode != nil {
				return fmt.Errorf("decode host http body: %w", errDecode)
			}
		}
	}
	return nil
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int                  `json:"status_code"`
	Headers    http.Header          `json:"headers,omitempty"`
	StreamID   string               `json:"stream_id,omitempty"`
	Chunks     []rpcHostStreamChunk `json:"chunks,omitempty"`
}

type rpcHostStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// truncateForError 在报错里只放一小段原始报文，避免把上游响应体整段灌进日志。
func truncateForError(raw []byte) string {
	const limit = 200
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}
