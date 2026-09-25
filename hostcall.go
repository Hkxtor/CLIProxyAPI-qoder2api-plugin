package main

// hostCallScopedImpl 指向真实的 CGO 宿主调用；单元测试替换它来注入假宿主，
// 从而在没有任何 CPA 进程参与的情况下验证出站 HTTP、签到、账号枚举等链路。
var hostCallScopedImpl = nativeHostCallScoped
