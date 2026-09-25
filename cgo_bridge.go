package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static void clear_host_api(void) {
	stored_host = NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"qoder2api-plugin/cpasdk/pluginabi"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelopeFromError(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// 顺序不可调换：
	//  1. 停后台循环（签到调度、模型刷新），让它们不再发起新的宿主调用；
	//  2. 取消并等待在途流式转发，使其在关闭前完成 host.stream.close；
	//  3. 关闭宿主调用准入并等待已进入的 CGO 调用返回；
	//  4. 清空 host api。
	// 宿主在 Shutdown 之后会 dlclose 本动态库：任何仍在执行的 Go/CGO 栈都会崩溃，
	// 因此第 3 步必须等待排空（见 hostgate.go 的说明）。
	stopBackgroundWork()
	waitPluginStreams(shutdownWaitTimeout)
	waitHostCallsForShutdown(shutdownDiagnosticInterval)
	C.clear_host_api()
}

// callHost 发起宿主回调（无请求作用域）。
func callHost(method string, payload any) (json.RawMessage, error) {
	return callHostScoped("", method, payload)
}

// callHostScoped 发起宿主回调（可测试入口）。
//
// 真实实现走 CGO 调用宿主（nativeHostCallScoped）；单元测试通过替换
// hostCallScopedImpl 注入假宿主，从而在没有任何 CPA 进程的情况下验证
// 出站 HTTP、签到、账号枚举等完整链路。
func callHostScoped(callbackID, method string, payload any) (json.RawMessage, error) {
	return hostCallScopedImpl(callbackID, method, payload)
}

// nativeHostCallScoped 发起宿主回调，并在载荷里带上 host_callback_id。
//
// host_callback_id 把宿主调用绑定到具体的请求作用域：流式转发（host.stream.emit）
// 与出站 HTTP（host.http.do_stream）都必须携带它，否则宿主无法把上游请求/响应
// 关联回原始请求，也无法在请求结束时回收流资源。
func nativeHostCallScoped(callbackID string, method string, payload any) (json.RawMessage, error) {
	// 整个 CGO 调用期间持有准入槽位：宿主回调不可取消，超时返回的调用仍会占用
	// 一个 OS 线程直到宿主返回，因此必须限制并发上限，避免挂起时线程爆炸。
	if err := tryAcquireHostCall(); err != nil {
		return nil, err
	}
	defer releaseHostCall()

	rawPayload, errMarshal := marshalHostPayload(callbackID, payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("%s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("%s: allocate host callback payload", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("%s: host callback returned no response, code=%d", method, int(callCode))
	}
	result, errDecode := decodeEnvelopeResult(rawResponse)
	if errDecode != nil {
		return nil, fmt.Errorf("%s: %w", method, errDecode)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("%s: host callback returned code=%d", method, int(callCode))
	}
	return result, nil
}

// marshalHostPayload 把载荷编码为 JSON，并按需注入 host_callback_id。
func marshalHostPayload(callbackID string, payload any) ([]byte, error) {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload: %w", errMarshal)
	}
	if callbackID == "" {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host callback payload: %w", errUnmarshal)
		}
	}
	encodedID, _ := json.Marshal(callbackID)
	fields["host_callback_id"] = encodedID
	return json.Marshal(fields)
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
