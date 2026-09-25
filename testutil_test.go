package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"qoder2api-plugin/cpasdk/pluginabi"
	"qoder2api-plugin/cpasdk/pluginapi"
	"qoder2api-plugin/internal/bridge"
)

// 本文件提供"假宿主"：把 CGO 宿主回调替换成内存实现，
// 使插件的完整链路（auth 解析、模型注册、出站 HTTP、签到、流式转发）
// 都能在没有 CPA 进程的情况下被测试。

const testHostCallbackID = "test-callback"

// fakeUpstreamResponse 是假宿主对一次出站请求的应答。
type fakeUpstreamResponse struct {
	Status int
	Header map[string][]string
	Body   string
}

// fakeStream 是一次被模拟的流式响应。
type fakeStream struct {
	chunks   []string
	position int
	closed   bool
}

// fakeHost 是宿主回调的内存实现。
type fakeHost struct {
	mu sync.Mutex

	calls     []string
	authFiles []pluginapi.HostAuthFileEntry
	authJSON  map[string]string
	upstream  func(method, url, body string) fakeUpstreamResponse

	streams    map[string]*fakeStream
	nextStream int

	emits      []string
	requested  []string
	streamErrs []string
	closed     []string
	closeErr   []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		authJSON: map[string]string{},
		streams:  map[string]*fakeStream{},
		upstream: func(method, url, body string) fakeUpstreamResponse {
			return fakeUpstreamResponse{Status: 404, Body: `{"error":"no fake upstream handler"}`}
		},
	}
}

// requestURLs 返回假上游收到过的请求 URL（顺序保留）。
func (f *fakeHost) requestURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

// recordCalls 返回被调用过的方法序列（用于断言链路是否按预期走宿主）。
func (f *fakeHost) recordCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// emittedChunks 返回通过 host.stream.emit 转发出去的分片。
func (f *fakeHost) emittedChunks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.emits...)
}

// streamCloses 返回 host.stream.close 的 (streamID, error) 列表。
func (f *fakeHost) streamCloses() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...), append([]string(nil), f.closeErr...)
}

func (f *fakeHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	var fields map[string]json.RawMessage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &fields)
	}

	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()

	switch method {
	case pluginabi.MethodHostAuthList:
		f.mu.Lock()
		files := append([]pluginapi.HostAuthFileEntry(nil), f.authFiles...)
		f.mu.Unlock()
		return marshalJSON(map[string]any{"files": files})

	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		body, ok := f.authJSON[req.AuthIndex]
		f.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("auth_index %s not found", req.AuthIndex)
		}
		return marshalJSON(map[string]any{"auth_index": req.AuthIndex, "name": req.AuthIndex + ".json", "json": json.RawMessage(body)})

	case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		f.mu.Lock()
		handler := f.upstream
		// 记录请求 URL：测试要断言请求打到了哪个域名（如签到的区域域名）。
		f.requested = append(f.requested, req.URL)
		f.mu.Unlock()
		if handler == nil {
			return nil, fmt.Errorf("fake upstream handler is not configured")
		}
		response := handler(req.Method, req.URL, string(req.Body))
		if method == pluginabi.MethodHostHTTPDo {
			// 与真实宿主同形：宿主 host.http.do 回的是无 json tag 的 pluginapi.HTTPResponse。
			return marshalJSON(map[string]any{
				"StatusCode": response.Status,
				"Headers":    response.Header,
				"Body":       []byte(response.Body),
			})
		}
		f.mu.Lock()
		f.nextStream++
		streamID := fmt.Sprintf("stream-%d", f.nextStream)
		f.streams[streamID] = &fakeStream{chunks: splitChunks(response.Body)}
		f.mu.Unlock()
		return marshalJSON(map[string]any{
			"status_code": response.Status,
			"headers":     response.Header,
			"stream_id":   streamID,
		})

	case pluginabi.MethodHostHTTPStreamRead:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		stream := f.streams[req.StreamID]
		if stream == nil || stream.closed {
			f.mu.Unlock()
			return nil, fmt.Errorf("http stream %s is not open", req.StreamID)
		}
		if stream.position >= len(stream.chunks) {
			f.mu.Unlock()
			return marshalJSON(map[string]any{"done": true})
		}
		chunk := stream.chunks[stream.position]
		stream.position++
		done := stream.position >= len(stream.chunks)
		f.mu.Unlock()
		return marshalJSON(map[string]any{"payload": []byte(chunk), "done": done})

	case pluginabi.MethodHostHTTPStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		if stream := f.streams[req.StreamID]; stream != nil {
			stream.closed = true
		}
		f.mu.Unlock()
		return marshalJSON(map[string]any{})

	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(raw, &req)
		if strings.TrimSpace(req.StreamID) == "" {
			return nil, fmt.Errorf("stream id is required")
		}
		f.mu.Lock()
		if req.Error != "" {
			f.streamErrs = append(f.streamErrs, req.Error)
		} else {
			f.emits = append(f.emits, string(req.Payload))
		}
		f.mu.Unlock()
		return marshalJSON(map[string]any{})

	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.closed = append(f.closed, req.StreamID)
		f.closeErr = append(f.closeErr, req.Error)
		f.mu.Unlock()
		return marshalJSON(map[string]any{})

	default:
		return nil, fmt.Errorf("fake host does not implement %s", method)
	}
}

// splitChunks 把响应体切成小块，模拟宿主 32KB 分片读取的真实路径
// （而不是一次性喂完整正文，避免掩盖分片边界相关的 bug）。
func splitChunks(body string) []string {
	if body == "" {
		return nil
	}
	const size = 17
	chunks := make([]string, 0, len(body)/size+1)
	for len(body) > 0 {
		take := size
		if take > len(body) {
			take = len(body)
		}
		chunks = append(chunks, body[:take])
		body = body[take:]
	}
	return chunks
}

func marshalJSON(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

// installFakeHost 把假宿主接到插件的宿主调用入口上，并把上游重试退避压到毫秒级。
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	previous := hostCallScopedImpl
	hostCallScopedImpl = host.call

	previousBackoff := bridge.RetryBackoff
	bridge.RetryBackoff = func(int) time.Duration { return time.Millisecond }

	resetPluginGlobals(t)
	t.Cleanup(func() {
		hostCallScopedImpl = previous
		bridge.RetryBackoff = previousBackoff
		resetPluginGlobals(t)
	})
	return host
}

// resetPluginGlobals 清掉跨用例共享的进程级状态。
func resetPluginGlobals(t *testing.T) {
	t.Helper()
	bridgeCacheMu.Lock()
	bridgeCache = map[string]*bridgeCacheEntry{}
	bridgeCreates = map[string]*bridgeCreateCall{}
	bridgeCacheMu.Unlock()

	stateMu.Lock()
	stateCache = nil
	stateMu.Unlock()

	pluginLifecycleMu.Lock()
	pluginRegistered = false
	pluginLifecycleMu.Unlock()

	hostCallMu.Lock()
	hostCallShuttingDown = false
	hostCallMu.Unlock()

	// 凭证缓存是包级全局：不重置会让“应该回源查找”的测试看到上一个测试留下的缓存。
	credentialCacheMu.Lock()
	credentialCache = map[string]credentialCacheEntry{}
	credentialCacheMu.Unlock()
}

// setupTestPlugin 准备一个使用临时状态目录的插件实例。
func setupTestPlugin(t *testing.T, mutate ...func(*pluginConfig)) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.LogToFile = false
	for _, apply := range mutate {
		apply(&cfg)
	}
	if errConfig := decodeAndApply(t, cfg); errConfig != nil {
		t.Fatalf("apply config: %v", errConfig)
	}
	return cfg
}

func decodeAndApply(t *testing.T, cfg pluginConfig) error {
	t.Helper()
	if errApply := applyConfig(cfg); errApply != nil {
		return errApply
	}
	if _, errState := loadState(cfg); errState != nil {
		return errState
	}
	applyModelMappings(cfg)
	return nil
}

// buildConfigYAML 生成宿主会传给 plugin.register 的配置 YAML（含 enabled/priority）。
func buildConfigYAML(cfg pluginConfig) []byte {
	builder := &strings.Builder{}
	fmt.Fprintf(builder, "enabled: true\npriority: 1\nregion: %s\nstate_dir: %q\nmodel_prefix: %q\nlog_level: %s\n",
		cfg.Region, cfg.StateDir, cfg.ModelPrefix, cfg.LogLevel)
	if cfg.AutoCheckin {
		builder.WriteString("auto_checkin: true\n")
	}
	fmt.Fprintf(builder, "auto_checkin_at: %q\n", cfg.AutoCheckinAt)
	return []byte(builder.String())
}

// newTestContext 返回一个带超时的测试上下文（管理接口/刷新路径用）。
func newTestContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = cancel
	return ctx
}

// sleepShort 在测试里做短等待（异步转发投递需要调度机会）。
func sleepShort() { time.Sleep(5 * time.Millisecond) }

// base64Body 帮助测试构造 host.http.do 的响应体。
func base64Body(body string) string {
	return base64.StdEncoding.EncodeToString([]byte(body))
}
