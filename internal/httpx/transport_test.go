package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"qoder2api-plugin/cpasdk/pluginabi"
)

// fakeTransportHost 记录宿主调用并按脚本应答，用来验证这一层的关键语义：
// 请求体/头部确实传给了宿主、分片逐次读取、出错传播、关闭恰好一次。
type fakeTransportHost struct {
	mu        sync.Mutex
	calls     []string
	lastBody  []byte
	lastURL   string
	lastHdr   http.Header
	chunks    []string
	readDone  bool
	closedIDs []string
	failEmit  error
}

func (f *fakeTransportHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()

	switch method {
	case pluginabi.MethodHostHTTPDo:
		var req struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
			Body    []byte      `json:"body"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.lastBody, f.lastURL, f.lastHdr = req.Body, req.URL, req.Headers
		f.mu.Unlock()
		// 按宿主的真实线上形态应答：宿主直接把 pluginapi.HTTPResponse（无 json tag）
		// 丢进 RPC 信封，所以键名是 Go 字段名。旧 mock 用的是 status_code，
		// 结果真机上状态码静默变成 0、所有非流式上游请求全被判成失败。
		return marshal(map[string]any{
			"StatusCode": 201,
			"Headers":    http.Header{"Content-Type": []string{"application/json"}},
			"Body":       []byte(`{"ok":true}`),
		})

	case pluginabi.MethodHostHTTPDoStream:
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Body   []byte `json:"body"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.lastBody, f.lastURL = req.Body, req.URL
		f.mu.Unlock()
		return marshal(map[string]any{"status_code": 200, "headers": http.Header{"Content-Type": []string{"text/event-stream"}}, "stream_id": "s-1"})

	case pluginabi.MethodHostHTTPStreamRead:
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.chunks) == 0 {
			if f.readDone {
				return marshal(map[string]any{"done": true})
			}
			return marshal(map[string]any{})
		}
		chunk := f.chunks[0]
		f.chunks = f.chunks[1:]
		done := len(f.chunks) == 0
		return marshal(map[string]any{"payload": []byte(chunk), "done": done})

	case pluginabi.MethodHostHTTPStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.closedIDs = append(f.closedIDs, req.StreamID)
		f.mu.Unlock()
		return marshal(map[string]any{})

	default:
		return nil, errors.New("unexpected host method " + method)
	}
}

func (f *fakeTransportHost) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == method {
			count++
		}
	}
	return count
}

func (f *fakeTransportHost) closedStreamIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closedIDs...)
}

func marshal(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

func installFake(t *testing.T, host *fakeTransportHost) {
	t.Helper()
	previous := hostCaller()
	Configure(host.call)
	t.Cleanup(func() { Configure(previous) })
}

func TestClientUsesHostBridge(t *testing.T) {
	host := &fakeTransportHost{}
	installFake(t, host)

	req, errRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/api", strings.NewReader(`{"hello":"world"}`))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	req.Header.Set("X-Test", "1")
	resp, errDo := Client(context.Background(), time.Second).Do(req)
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(body) != `{"ok":true}` {
		t.Fatalf("response = %d %s", resp.StatusCode, body)
	}
	if host.lastURL != "https://example.invalid/api" {
		t.Fatalf("host saw url %q", host.lastURL)
	}
	if string(host.lastBody) != `{"hello":"world"}` {
		t.Fatalf("host saw body %q", host.lastBody)
	}
	if host.lastHdr.Get("X-Test") != "1" {
		t.Fatalf("host saw headers %v", host.lastHdr)
	}
}

func TestStreamClientReadsChunksAndClosesOnce(t *testing.T) {
	host := &fakeTransportHost{chunks: []string{"data: one\n\n", "data: two\n\n"}, readDone: true}
	installFake(t, host)

	ctx := WithCallbackID(context.Background(), "cb-1")
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/stream", strings.NewReader(`{}`))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	resp, errDo := StreamClient(ctx, 0).Do(req)
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("read: %v", errRead)
	}
	if !strings.Contains(string(body), "data: one") || !strings.Contains(string(body), "data: two") {
		t.Fatalf("body = %q", body)
	}
	// 读到 EOF 表示上游已自然结束，宿主侧无需再收到 stream_close。
	if count := host.callCount(pluginabi.MethodHostHTTPStreamClose); count != 0 {
		t.Fatalf("drained stream should not be closed again, calls = %d", count)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
	// 再次关闭不应重复调用宿主（幂等）。
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("second close: %v", errClose)
	}
	if host.callCount(pluginabi.MethodHostHTTPStreamRead) < 2 {
		t.Fatalf("expected chunked reads, calls=%v", host.calls)
	}
}

// TestStreamClientClosesHostStreamOnEarlyClose 验证提前关闭时插件会通知宿主回收上游流，且只通知一次。
func TestStreamClientClosesHostStreamOnEarlyClose(t *testing.T) {
	host := &fakeTransportHost{chunks: []string{"data: one\n\n", "data: two\n\n"}, readDone: true}
	installFake(t, host)

	ctx := WithCallbackID(context.Background(), "cb-early")
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/stream", strings.NewReader(`{}`))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	resp, errDo := StreamClient(ctx, 0).Do(req)
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	// 只读一小段就关闭：流未读完，必须显式通知宿主。
	header := make([]byte, 4)
	if _, errRead := io.ReadFull(resp.Body, header); errRead != nil {
		t.Fatalf("read: %v", errRead)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("second close: %v", errClose)
	}
	if count := host.callCount(pluginabi.MethodHostHTTPStreamClose); count != 1 {
		t.Fatalf("stream close called %d times, want 1", count)
	}
	if ids := host.closedStreamIDs(); len(ids) != 1 || ids[0] != "s-1" {
		t.Fatalf("closed stream ids = %v", ids)
	}
}

func TestStreamClientPropagatesStreamError(t *testing.T) {
	host := &fakeTransportHost{}
	installFake(t, host)
	previous := host.call
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostHTTPStreamRead {
			return marshal(map[string]any{"error": "upstream reset"})
		}
		return previous(callbackID, method, payload)
	})

	resp, errDo := StreamClient(WithCallbackID(context.Background(), "cb-2"), 0).Do(mustRequest(t, "https://example.invalid/stream"))
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	_, errRead := io.ReadAll(resp.Body)
	if errRead == nil || !strings.Contains(errRead.Error(), "upstream reset") {
		t.Fatalf("stream error not propagated: %v", errRead)
	}
	_ = resp.Body.Close()
}

func TestStreamClientRespectsClientContextCancellation(t *testing.T) {
	host := &fakeTransportHost{}
	installFake(t, host)

	// 请求本身没带 ctx 时，StreamClient 的 ctx 也必须能中断读取。
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithCallbackID(ctx, "cb-3")
	resp, errDo := StreamClient(ctx, 0).Do(mustRequest(t, "https://example.invalid/stream"))
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	cancel()
	if _, errRead := io.ReadAll(resp.Body); errRead == nil {
		t.Fatal("cancellation should surface as a read error")
	}
}

func TestStreamClientRespectsRequestContextCancellation(t *testing.T) {
	host := &fakeTransportHost{}
	installFake(t, host)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithCallbackID(ctx, "cb-4")
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/stream", strings.NewReader(`{}`))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	resp, errDo := StreamClient(context.Background(), 0).Do(req)
	if errDo != nil {
		t.Fatalf("do: %v", errDo)
	}
	cancel()
	if _, errRead := io.ReadAll(resp.Body); errRead == nil {
		t.Fatal("request cancellation should surface as a read error")
	}
}

func TestClientFailsWithoutHostBridge(t *testing.T) {
	previous := hostCaller()
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		return nil, errors.New("host http.do failed")
	})
	t.Cleanup(func() { Configure(previous) })

	if _, errDo := Client(context.Background(), time.Second).Do(mustRequest(t, "https://example.invalid/x")); errDo == nil {
		t.Fatal("host failure must surface to the caller")
	}
}

func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, errRequest := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	if errRequest != nil {
		t.Fatalf("new request: %v", errRequest)
	}
	return req
}

// TestClientAcceptsBothHostWireShapes 固定 host.http.do 的线协议兼容性。
//
// 真实宿主（pluginapi.HTTPResponse 无 json tag）发的是 Go 字段名；
// 少数带 tag 的中间结构体发 snake_case。两种都必须解出同样的状态码——
// 只认一种的话状态码会静默变 0，上游 200 也会被判成失败。
func TestClientAcceptsBothHostWireShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"go field names (real host)", `{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"eyJvayI6dHJ1ZX0="}`},
		{"snake case (tagged shape)", `{"status_code":200,"headers":{"Content-Type":["application/json"]},"body":"eyJvayI6dHJ1ZX0="}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := hostCaller()
			Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
				return json.RawMessage(tc.payload), nil
			})
			t.Cleanup(func() { Configure(previous) })

			resp, errDo := Client(context.Background(), time.Second).Do(mustRequest(t, "https://example.invalid/api"))
			if errDo != nil {
				t.Fatalf("do: %v", errDo)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != `{"ok":true}` {
				t.Fatalf("body = %q", body)
			}
		})
	}
}

// TestClientFailsLoudlyWhenHostOmitsStatus 确认协议不匹配时直接报错，而不是当成状态 0。
func TestClientFailsLoudlyWhenHostOmitsStatus(t *testing.T) {
	previous := hostCaller()
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		return json.RawMessage(`{"Body":"aGk="}`), nil
	})
	t.Cleanup(func() { Configure(previous) })

	_, errDo := Client(context.Background(), time.Second).Do(mustRequest(t, "https://example.invalid/api"))
	if errDo == nil {
		t.Fatal("missing status field must surface as an error, not status 0")
	}
	if !strings.Contains(errDo.Error(), "no status field") {
		t.Fatalf("error should name the ABI mismatch: %v", errDo)
	}
}
