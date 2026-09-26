# qoder2api-plugin

[![CI](https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Hkxtor/CLIProxyAPI-qoder2api-plugin?display_name=tag)](https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/releases)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

把 [qoder2api](https://github.com/Zhengyuuuui/qoder2api) 打包成 **CLIProxyAPI 原生动态库插件**。

插件通过宿主的 `cliproxy_plugin_init` ABI 加载，一次性声明五类能力，宿主的管理端、路由、
鉴权、日志与代理全部复用，不需要再单独跑一个 qoder2api 服务：

| 能力 | 作用 |
| --- | --- |
| `auth_provider` | 识别并加载 `auths/qoder-*.json` 凭证，定时向上游校验/刷新 |
| `model_provider` | 注册 Qoder 模型（带前缀，默认 `qoder-`），支持从上游拉实时清单 |
| `executor` | 转发 chat-completions / claude / codex 三种协议，含 SSE 流式 |
| `quota_provider` | 查询套餐额度与个人拓展包，供管理端展示 |
| `management` | 自有管理接口 + 内嵌控制台页（账号、额度、签到、日志） |

## 特性

- **一套凭证走 CPA**：账号即 `auths/qoder-*.json`，CPA 的轮询、优先级、禁用、代理、日志全部生效。
- **三种协议**：`/v1/chat/completions`、`/v1/messages`（Claude）、Codex 系客户端都可直接指向 CPA。
- **流式**：SSE 经宿主的 stream 桥逐片读取，支持调用方取消，保留 qoder2api 的信封重试语义；
  分帧按宿主契约处理（chat-completions 发裸 JSON，claude/codex 发完整 SSE 帧），两条协议均已用真实客户端验证。
- **排队自适应**：免费模型（Qwen3.8-Flash）繁忙时上游会要求排队，插件按上游建议时长等待后重试，不会把它误报成额度不足（见「免费模型与排队」）。
- **模型前缀隔离**：默认注册为 `qoder-*`，不与 CPA 原生 provider 抢模型名；可配置、可关闭。
- **额度与签到**：管理页可视化额度、一键签到、每日定时签到与连续天数统计。
- **出站全走宿主桥**：上游请求经 `host.http.do` / `do_stream`，因此走宿主代理、进宿主请求日志。

## 构建

需要 Go 1.22+ 与 C 编译器（`cgo`）。CI（`.github/workflows/ci.yml`）会在 5 个平台上构建，
打 `v*` tag 时自动把产物发布成 Release；本地构建 = `./build.sh`。

发行版号由 CI 用 `-ldflags -X main.pluginVersion=<tag>` 注入，本地构建保留默认值。

```bash
./build.sh          # 先跑 go test ./...，再产出 dist/qoder2api.so
```

产物按平台命名：Linux `qoder2api.so`、macOS `qoder2api.dylib`、Windows `qoder2api.dll`。

## 安装

### 方式一：通过 CPA 插件商店安装（推荐，可一键升级）

本仓库根目录的 `registry.json` 就是 CPA 的**三方源清单**。在 CPA 的 `config.yaml` 里加上它：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/main/registry.json"
```

重启 CPA → 管理面板 → 插件商店 → 搜 **Qoder 2API** → 一键安装。也可以直接调接口：

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugin-store/qoder2api/install" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

宿主会取本仓库最新 Release 里的 `qoder2api_<版本>_<goos>_<goarch>.zip`，核对同名 Release 的
`checksums.txt`（sha256 校验），解包到 `plugins/<goos>/<goarch>/qoder2api-v<版本>.so`。
插件 ID 仍是 `qoder2api`（宿主动剥掉 `-v<版本>`），所以 `plugins.configs.qoder2api` 配置照旧；
再次点击安装即为升级。商店安装要求 `plugins.enabled: true`。

<details>
<summary>自建三方源（或不想依赖 GitHub API 配额）</summary>

List 接口会为每个 GitHub Release 型条目调用一次 `api.github.com`（未认证 60 次/小时/IP），
可用 `plugins.store-auth` 给 GitHub 配 token，或改用 `direct` 类型在清单里直接声明产物：

```json
{
  "id": "qoder2api",
  "version": "0.1.1",
  "install": {
    "type": "direct",
    "artifacts": [
      {
        "goos": "linux", "goarch": "amd64",
        "url": "https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/releases/download/v0.1.1/qoder2api_0.1.1_linux_amd64.zip",
        "sha256": "<zip 的 sha256>"
      }
    ]
  }
}
```

两种类型对产物格式的要求一致：归档名 `{id}_{version}_{goos}_{goarch}.zip`，
zip 根目录下**只能有** `{id}{ext}` 一个动态库，校验和资产名必须恰好是 `checksums.txt`。
本仓库用 `scripts/packstore` 生成这些资产（`./scripts/packstore/` 下有契约单测）。
</details>

### 方式二：下载预编译产物（不用装 Go）

[Releases](https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin/releases) 里按平台取文件，
**重命名成宿主认识的插件名**后放进 `plugins/`：

| 平台 | 下载文件 | 放进去时改名为 |
| --- | --- | --- |
| Linux x86_64 | `qoder2api-linux-amd64.so` | `qoder2api.so` |
| Linux arm64 | `qoder2api-linux-arm64.so` | `qoder2api.so` |
| macOS（Apple Silicon） | `qoder2api-darwin-arm64.dylib` | `qoder2api.dylib` |
| macOS（Intel） | `qoder2api-darwin-amd64.dylib` | `qoder2api.dylib` |
| Windows x64 | `qoder2api-windows-amd64.dll` | `qoder2api.dll` |

同名 Release 附带的 `SHA256SUMS.txt` 可用于校验；产物是宿主 ABI 的 `c-shared` 动态库，
文件名必须是 `qoder2api.<ext>`（宿主用文件名（去掉扩展名）作为插件 ID）。

> ⚠️ **如果你已经用插件商店装过本插件**，宿主会把版本钉在
> `config.yaml` 的 `plugins.configs.qoder2api.store.version`（以及 `release-tag`）上，
> 此时手动往 `plugins/` 里丢新 `.so` **不会生效**（新版文件会被版本过滤掉，而且宿主对此是静默的）。
> 请用商店升级（面板里的“更新”，或 `POST /v0/management/plugin-store/qoder2api/install`）。
> 宿主从商店安装的产物位于 `plugins/<goos>/<goarch>/qoder2api-v<版本>.<ext>`。

### 方式三：本地构建

1. 把产物放进 CPA 的插件目录（`plugins.dir` 指向的目录）：

   ```bash
   cp dist/qoder2api.so /path/to/cpa/plugins/
   ```

2. 在 CPA `config.yaml` 里开启插件并配置本插件：

   ```yaml
   plugins:
     enabled: true
     dir: "/path/to/cpa/plugins"
     configs:
       qoder2api:
         enabled: true          # 插件级开关（宿主读取）
         region: "cn"           # global（默认）或 cn
         model_prefix: "qoder-" # 注册到 CPA 的模型前缀
         auto_checkin: true
         auto_checkin_at: "10:00"
   ```

3. 放入账号凭证（见下一节），重启 CPA。日志里应能看到：

   ```
   pluginhost: plugin loaded plugin_id=qoder2api path=.../qoder2api.so
   [qoder2api][...] registered: region=cn prefix="qoder-" state_dir=~/.qoder2api-plugin
   [qoder2api][...] parsed qoder auth <label> (region=cn)
   ```

## 账号导入

### 方式一：面板 OAuth 登录（推荐，浏览器授权一次即可）

插件实现了 CPA 的 `auth.login.start` / `auth.login.poll`，可以直接在面板里登录 qoder 账号：

1. 打开管理面板 → 添加认证 / 登录 → 选 **Qoder 2API**；
2. 浏览器打开插件返回的登录页（`https://qoder.com/device/selectAccounts`，带 PKCE 参数）；
3. 在弹出的页面选择/登录你的 Qoder 账号并确认；
4. 面板轮询到授权完成后，插件把 `device_token` + `refresh_token` 写成一个 auth 文件，账号立即可用、可自动续期。

也可以直接调接口拿登录 URL：

```bash
# 默认跟随插件配置的 region；需要国内版就加 ?region=cn
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  "http://127.0.0.1:<port>/v0/management/qoder-auth-url"
# → {"url":"https://qoder.com/device/selectAccounts?...","state":"..."}
#
# 浏览器授权后用 state 轮询（面板会自动做这件事）
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  "http://127.0.0.1:<port>/v0/management/oauth-callback?state=<state>"
# → {"status":"wait"} 直到授权完成 → {"status":"ok"}
```

要点：

- **区域**：`?region=cn` 或 `?region=global` 覆盖插件默认 `region`（国内账号必须用 `cn`，否则登录页域名不对）；
- **有效期**：单次登录会话 10 分钟，超时面板会提示重新发起；
- **节流**：面板轮询快于上游建议节奏，插件对同一会话做 1.2 秒最小间隔，避免打满上游；
- **登录产物与导入完全同构**：同一个 `auth.refresh` 路径解析、同一个额度/签到链路；
- 登录失败时面板会显示具体原因（网络/上游状态码等），插件不会把瞬时失败当成凭证无效。

> 历史背景：v0.1.2 之前插件只实现了文件型 auth（identifier/parse/refresh），面板点 OAuth 登录会返回
> `failed to generate authorization url`（宿主日志 `unknown method: auth.login.start`）。已修复。

### 方式二：直接把 qoder2api 的账号导出文件丢进 auths/（推荐）

qoder2api 桌面端导出的 `qoder2api-accounts-*.json`（`format: "qoder2api-accounts"`，
**必须勾选包含凭证**）可以直接放进去，不用手工拆：

```bash
install -m 600 qoder2api-accounts-<导出日期>.json /path/to/cpa/auths/
```

- 一个导出文件会展开成多个 CPA 账号（每条账号独立展示、独立计费路由）；
- 每条账号自带 `region` 优先（例如国际版账号就是 `global`），不受插件默认 `region` 影响；
- 多账号导出会被 CPA 标记为“插件虚拟账号”：**不能在面板上单独编辑/删除**，改账号请改这个文件；
- 单账号导出在第一次成功刷新后，会被 CPA 改写成本插件的扁平格式（一条账号一个文件）；
- 导出时若没勾选包含凭证（`include_secrets: false`），导入会明确报错提示。

导出条目里的字段映射：`secret` 里的 `device_token`/`refresh_token` → 插件的凭证；
`name`/`email`/`plan`/`region` → 展示与区域；用户可见的账号 ID 用导出里的 `id`。

### 方式三：手写单账号文件

一个文件一个账号，放在 CPA 的 `auth-dir`（通常是 `auths/`）：

```json
{
  "type": "qoder",
  "token": "pt-xxxxxxxx",     // 必填：Qoder personal token / access token
  "refresh_token": "drt-xxx", // 可选：OAuth 账号的续期 token（有就填，丢了无法续期）
  "region": "cn",             // 可选：覆盖插件默认 region（global / cn）
  "label": "主账号",           // 可选：展示名
  "email": "me@example.com",  // 可选：仅用于展示
  "prefix": "work"            // 可选：CPA 账号级模型前缀
}
```

文件名建议 `qoder-*.json`（插件也接受 `type: qoder` 或含 `qoder_token`/`device_token`/`secret` 的文件）。
token 字段也支持 `device_token`、`access_token`、`api_key`、`personal_token`、`pat`、`qoder_token`。
OAuth 导出里的 `"secret": "{\"device_token\":\"dt-…\",\"refresh_token\":\"drt-…\"}"` 整段拷贝也能识别。

**从独立部署的 qoder2api 迁移**：把它 `settings.json` 里的 `machine_salt` 填到本插件的
`machine_salt`，可保持设备指纹一致（否则插件会自动生成新的盐，上游可能视为新设备）。

### 部署检查清单（实测走通）

| 位置 | 内容 |
| --- | --- |
| `CLIProxyAPI/auths/` 下的账号文件 | 导入或面板登录得到的账号（宿主首次刷新后会改写成扁平单账号格式） |
| `CLIProxyAPI/plugins/qoder2api.so` | 插件产物（手动安装）；商店安装则是 `plugins/<goos>/<goarch>/qoder2api-v<版本>.so` |
| `CLIProxyAPI/config.yaml` | 监听端口、`auth-dir: auths`、`plugins.enabled: true`、插件配置、API key 与管理密钥 |
| `CLIProxyAPI/bin/cpa-server` | 宿主二进制（不要提交进仓库） |
| 自行另存的 `cpa-management-key`（600） | 管理密钥明文（`config.yaml` 里的那份启动时会被哈希回写，所以明文另存） |
| `~/.qoder2api-plugin/` | 插件状态目录（机器盐、模型缓存、签到记录） |

启动与查看：

```bash
cd CLIProxyAPI && ./bin/cpa-server --config ./config.yaml
# 管理页：http://127.0.0.1:<port>/v0/resource/plugins/qoder2api/console （需要管理密钥）
# 注意：console 页面地址是 /v0/resource/plugins/<pluginID>/console，不是 /v0/management/...
```

管理密钥明文会被宿主在启动时哈希并回写 `config.yaml`——**只写明文会被吃掉**，
所以明文要自己另存一份（600，例如 `~/cpa-management-key`）。改密钥请改这个文件并同步 `config.yaml`。

### 导入后怎么确认

```bash
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/qoder2api/status
```

账号应该带着 `region` / `email` / `auth_mode` / `status` 出现；`models/refresh` 能拿到上游实时清单
（需要账号有有效凭证）。凭证是否真的可用，可用
`POST /v0/management/auth-files/refresh {"all":true}` 让宿主驱动插件刷新一次来验证。

## 配置项

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `region` | enum | `global` | Qoder 站点：`global`（国际）/ `cn`（国内）。账号里的 `region` 可覆盖。 |
| `state_dir` | string | `~/.qoder2api-plugin` | 状态目录（机器盐、模型缓存、签到记录、日志）。也可用环境变量 `QODER2API_PLUGIN_HOME`。 |
| `model_prefix` | string | `qoder-` | 注册到 CPA 的模型 ID 前缀；留空则不加前缀。修改请走 `PATCH /v0/management/plugins/qoder2api/config` 或 CPA 面板的插件配置（热重载）。 |
| `extra_models` | list | 空 | 额外注册的模型 ID（不含前缀），逗号/空格分隔。 |
| `model_mapping` | list | 空 | `客户端模型名=上游SKU` 映射，逗号/空格分隔，例如 `sonnet=qmodel_38max,gpt=dmodel`。 |
| `log_level` | enum | `info` | `debug` / `info` / `error`。 |
| `log_to_file` | bool | `false` | 是否把插件日志写入 `<state_dir>/logs`。 |
| `auto_checkin` | bool | `false` | 是否每日自动签到。 |
| `auto_checkin_at` | string | `10:00` | 自动签到时间（本地时区 `HH:MM`）。 |
| `machine_salt` | string | 自动生成 | 设备指纹盐覆盖项。 |
| `queue_max_waits` | int | `2` | 上游要求排队时最多等几次；`0` = 不等待（快速失败）。 |
| `queue_wait_seconds` | int | `0` | 强制单次等待秒数；`0` = 跟随上游建议值（无建议时 15 秒）。 |

## 模型与映射

- 注册的模型 ID = `model_prefix` + **模型名**，例如 `qoder-GLM-5.3`、`qoder-Qwen3.8-Flash`。
  模型名取自上游实时清单的 display_name，拿不到名称的 SKU（如 `kmodel`）才退回 SKU 本身。
- **为什么用模型名而不是上游 SKU**：CPA 的 `/v1/models` 与各客户端下拉只暴露模型 ID（宿主不给
  插件模型带 `display_name` 字段），用 SKU 会让用户看到 `qoder-qmodel_38max` 这种内部代号。
- 执行时插件会剥掉前缀，把模型名还原成上游 SKU（`Qwen3.8-Flash` → `qfmodel`）再请求上游。
- ⚠️ **模型 ID 变了，客户端要跟着改**：升级后请把客户端里的 `qoder-qfmodel` 换成 `qoder-Qwen3.8-Flash`
  （对照表见下）。插件内部仍认旧 SKU（`qfmodel` 直通上游），但**宿主的路由表里只有新 ID**，
  所以拿旧 ID 请求会在宿主层就被拒：`400 unknown provider for model qoder-qfmodel`。
  **不想改客户端**的话，把旧名字加进插件配置 `extra_models` 就能恢复 —— 这是**可选**开关，默认不注册
  任何旧 ID（`extra_models` 是运维显式声明，不与实时清单做同名去重；改完记得重启宿主）：

  ```yaml
  extra_models: "qfmodel,qmodel_38max"   # 于是 qoder-qfmodel / qoder-qmodel_38max 又能用了
  # YAML 列表写法也可以（v0.1.5 起）：
  # extra_models: ["qfmodel", "qmodel_38max"]
  ```
  这类 ID 会以旧名字出现在 `/v1/models` 里（列表里会同时有新名字与旧名字）——要干净就只留新名字。
- 新旧对照（国际版实时清单）：`qfmodel → qoder-Qwen3.8-Flash`、`qmodel_38max → qoder-Qwen3.8-Max`、
  `qmodel_latest → qoder-Qwen3.7-Max`、`qmodel → qoder-Qwen3.7-Plus`、`kmodel_latest → qoder-Kimi-K3`、
  `kmodel → qoder-Kimi-K2.8-Preview`、`gmodel → qoder-GLM-5.3`、`gfmodel → qoder-GLM-5.3-Flash`、
  `dmodel → qoder-DeepSeek-V4-Pro`、`dfmodel → qoder-DeepSeek-Flash`、`mmodel → qoder-MiniMax-M3`、
  `auto/ultimate/performance/efficient → qoder-Auto` 等。注意 `q37fmodel`、`gm51model` 上游没给名称，
  所以列表里仍显示 SKU 形式（不编造名称）；`ultimate/performance/efficient` 属于未开通档位（上游标
  `enable=false`），本插件不注册。
- 模型来源按优先级：**上游实时清单**（含 display_name、上下文窗口、推理标记）→ 内置兜底 SKU →
  人类可读别名（`claude-sonnet`、`claude-opus`、`claude-haiku`、`gpt`、`gemini`）→ `extra_models`。
  同一个上游 SKU 只注册一次，不会因为写法和来源不同在列表里出现两次。
- 别名是按**关键字**路由到上游 SKU 的（例如 `sonnet` → `gmodel`），可用 `model_mapping` 改：
  配置 `model_mapping: "sonnet=qmodel_38max"` 后，含 `sonnet` 的请求会走 `qmodel_38max`。
  映射键写成带前缀的 `qoder-Qwen3.8-Flash=...` 也生效（等于写模型的完整 ID），且优先于内置的模型名别名。
- 想刷到最新模型清单：管理页点“刷新模型”，或 `POST /v0/management/plugins/qoder2api/models/refresh`。
  ⚠️ 清单或模型 ID 变化后**要重启宿主**才会体现在 `/v1/models` 里 —— 实测：把 `extra_models` 清空后
  宿主的模型目录仍是 21 项（含两个旧 ID），重启后回到 19 项。宿主只在插件加载/重载时拉取模型清单。

## 添加账号（OAuth 登录）

两种区域入口都在**插件控制台页**里：`GET /v0/resource/plugins/qoder2api/console` → 卡片「添加账号（OAuth 登录）」

- **登录国际版** → `qoder.com` 的 device 授权页
- **登录国内版** → `qoder.com.cn` 的 device 授权页

点按钮会显示授权链接并自动轮询，浏览器里完成授权后**账号直接写入 CPA**（由宿主保存 auth 记录，不需要手工导入）。
宿主的 OAuth 面板只列它自己硬编码的 provider，插件不出现在那份列表里 —— 所以区域入口由插件页提供。

不想用页面的话，等价的两条宿主接口（`region` 走 query，会被宿主透传成插件 metadata）：

```bash
# 1) 取授权链接（region 省略时用插件配置里的 region）
curl -H "Authorization: Bearer <管理密钥>" \
  "http://127.0.0.1:18318/v0/management/qoder-auth-url?region=cn"
#    → {"url":"https://qoder.com.cn/device/selectAccounts?...","state":"<state>"}

# 2) 轮询状态（status: wait / ok / error）；ok 表示账号已落盘
curl -H "Authorization: Bearer <管理密钥>" \
  "http://127.0.0.1:18318/v0/management/get-auth-status?state=<state>"
```

## 客户端接入

CPA 的地址 + 任意 `api-keys` 即可，模型名用带前缀的 ID：

```bash
# OpenAI 兼容（模型名用带前缀的模型名，如 qoder-GLM-5.3 / qoder-Qwen3.8-Flash）
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA API KEY>" -H "Content-Type: application/json" \
  -d '{"model":"qoder-Qwen3.8-Flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Claude Code / Codex CLI 等指向 CPA 即可，插件声明了 `chat-completions` / `claude` / `codex`
三种输入输出格式，宿主不会做二次翻译。

## 管理接口

需要 CPA 的管理密钥（`remote-management.secret-key`）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v0/management/plugins/qoder2api/status` | 账号列表（含 `auth_index`）、区域、前缀、签到状态、模型统计。**不含 token**。 |
| POST | `/v0/management/plugins/qoder2api/checkin` | 签到。body：`{"account_ids":["qoder-x"]}`，省略则全部账号。 |
| POST | `/v0/management/plugins/qoder2api/quotas` | 批量查额度。body：`{"account_ids":[...],"concurrent":2}`。 |
| POST | `/v0/management/plugins/qoder2api/models/refresh` | 从上游拉模型清单并缓存。 |
| GET | `/v0/management/plugins/qoder2api/logs?limit=200&since=0` | 读插件日志（环形缓冲，`since` 递增拉增量）。 |
| POST | `/v0/management/plugins/qoder2api/settings` | 改**插件级**设置。body：`{"auto_checkin":true,"auto_checkin_at":"09:30","model_mapping":{"sonnet":"gmodel"}}`。这些立即生效。 |
| PATCH | `/v0/management/plugins/qoder2api/config` | **宿主提供**：改插件配置（`model_prefix`、`region`、`state_dir`、`extra_models` 等），会写回 `config.yaml` 并热重载。例：`{"model_prefix":"q2-"}`。 |
| GET | `/v0/management/plugins/qoder2api/config` | **宿主提供**：读当前插件配置。 |
| POST | `/v0/management/plugins/qoder2api/quota` | **宿主提供**：单账号额度。body：`{"auth_index":"<status 里的 auth_index>"}`，宿主转发到插件的 `quota.fetch`。 |
| GET | `/v0/resource/plugins/qoder2api/console` | 内嵌控制台页（静态，不含任何凭证）。 |

页面的管理密钥从 CPA 面板的浏览器存储里复用（`localStorage` / `sessionStorage`），
**鉴权失败会停止自动刷新**，避免触发 CPA 管理接口的按 IP 封禁。

### 为什么模型前缀不能在插件设置里改

`model_prefix` / `region` 属于**注册期参数**：宿主的模型注册表只在它自己配置变更时重读插件的模型清单。
插件单方面改前缀会让 `/v1/models` 与插件内部状态不一致。所以插件自己的 `/settings` 会拒绝这类改动
（返回 409 并告诉你正确接口），请用上面宿主提供的 `PATCH /v0/management/plugins/qoder2api/config`，
或在 CPA 面板的插件配置里改——两者都会持久化并热重载（已在本仓库验证：PATCH 前缀后
`/v1/models` 从 19 个 `qoder-*` 变成 19 个 `q2-*`）。

`status` 里同时给出两个口径，便于排查这类不一致：
`registered_models`（插件当前会注册的模型数）与 `model_count`（已缓存的上游实时清单条数）。

## 状态与安全

- 状态目录内容：`state.json`（设置覆盖、模型缓存、签到记录与连续天数）、`machine_salt`（设备指纹盐）、
  `logs/`（可选日志，开启 `log_to_file` 后生成）。
- 所有上游请求都经宿主 HTTP 桥（`host.http.do` / `host.http.do_stream`），因此受宿主的全局代理、
  账号级代理与请求日志约束。
- 插件对外只回传账号名、区域、状态、额度、签到结果；**从不回传 token**。控制台页是纯静态 HTML，
  唯一的网络目标是 CPA 自身的 `/v0/management/...` 与 `/v0/resource/...`。

## 免费模型与排队（Qwen3.8-Flash 实测）

Qoder 的免费模型（`qfmodel` = Qwen3.8-Flash）**能通过本插件正常出字**，但它走"排队制"：

- 空闲时几秒内直接返回（实测 3~4 秒拿到完整回复，含 `[DONE]` 的 SSE 流）；
- 繁忙时上游返回 **HTTP 403**，正文是 `{"isQueued":true,"serviceAvailable":false,"queueType":"p3","retryAfterSeconds":30}`，
  即"该模型当前不可服务，建议 N 秒后再来"；`retryAfterSeconds` 在 9~30 秒之间浮动。

插件的行为：

1. 识别排队信号 → 按上游建议时长等待后重试（默认最多 2 次），期间调用方无感；
2. 预算用尽 → 返回 **503 + `qoder_model_busy`**，消息明说"排队中、不是额度或凭证问题"。
   （旧行为把 403 原样抛出，宿主标成 `insufficient_quota`，把人引向充值——已修。）

想提高免费模型的一次成功率，放宽等待窗口：

```yaml
plugins:
  configs:
    qoder2api:
      queue_max_waits: 4        # 最多等 4 次
      queue_wait_seconds: 0     # 跟随上游建议值（9~30 秒）→ 最坏约 2 分钟
```

代价是排队时**请求会挂住**（实测 60~120 秒才算失败）。要"立刻失败、自己重试"就设 `queue_max_waits: 0`。

### 一个宿主层面的坑

宿主 CPA 在插件报错后会**冷却该凭证**（`transient-error-cooldown-seconds`，默认约 60 秒）。
所以排队失败后紧接着再打同一模型，会立刻收到
`auth_unavailable: no auth available ... last upstream error: 上游模型 qfmodel 排队中`——
这不是新错误，是宿主在冷却期内不复用该凭证。等冷却过去或重启宿主即可。

## 实测记录

真实账号、真实上游的验证过程（含 7 个在部署中暴露并修掉的缺陷、免费模型排队行为、
宿主凭证冷却现象）见 [`docs/verification.md`](docs/verification.md)。

## 已知限制

- 本插件的验证分两层：单元测试 + 真实 CPA 宿主集成（所需凭证用户自备）。集成测试已覆盖：
  插件加载、导出文件导入、模型注册、真实上游模型清单（15 个）、真实额度查询、宿主驱动的凭证刷新、
  非流式与流式对话走到上游并正确回传上游错误。**能否真正出字取决于账号额度**。
- 签到只针对国内账号：国际版没有每日签到计划（`/sash/api/v1/me/daily-check-in*` 实测 404），
  国际版账号签到会直接返回“该区域无每日签到活动”；签到的活动域名按账号 `region` 选，
  否则国际版 token 打国内域名会被判 401 TOKEN_EXPIRE。
- 多账号导出文件被标记为虚拟账号后，CPA **不会**把刷新后的 token 写回该文件；
  需要轮换凭证的场景请改用单账号文件。
- 宿主的管理端**额度路由**只发 `AuthIndex`/`AuthID` + metadata/attributes，**不发 `StorageJSON`**；
  插件不把 token 放进 metadata，所以凭证解析必须能按 `auth_index` 回查（否则额度查询会误报“凭证缺失”）。
- 移植自 qoder2api（GPL-3.0），下游能力是 **CPA 插件**形态：不再自带独立 Web 服务、账号存储、
  OAuth 登录与 Dockerfile。控制台只保留账号/额度/签到/日志四类视图。
- `cpasdk/` 是对齐宿主契约的本地结构体副本（宿主内部包不可导入）。宿主升级若改动 ABI/JSON 契约，
  需要同步这里——宿主 `host.http.do` 回的是无 json tag 的 `pluginapi.HTTPResponse`（Go 字段名），
  只认一种键名会让状态码静默变 0、把所有非流式上游请求判成失败。
- 上游风控依赖设备指纹（机器盐 + cosy 签名）。换机器/换状态目录会被视为新设备。

## 许可

GPL-3.0（因移植了 qoder2api/QCCG 的 GPL-3.0 代码）。来源与署名见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
