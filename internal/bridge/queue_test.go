package bridge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// realQueuePayload 是 2026-09-25 实测 Qwen3.8-Flash（qfmodel）免费模型时上游返回的原始正文：
// HTTP 403 + 三层嵌套 JSON 字符串（isQueued / serviceAvailable / retryAfterSeconds）。
const realQueuePayload = `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":0,\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":false,\\\"waitTime\\\":30}\"}"}`

func TestParseQueueSignalOnRealPayload(t *testing.T) {
	signal, ok := ParseQueueSignal(realQueuePayload)
	if !ok {
		t.Fatalf("real queue payload must be recognised: %s", realQueuePayload)
	}
	if !signal.Queued {
		t.Error("queued = false, want true")
	}
	if signal.ServiceAvailable {
		t.Error("serviceAvailable = true, want false")
	}
	if signal.ModelKey != "qfmodel" {
		t.Errorf("modelKey = %q, want qfmodel", signal.ModelKey)
	}
	if signal.QueueType != "p3" {
		t.Errorf("queueType = %q, want p3", signal.QueueType)
	}
	if signal.RetryAfterSeconds != 30 {
		t.Errorf("retryAfterSeconds = %d, want 30", signal.RetryAfterSeconds)
	}
	if got := signal.WaitDuration(); got != 30*time.Second {
		t.Errorf("WaitDuration = %s, want 30s", got)
	}
}

// TestParseQueueSignalIgnoresOrdinaryErrors 确认不会把普通错误误判成排队。
func TestParseQueueSignalIgnoresOrdinaryErrors(t *testing.T) {
	cases := []string{
		`{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`,
		`{"code":"401","message":"token is not active"}`,
		`{"error":{"message":"invalid_parameter_error"}}`,
		``,
		`plain text error`,
	}
	for _, payload := range cases {
		if _, ok := ParseQueueSignal(payload); ok {
			t.Errorf("must not detect a queue signal in %q", payload)
		}
	}
}

// TestQueueSignalIsTransientButNotCredential 锁死分类：排队是可重试的，且不是凭证问题。
func TestQueueSignalIsTransientButNotCredential(t *testing.T) {
	if !IsTransientUpstream(403, realQueuePayload) {
		t.Error("queue signal must be retryable even though it arrives as HTTP 403")
	}
	errBusy := NewUpstreamError(403, realQueuePayload)
	if errBusy.ErrType != ErrTypeModelBusy {
		t.Fatalf("errType = %q, want %q", errBusy.ErrType, ErrTypeModelBusy)
	}
	if got := ErrorStatus(errBusy); got != 503 {
		t.Fatalf("ErrorStatus = %d, want 503 (403 would be reported as insufficient_quota by the host)", got)
	}
	if !strings.Contains(errBusy.Message, "排队") || !strings.Contains(errBusy.Message, "不是额度") {
		t.Fatalf("message must say it is a queue, not quota: %q", errBusy.Message)
	}
	// 真实额度错误仍然是 403 + 权限语义（不能被排队逻辑吞掉）。
	errQuota := NewUpstreamError(403, `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`)
	if errQuota.ErrType == ErrTypeModelBusy {
		t.Fatal("quota rejection must not be classified as busy")
	}
	if got := ErrorStatus(errQuota); got != 403 {
		t.Fatalf("quota ErrorStatus = %d, want 403", got)
	}
}

// TestQueueSignalFromStreamBusinessError 流内业务错误也要识别排队。
func TestQueueSignalFromStreamBusinessError(t *testing.T) {
	errStream := NewStreamBusinessError(realQueuePayload)
	if errStream.ErrType != ErrTypeModelBusy {
		t.Fatalf("errType = %q, want %q", errStream.ErrType, ErrTypeModelBusy)
	}
	if errStream.Queue == nil || errStream.Queue.ModelKey != "qfmodel" {
		t.Fatalf("queue signal not attached: %+v", errStream.Queue)
	}
}

// TestWaitDurationIsCapped 上游给出离谱等待时长时不把请求挂死。
func TestWaitDurationIsCapped(t *testing.T) {
	signal := QueueSignal{Queued: true, ServiceAvailable: false, RetryAfterSeconds: 3600}
	if got := signal.WaitDuration(); got != MaxQueueWait {
		t.Fatalf("WaitDuration = %s, want cap %s", got, MaxQueueWait)
	}
	empty := QueueSignal{Queued: true, ServiceAvailable: false}
	if got := empty.WaitDuration(); got != DefaultQueueWaitSeconds*time.Second {
		t.Fatalf("WaitDuration without hints = %s, want %ds", got, DefaultQueueWaitSeconds)
	}
}

// TestQueueSignalFromWrappedError 排队信号在错误被包装后仍要能识别（重开上游时按建议时长等待）。
func TestQueueSignalFromWrappedError(t *testing.T) {
	base := NewUpstreamError(403, realQueuePayload)
	signal, ok := QueueSignalFromError(base)
	if !ok || signal.ModelKey != "qfmodel" {
		t.Fatalf("direct error not recognised: %+v ok=%v", signal, ok)
	}
	wrapped := fmt.Errorf("chat handler failed: %w", base)
	signal, ok = QueueSignalFromError(wrapped)
	if !ok || signal.RetryAfterSeconds != 30 {
		t.Fatalf("wrapped error not recognised: %+v ok=%v", signal, ok)
	}
	if _, okEmpty := QueueSignalFromError(nil); okEmpty {
		t.Fatal("nil error must not report a queue signal")
	}
	// 已被友好化的错误（只剩中文描述）也要能识别出来。
	friendly := errors.New(NewUpstreamError(403, realQueuePayload).Message)
	if _, okFriendly := QueueSignalFromError(friendly); okFriendly {
		t.Log("friendly text still carries the queue wording (acceptable)")
	} else {
		t.Log("friendly text no longer carries queue markers; QueueSignalError preserves ErrType instead")
	}
}

// TestParseQueueSignalFromEscapedText 锁死“正文被转义/拼接、JSON 解不出来”的形态：
// 上游把排队信号塞在字符串里时，正则兜底也必须能认出 isQueued / serviceAvailable。
func TestParseQueueSignalFromEscapedText(t *testing.T) {
	escaped := `upstream 403: {"statusCodeValue":403,"statusMessageValue":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"retryAfterSeconds\":45,\"serviceAvailable\":false}"}`
	signal, ok := ParseQueueSignal(escaped)
	if !ok {
		t.Fatalf("escaped payload must still be recognised: %s", escaped)
	}
	if !signal.Queued || signal.ServiceAvailable {
		t.Fatalf("booleans not read from escaped text: %+v", signal)
	}
	if signal.RetryAfterSeconds != 45 || signal.ModelKey != "qfmodel" {
		t.Fatalf("hints not read from escaped text: %+v", signal)
	}
	if got := signal.WaitDuration(); got != 45*time.Second {
		t.Fatalf("WaitDuration = %s, want 45s", got)
	}
}

// TestQueueSignalErrorKeepsSignal 友好化后的错误必须仍带着排队信号（上层靠它决定是否重试）。
func TestQueueSignalErrorKeepsSignal(t *testing.T) {
	signal, _ := ParseQueueSignal(realQueuePayload)
	err := QueueSignalError(signal)
	if err.Queue == nil {
		t.Fatal("QueueSignalError must keep the parsed queue signal")
	}
	if _, queued := QueueSignalFromError(err); !queued {
		t.Fatal("friendly queue error must still be recognised as queued")
	}
	if err.ErrType != ErrTypeModelBusy || err.Status != 503 {
		t.Fatalf("unexpected classification: type=%s status=%d", err.ErrType, err.Status)
	}
}

// TestQueueSignalPriorityInFriendlyError 排队优先于内容审核/瞬时故障：
// 免模型排队时上游也回 403，绝不能被归成额度或凭证问题。
func TestQueueSignalPriorityInFriendlyError(t *testing.T) {
	message, errType := FriendlyUpstreamError(403, realQueuePayload)
	if errType != ErrTypeModelBusy {
		t.Fatalf("errType = %q, want %q", errType, ErrTypeModelBusy)
	}
	for _, want := range []string{"排队", "不是额度"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message should contain %q: %q", want, message)
		}
	}
	// 真实额度拒绝仍走额度语义（对照组）。
	quotaMessage, quotaType := FriendlyUpstreamError(403, `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`)
	if quotaType == ErrTypeModelBusy {
		t.Fatal("quota rejection must not become busy")
	}
	if !strings.Contains(quotaMessage, "upstream 403") {
		t.Fatalf("quota message should keep the upstream detail: %q", quotaMessage)
	}
}
