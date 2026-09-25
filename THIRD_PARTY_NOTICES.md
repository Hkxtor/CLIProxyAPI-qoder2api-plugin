# 第三方来源与许可说明

本插件（`qoder2api-plugin`）是把 [qoder2api](https://github.com/Zhengyuuuui/qoder2api) 的能力
打包成 CLIProxyAPI 原生动态库插件。它包含从其它项目移植/改造的代码，**不是**从头重写。

## 1. qoder2api（上游业务实现）

- 仓库：https://github.com/Zhengyuuuui/qoder2api
- 二次开发上游：[QCCG](https://github.com/wangtufly/QCCG)
- 许可：**GPL-3.0**（qoder2api 依据 QCCG 的开源协议二次开发，见其 README 的 License 章节）

本插件中源自/改造自 qoder2api 的部分：

| 本插件路径 | 上游对应 | 改造内容 |
| --- | --- | --- |
| `internal/cosy/` | `internal/cosy/` | 去掉账户存储耦合，保留签名、设备指纹、加密算法 |
| `internal/bridge/` | `internal/bridge/` | 协议转换与 SSE 信封保持原语义，出站 HTTP 改为走 CPA 宿主桥 |
| `internal/logger/` | `logger/` | 去掉凭证前缀日志，接插件日志级别与落盘开关 |
| `baseprompt.json` | `baseprompt.json` | 原文件（Qoder 上游要求的基础提示词） |
| `checkin.go` 的部分逻辑 | `checkin.go` | 签到流程与签到窗口判定 |
| `internal/qoder/` | `account/`、`service.go` 中的区域与端点定义 | 抽成区域/端点常量与归一化 |
| `checkin_source_reference.go.txt` | `checkin.go` | 上游实现的参考副本（后缀为 `.go.txt`，**不参与编译**，仅供对比） |

## 2. CLIProxyAPI（宿主 ABI）

- 仓库：https://github.com/router-for-me/CLIProxyAPI
- 许可：**MIT License**（Copyright (c) 2025-2005.9 Luis Pater；Copyright (c) 2025.9-present Router-For.ME）

本插件中源自/对齐宿主的部分：

| 本插件路径 | 上游对应 | 改造内容 |
| --- | --- | --- |
| `cpasdk/pluginabi/types.go` | `internal/pluginabi/` | 用户名、方法名、Schema 常量与信封字段对齐 |
| `cpasdk/pluginapi/types.go` | `internal/pluginapi/` | 注册、模型、auth、executor、quota、management 等 RPC 结构体对齐 |
| `internal/httpx/` | `internal/pluginhost/` 的 host http 桥协议 | 按宿主契约重写为插件侧客户端 |

`cpasdk` 是**对齐宿主契约的本地副本**，不是 import 宿主的内部包（内部包不可外部导入）。
宿主升级后如 ABI 有变化，需要同步这里的结构体。

## 3. 本插件的许可

由于移植了 GPL-3.0 代码，本插件整体以 **GPL-3.0** 发布（见 `LICENSE`）。
分发二进制（`dist/qoder2api.so`）时请一并保留 `LICENSE` 与本文件。
