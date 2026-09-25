package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// 信封转换是插件与宿主之间的唯一契约：错误必须带 http_status，
// 否则 CPA 只会看到 500，也就不会按 401/429 处理凭证与限流。

func TestErrorEnvelopeFromPluginErrorCarriesStatus(t *testing.T) {
	raw := errorEnvelopeFromError(newPluginError("qoder_credential_invalid", "token expired", 401))
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if env.OK {
		t.Fatal("error envelope must have ok=false")
	}
	if env.Error.Code != "qoder_credential_invalid" || env.Error.HTTPStatus != 401 {
		t.Fatalf("envelope = %s", raw)
	}
}

func TestErrorEnvelopeFromPlainError(t *testing.T) {
	raw := errorEnvelopeFromError(errors.New("boom"))
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error.Code != "plugin_error" || env.Error.Message != "boom" {
		t.Fatalf("envelope = %s", raw)
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("plain error should not invent an http status: %s", raw)
	}
}

func TestErrorEnvelopeFromNilError(t *testing.T) {
	raw := errorEnvelopeFromError(nil)
	if len(raw) == 0 {
		t.Fatal("nil error must still produce a valid envelope")
	}
	var env map[string]any
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env["ok"] != false {
		t.Fatalf("envelope = %s", raw)
	}
}

// TestDecodeStringRequestRejectsBadJSON 保证畸形请求不会以"零值请求"继续执行
// （否则会拿空凭证去建会话，产生难以定位的上游错误）。
func TestDecodeStringRequestRejectsBadJSON(t *testing.T) {
	var target executorRPCRequest
	errDecode := decodeStringRequest([]byte("{not json"), &target)
	if errDecode == nil {
		t.Fatal("decodeStringRequest should fail on malformed JSON")
	}
	if !errors.Is(fmt.Errorf("%w", errDecode), errDecode) {
		t.Fatalf("unexpected error: %v", errDecode)
	}
	if errEmpty := decodeStringRequest(nil, &target); errEmpty != nil {
		t.Fatalf("empty request should be allowed: %v", errEmpty)
	}
}
