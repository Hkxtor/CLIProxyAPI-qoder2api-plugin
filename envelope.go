package main

import (
	"encoding/json"
	"fmt"
)

// envelope 是 CPA 插件 ABI 的 JSON 信封。与宿主 sdk/pluginabi.Envelope 对齐。
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError 携带可选 HTTP 状态码；宿主会把它映射为返回给客户端的状态码。
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// pluginError 是插件内部的结构化错误：可携带 HTTP 状态码与可重试标记。
type pluginError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

func (e *pluginError) Error() string { return e.Message }

func newPluginError(code, message string, status int) *pluginError {
	return &pluginError{Code: code, Message: message, HTTPStatus: status}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// errorEnvelopeFromError 把业务错误转成信封，保留 HTTP 状态码（流式/非流式的错误映射依赖它）。
func errorEnvelopeFromError(err error) []byte {
	if err == nil {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "plugin_error", Message: "unknown error"}})
		return raw
	}
	if pe, ok := err.(*pluginError); ok {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code:       pe.Code,
			Message:    pe.Message,
			Retryable:  pe.Retryable,
			HTTPStatus: pe.HTTPStatus,
		}})
		return raw
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "plugin_error", Message: err.Error()}})
	return raw
}

func decodeEnvelopeResult(raw []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback failed")
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
