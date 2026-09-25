package main

import (
	"errors"
	"sync"
	"time"

	"qoder2api-plugin/internal/logger"
)

// 宿主回调（host.*）在当前 CPA ABI 下不可取消：一旦发起，就必须等宿主返回。
// 因此本文件提供两件事：
//  1. 并发准入 —— 限制同时在飞的 CGO 宿主调用数量，避免上游挂起时线程无限增长；
//  2. 关闭序 —— Shutdown 时先禁止新的宿主调用，再等待在飞调用排空，最后才允许宿主
//     dlclose 本动态库（否则会在仍然活跃的 Go/CGO 栈上卸载，导致进程崩溃）。

const (
	// maxConcurrentHostCalls 是在飞宿主调用的上限。流式转发会同时使用
	// host.http.stream_read 与 host.stream.emit，因此不能设得过小。
	maxConcurrentHostCalls = 64

	shutdownDiagnosticInterval = 5 * time.Second
	// shutdownWaitTimeout 只用于等待本插件自己的流式 goroutine；
	// 它们可以被取消，因此可以有界等待。宿主调用排空是无界等待。
	shutdownWaitTimeout = 10 * time.Second
)

// errHostCallsShuttingDown 表示插件已进入关闭序，不再接受新的宿主调用。
var errHostCallsShuttingDown = errors.New("host callbacks unavailable: plugin is shutting down")

var (
	hostCallMu           sync.Mutex
	hostCallShuttingDown bool
	hostCallGate         = make(chan struct{}, maxConcurrentHostCalls)
	hostCallWG           sync.WaitGroup
)

// tryAcquireHostCall 申请一个宿主调用槽位。
// 准入检查与 WaitGroup.Add 必须在同一把锁内完成，否则 Wait 可能观察到零计数。
func tryAcquireHostCall() error {
	hostCallMu.Lock()
	if hostCallShuttingDown {
		hostCallMu.Unlock()
		return errHostCallsShuttingDown
	}
	hostCallWG.Add(1)
	hostCallMu.Unlock()
	hostCallGate <- struct{}{}
	return nil
}

func releaseHostCall() {
	<-hostCallGate
	hostCallWG.Done()
}

func hostCallInflight() int { return len(hostCallGate) }

func closeHostCallAdmission() {
	hostCallMu.Lock()
	hostCallShuttingDown = true
	hostCallMu.Unlock()
}

// waitHostCallsForShutdown 关闭准入并等待所有在飞宿主调用返回。
//
// 这里刻意不做有界等待：宿主回调不可取消，提前返回会在仍然执行的 Go/CGO 栈上
// 触发 dlclose。等待期间每 5 秒输出一次诊断，便于定位卡住的上游。
func waitHostCallsForShutdown(diagEvery time.Duration) {
	closeHostCallAdmission()
	if diagEvery <= 0 {
		diagEvery = shutdownDiagnosticInterval
	}
	if hostCallInflight() == 0 {
		hostCallWG.Wait()
		return
	}
	done := make(chan struct{})
	go func() {
		hostCallWG.Wait()
		close(done)
	}()
	ticker := time.NewTicker(diagEvery)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logger.Error("shutdown: waiting for in-flight host callbacks inflight=%d waited=%s (host.http.do is not cancelable)",
				hostCallInflight(), time.Since(start).Round(time.Millisecond))
		}
	}
}
