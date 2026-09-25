# qoder2api-plugin 实测记录（更新于免费模型实测与流式分帧修复之后）

> 这是本插件在本机的实测记录，已去除账号、密钥与本机绝对路径等敏感信息。

产物：`dist/qoder2api.so`（`cliproxy_plugin_init` ABI，CGO `c-shared`）。
许可：GPL-3.0（移植自 QCCG/qoder2api）。

## 本轮做了什么

用一份 qoder2api 桌面端导出的国际版账号配置（多账号 bundle）做导入，导入过程暴露并修掉了 4 个真实缺陷。

### 1. 导出文件不能导入（功能缺失 → 已实现）

原先只认扁平 `token`/`device_token` 字段，导出格式是
`{"format":"qoder2api-accounts","accounts":[{"secret":"{\"device_token\":…,\"refresh_token\":…}"}]}`。

现在：`auth.parse` 识别导出文件并一个文件展开成多个 CPA 账号（`AuthParseResponse.Auths`），
保留每条账号的 `region`/`name`/`email`/`plan`/`id`；`secret` 既可嵌 JSON 也可为明文 token；
坏条目跳过并计数，全都不可用时报错并提示“导出时要勾选包含凭证”。

### 2. `host.http.do` 状态码恒为 0（严重 → 已修）

宿主把 `pluginapi.HTTPResponse`（**无 json tag**）直接放进 RPC 信封，线上键名是 Go 字段名
`StatusCode`/`Headers`/`Body`；插件按 `status_code` 解析 → 状态码永远 0 →
`bridge/client.go` 的 `StatusCode != 200` 判定把**所有非流式上游请求判成失败**：
模型清单、额度、签到、非流式对话全废。错误体里带着上游真实响应，看起来像“凭证被拒”，极难排查
（我此前就误判过一次）。现在两种键名都接受，且缺失状态码时直接报 ABI 不匹配而不是静默当 0。
测试 mock 也改成宿主真实形态，并加了双形态回归测试。

### 3. 签到域名写死国内（严重 → 已修）

忠实移植了上游的 `openapi.qoder.com.cn`。实测国际版 device token 打国内域名一律
`401 TOKEN_EXPIRE`，换 `openapi.qoder.sh` 即 200。现在按账号 `region` 选域名（`Endpoints.SashBase`）。
另外实测国际版**没有**每日签到计划（`/sash/api/v1/me/daily-check-in*` → 404 NotFound），
只有 `VIEW_DETAILS` 促销活动；插件不会误领促销，并明确提示“该区域无每日签到活动”。

### 4. 凭证不会被宿主刷新（→ 已修）

宿主判断“可否刷新”看 `auth.Metadata["refresh_token"]`，并靠 `NextRefreshAfter` 排期。
插件原先两者都没给，导致 `/auth-files/refresh` 返回 `results: []`（根本不尝试）。
现在 OAuth 账号会带上 `refresh_token` 元数据与下次刷新时间；PAT 账号不会伪造 refresh token。

### 5. 免费模型“排队”被当成额度不足（严重 → 已修）

用户提示 Qwen3.8-Flash 是免费调用、可直接测。实测确实能出字，但上游有**排队制**：
繁忙时返回 **HTTP 403**，正文是
`{"isQueued":true,"serviceAvailable":false,"queueType":"p3","retryAfterSeconds":30}`
（`retryAfterSeconds` 在 9~30 秒之间浮动）。

修复前：这个 403 被原样抛出，宿主标成 `insufficient_quota` / `permission_error`，把用户
**引向充值**（其实免费模型与额度无关）。

修复后：

1. `internal/bridge/queue.go` 识别排队信号（多层转义 JSON + `data:` 前缀 + 正则兜底都能认），
   该错误被明确标注为 `upstream_model_busy`；
2. 按上游建议时长等待后重开上游（默认最多 2 次），期间调用方无感；
   等待预算耗尽才返回 **503 + `qoder_model_busy`**，消息写明“不是额度或凭证问题”；
3. 新增配置 `queue_max_waits`（默认 2，`0` = 快速失败）与 `queue_wait_seconds`
   （默认 0 = 跟随上游建议值），支持 `PATCH /v0/management/plugins/qoder2api/config` 热重载；
4. fast-fail 模式（`queue_max_waits: 0`）不会退化成 1s/2s 的瞬时重试去白打上游。

### 6. 流式内容全空：分帧重复（严重 → 已修）

真实客户端实测发现流式**看似正常、内容全空**：宿主写出的是 `data: data: {...}`。
根因是分帧契约按协议不同：

- **chat-completions**：宿主处理器自己 `fmt.Fprintf("data: %s\n\n", chunk)` → 插件必须发**裸 JSON**，
  且**不能**发 `[DONE]`（宿主自己补）；
- **claude / codex**：宿主处理器**原样写出** → 插件必须发完整 SSE 帧（`event:`/`data:`）。

插件原先三种协议统一加 `data: ` 前缀 → OpenAI 客户端收到无效 JSON，流式内容全空。
现在 `streamWriter` 按宿主声明的输出格式（`rpc.format`）归一化分帧；
端到端测试升级为**解析**每个分片（`json.Valid`），不再用子串匹配（旧断言正是被
`data: data:` 蒙过去的原因）。

### 7. 宿主冷却会掩盖真实原因（现象说明，非缺陷）

插件报错后宿主会冷却该凭证（`transient-error-cooldown-seconds`，默认约 60 秒）。
排队失败后立刻重试同一模型会收到
`auth_unavailable: no auth available ... last upstream error: 上游模型 qfmodel 排队中`。
这不是新错误，是宿主在冷却期内不复用该凭证；等冷却过去或重启宿主即可。

### 8. CI 与发布（GitHub Actions + Release）

- `.github/workflows/ci.yml`：`test`（gofmt / go vet / go test）+ 五个平台的 `c-shared` 构建
  （linux/amd64、linux/arm64、darwin/arm64 为必过；darwin/amd64 与 windows/amd64 标为 best-effort）
  + 打 `v*` tag 时自动发 Release（含 `SHA256SUMS.txt`）。
- 首次 tag 构建暴露一个真问题：release job 在 `download-artifact` 之后没有 `.git`，
  `gh release create` 直接失败（`failed to run git: fatal: not a git repository`）。
  现在显式 `actions/checkout` 并传 `--repo "$GITHUB_REPOSITORY"`。
- `pluginVersion` 改为可用 `-ldflags -X main.pluginVersion=<tag>` 注入：v0.1.0 的产物在宿主里
  注册为 `version=v0.1.0`（实测日志）；注入值为空时回落默认版本，并有单测锁死。
- **用 Release 产物（而不是本地构建）在本机真实宿主复测**：插件加载 → 注册版本 `v0.1.0`
  → 19 个模型 → 1 个账号 → 真实上游调用（免费模型，高峰期排队预算耗尽 → 503 且消息明确
  “不是额度或凭证问题”）→ 进程稳定、无 panic。

### 9. 插件商店（registry 三方源）安装

宿主 `internal/pluginstore` 的安装契约（读源码 + 用宿主真实安装代码验证）：

- 归档资产名必须精确等于 `{id}_{version}_{goos}_{goarch}.zip`（version 是 tag 去掉 `v`）；
- 校验和资产名必须**恰好**是 `checksums.txt`，每行 `<sha256>  <文件名>`（`*` 前缀会被剥掉）；
- zip 根目录下必须有且仅有一个动态库，名字是 `{id}{ext}` 或 `{id}-v{version}{ext}`；
  zip 内出现第二个动态库或嵌套路径会直接失败；
- 安装落点 `plugins/{goos}/{goarch}/{id}-v{version}{ext}`，宿主把 `-v<版本>` 剥掉后 ID 仍是 `qoder2api`。

本仓库为此增加了：

- `scripts/packstore`：用 Go 生成 zip 与 `checksums.txt`（Windows runner 没有 `zip`/`sha256sum`），
  8 条单测锁死上面的命名与内容规则；
- CI：build 阶段按 tag 打 zip，**release 阶段统一汇总** `checksums.txt`
  （矩阵里每个平台各写一份同名文件会互相覆盖，导致部分 zip 校验和丢失）；
- 根目录 `registry.json`（schema_version 1，github-release 类型，只给 `repository`，
  版本交给宿主从 latest release 推导），并有测试校验它能通过宿主的 `ValidatePlugin` 规则。

用宿主真实代码做的离线验证（临时测试，验证后已移出宿主仓库）：

```text
SelectReleaseAssets → ParseChecksums → VerifyChecksum → InstallArchive
安装成功：/tmp/.../linux/amd64/qoder2api-v0.1.1.so（6,635,440 字节，zip 2,752,277 字节）
pluginFileInfoFromPath → id=qoder2api version=0.1.1
discoverCurrentPluginFiles → 1 个已安装插件（面板据此显示“已安装/可更新”）
```

真实宿主端到端实测（v0.1.1，独立实例端口 18319，未触碰生产实例）：

```text
GET  /v0/management/plugin-store
  sources: official + source-94348b6b9bec (raw.githubusercontent.com)
  plugins: qoder2api  install_type=github-release  source_id=source-94348b6b9bec

POST /v0/management/plugin-store/qoder2api/install?source=source-94348b6b9bec
  status=installed  version=0.1.1  install_type=github-release
  path=/tmp/cpa-store-it/plugins/linux/amd64/qoder2api-v0.1.1.so  restart_required=False

重启后宿主日志：
  plugin loaded plugin_id=qoder2api version=0.1.1 path=.../linux/amd64/qoder2api-v0.1.1.so
面板状态：installed_version=v0.1.1  update_available=False
插件状态接口：version=v0.1.1（与 release 一致）
落盘文件 sha256 == Release 裸产物 sha256（18d287a4ea4aec5074a8…）
```

### 生产实例已切换到商店安装（2026-09-25）

- `plugins.store-sources` 指向本仓库 `registry.json`；原手动装的 `plugins/qoder2api.so`（v0.1.0）
  已移到 `CLIProxyAPI/plugins-backup/`（未删除，可回滚）。
- 商店落点 `plugins/linux/amd64/qoder2api-v0.1.1.so`，sha256 与 Release 裸产物一致
  （`18d287a4ea4aec5074a8…`）。
- 重启后：`plugin loaded version=0.1.1`、19 个 `qoder-*` 模型、1 个账号、
  真实调用 200（免费模型排队 102 秒后成功）、管理页 200、panic 计 0。
- 商店视图：`installed_version=v0.1.1`、`update_available=False`。

⚠️ 踩的坑（已修复，供后续参考）：第一次改生产配置时把 `store-sources` 写成了顶格缩进，
YAML 解析失败（`line 114: did not find expected '-' indicator`）导致宿主起不来，停机约 2 分钟。
教训：**改生产 YAML 后必须先过一遍解析器再重启**，`config.yaml` 的 `plugins` 子键要缩进 2 空格。

### v0.1.2：面板 OAuth 登录（auth.login.start / auth.login.poll）

**用户报告**：加载插件后，在面板用 OAuth 登录报 `failed to generate authorization url`。

**根因**：宿主把 `GET /v0/management/<provider>-auth-url` 路由到已注册 auth provider 的
`StartLogin`（`auth_files_oauth_callback.go` → `pluginhost.StartLogin`），而插件只实现了
`auth.identifier/parse/refresh`，缺 `auth.login.start`：

```text
GET /v0/management/qoder-auth-url → 500 {"error":"failed to generate authorization url"}
宿主日志: failed to start plugin auth login error=unknown method: auth.login.start
```

对照实验（同一宿主）：内置 provider 的 `codex/anthropic/antigravity-auth-url` 全部 200，
只有插件 provider（qoder）失败 —— 确认与插件无关的内置 OAuth 不受影响。

**修复**：按上游 `qoder2api/account/oauth.go` 的 device flow + PKCE 实现两个 ABI 方法
（`auth_login.go`）：

```text
start: {DeviceLoginBase}?nonce=&challenge=&challenge_method=S256&client_id=e883ade2-…
poll : {PollEndpoint}?nonce=&verifier=&challenge_method=S256
       404 = 用户尚未授权（继续等待）；200 = {token, refresh_token}
```

登录产物与导入路径同构（同一 `parseQoderCredential` 可读回；`Metadata.refresh_token` 让宿主
认可“可刷新”），所以额度、签到、刷新链路无需改动。另外：区域按 `?region=` → 插件配置优先级选取；
单会话 10 分钟有效期；同会话上游轮询做 1.2 秒节流；上游 5xx/网络抖动只记日志并保持 pending
（不当成凭证失败）。

**单测覆盖**（`auth_login_test.go`，11 个用例）：URL/PKCE 参数、state 唯一、区域选择、wait→success、
产物同构（含 `parseQoderCredential` 回读断言）、userinfo 失败降级、上游 5xx 保持会话、
未知/过期 state、节流、宿主字段名的 wire 契约、落盘 ID 安全性。

**踩坑记录（重要）**：插件商店安装会把 `store.version` + `store.release-tag` 写进
`config.yaml` 的 `plugins.configs.<id>`，宿主 `selectPluginFiles` 按这个版本**钉死**文件选择：

- 手动往 `plugins/<goos>/<goarch>/` 放更高版本的 `.so` **不会生效**，且宿主**静默**跳过（无任何日志）；
- 手工改 `store.version` 而不改 `release-tag` 同样加载不到（实测两者并存时插件根本不加载）；
- 临时实例不加载插件也是同一机制（config 里带着从生产复制的 `store.version: 0.1.1`，
  放进去的 `qoder2api.so` / `qoder2api-v0.1.2.so` 版本都对不上）。

结论：**升级只能走商店**（面板更新或 `POST /v0/management/plugin-store/qoder2api/install`），
它会同时更新 `store.version` 与 `release-tag` 并下载 CI 产物。

## v0.1.2 修正后的完整实测证据（生产实例）

```text
# 升级：必须走商店 install（版本被 store.version + release-tag 双向钉死）
POST /v0/management/plugin-store/qoder2api/install?source=source-94348b6b9bec
  → status=installed  version=0.1.2  path=plugins/linux/amd64/qoder2api-v0.1.2.so
安装后 config: store.version=0.1.2, store.release-tag=v0.1.2
落盘 sha256 == Release 裸产物 (a9e172bb022db97017083149…)
宿主日志: plugin loaded plugin_id=qoder2api version=0.1.2

# 登录入口（修复前 500 failed to generate authorization url）
GET /v0/management/qoder-auth-url
  → 200  url=https://qoder.com/device/selectAccounts?nonce=…&challenge=…&challenge_method=S256&client_id=e883ade2-…
         state=11587eb2… (32 字符)
GET /v0/management/qoder-auth-url?region=cn
  → 200  url=https://qoder.com.cn/device/selectAccounts?...   （区域覆盖生效）

# 轮询（接口名是 get-auth-status，不是 auth-status；插件内部真打上游）
GET /v0/management/get-auth-status?state=<state>  → 200 {"status":"wait"} ×4
插件日志: oauth login started (region=global, state=11587eb2)

# 其它能力未受影响
/v1/models → 19 个 qoder-* 模型；插件状态接口 → version=v0.1.2，账号 1 个
```

宿主对照实验（同一实例、同一时刻）：内置 `codex-auth-url` / `anthropic-auth-url` /
`antigravity-auth-url` 全部 200，`xai-auth-url` 500 且原因是 `failed to start device authorization
flow`（上游网络不可达）——与插件无关。

**用户侧复验**：v0.1.2 发布后用户在其生产环境实测通过面板 OAuth 登录。

**本工作实例的边界**：本机实例上我只验证到 `start → 200`（含 PKCE 参数与区域覆盖）与
`get-auth-status → wait`（插件确实打了真实上游），未在本机完成一次真实浏览器授权
（我发出的链接在 10 分钟窗口内未完成授权，无新增账号落盘，属预期）。

⚠️ 另外提醒：改生产 YAML 后必须过一遍 `yaml.safe_load` 再重启（本轮早前曾因
`store-sources` 缩进顶格导致宿主起不来，停机约 2 分钟）。

### v0.1.3：模型 ID 改用人类可读模型名

**用户需求**：客户端/面板里"直接显示模型名称而不是 qoder 中的模型 ID"。

**证据**：宿主的插件模型条目**不带** `display_name`（实测 `/v1/models` 只有
`created/id/object/owned_by`），所以可读性只能体现在模型 ID 本身：

```text
上游实时清单（国际版, 2026-09-25）：
  qmodel_38max → Qwen3.8-Max      qfmodel → Qwen3.8-Flash
  qmodel_latest → Qwen3.7-Max     qmodel  → Qwen3.7-Plus
  kmodel_latest → Kimi-K3         kmodel  → Kimi-K2.8-Preview
  gmodel → GLM-5.3                gfmodel → GLM-5.3-Flash
  dmodel → DeepSeek-V4-Pro        dfmodel → DeepSeek-Flash
  mmodel → MiniMax-M3             auto/ultimate/performance/efficient
```

**改动**：
- 注册 ID = `model_prefix` + display_name（拿不到名称的 SKU 才退回 SKU，不编名字）；
- `Name` 字段仍保留上游 SKU，执行时通过内置别名表 `名 → SKU` 还原（`Qwen3.8-Flash` → `qfmodel`）；
- 模型目录**按上游 SKU 去重**：否则 `extra_models: [gmodel]` 与实时清单会以两个 ID 注册同一个 SKU；
- 修掉一个既有缺陷：`model_mapping` 里写带前缀的键（`qoder-qfmodel: xxx`）以前永远匹配不上，
  因为执行路径先 `stripModelPrefix` 再查表；现在带前缀键按裸键登记，且**优先于内置别名**；
- 管理页模型表改为「注册 ID（模型名）| 上游 SKU | 上下文 | 最大输出」。

**兼容性**：旧 ID（`qoder-qfmodel`、`qoder-qmodel_latest`）**保持可用**（裸 SKU 直通上游），
只是不再出现在模型列表里 —— 从旧版升级不需要改客户端配置。

**测试**：`TestModelIDsUseHumanReadableNames`（ID 是名称 + Name 保留 SKU + 名称/SKU/关键字三种
请求都能还原）、`TestUserMappingOverridesDisplayAlias`（带前缀映射键生效且优先于内置别名）、
目录去重用例（裸 SKU ID 不再注册、同一 SKU 不重复）。

## 部署到宿主（本机实测）

按下面步骤装好并跑通（凭证与日志类文件都被 .gitignore 忽略）：

- `auths/` 下的多账号导出文件（600）—— 原件未改动（md5 核对一致）；
  宿主首次成功刷新后会把它改写成扁平单账号格式（既定行为）。
- `plugins/qoder2api.so`、`bin/cpa-server`、`config.yaml`（port 18318 / auth-dir auths / 插件启用）。
- 管理密钥明文会被宿主启动时哈希回写，故明文另存为独立文件（600）。
- 管理页：`http://127.0.0.1:18318/v0/resource/plugins/qoder2api/console`。
- 验证完成后已停服（端口释放、`plugin unloaded`）；启动命令写在 README。

### 部署侧修复：管理端额度路由只发 AuthIndex、不发 StorageJSON（严重 → 已修）

宿主 `internal/api/handlers/management/plugin_quota.go` 组装 `QuotaFetchRequest` 时
只带 `AuthIndex`/`AuthID`/`Metadata`/`Attributes`；插件又不把 token 放进 metadata/attributes，
而 `handleQuotaFetch` 只解析 `StorageJSON` → 真实部署里额度查询直接 502
（`auth storage has no token field ... and no secret blob`）。现在额度/刷新/执行三条路径统一走
`resolveRPCCredential`：优先用宿主给的 storage，缺失则按 auth_index（必要时用 auth_id 经
`host.auth.list` 映射）回查；`host.auth.get` 只认 index，不能拿 id 直接当 index 用。
同一处还把 `model.for_auth`、执行路径的凭证解析改成 bundle 感知，避免多账号导出文件被误判。

## 真实账号实测（evidence）

账号：一个国际版 Qoder 账号（**已脱敏**），区域 global，OAuth（device + refresh token）。

| 项目 | 结果 |
| --- | --- |
| 导入 | 1 账号，`region=global`、`auth_mode=oauth`、`email` 正确 |
| 凭证有效性 | 宿主驱动刷新 `{"success":true}`（插件内 userinfo 200） |
| 真实上游模型清单 | `POST /plugins/qoder2api/models/refresh` → 200，15 个模型 |
| 模型注册 | `/v1/models` → 19 个 `q2-*`（15 实时 + 别名/兜底） |
| 真实额度 | 200：`plan=Free`、`userQuota.total=0`、`isQuotaExceeded=true` |
| 非流式对话 | 上游 403 `code 112`（pricingUrl）→ 插件转 403 `insufficient_quota` |
| 流式对话 | 事件流打通，上游业务错误原样回传 |
| **免费模型 Qwen3.8-Flash 非流式** | **200 + 真实内容**（`你好`，`finish_reason=stop`，tokens≈13.2k），中途排队 130 秒后成功 |
| **免费模型 Qwen3.8-Flash 流式（OpenAI 协议）** | **200，21 个分片全部可解析，0 个不可解析**，内容完整 |
| **免费模型 Qwen3.8-Flash 流式（Claude `/v1/messages`）** | **200**：`message_start`→`content_block_start`→`ping`→`content_block_delta`(text=你好)→`content_block_stop`→`message_delta`→`message_stop`，中途排队 30 秒后成功 |
| 免费模型排队耗尽预算 | 503 + `qoder_model_busy`（消息明确“不是额度或凭证问题”） |
| 签到 | 打对区域（不再 401），返回 `no_campaign`（国际版无签到计划） |
| 稳定性 | 无 panic/fuse，日志不含凭证 |
| 本仓库部署 | 9/9 冒烟通过（含修复后的额度路由）；管理页 6 个接口全 200；`/console` 返回 HTML |

**关键结论**：链条已完整打通到上游并正确回传上游语义。该账号是 **Free 计划、额度 0**
（`isQuotaExceeded: true`），所以上游对任何推理请求回 403 引导升级——这是账号额度状态，
不是插件缺陷；换一个有额度的账号即可出字。

## 自动化验证

- GitHub Actions 在 5 个平台构建全绿（linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64）。
- `gofmt -l` 干净、`go vet ./...` 干净、`go test ./... -count=1` 全绿（含新增回归测试）。
- 真实宿主集成冒烟 11/11 通过（加载/模型/账号/上游清单/额度/刷新/签到/非流式/流式/无泄漏/无 panic）。

## 行为说明（用户需要知道的）

- 多账号导出文件被 CPA 标记为“插件虚拟账号”：不能在面板单独编辑/删除，刷新也不会写回该文件；
  单账号导出在首次成功刷新后会被 CPA 改写成本插件扁平格式（一条账号一个文件）。
- 国际版账号：无每日签到；额度与计费走 `openapi.qoder.sh`。
- 插件状态目录默认 `~/.qoder2api-plugin`（`machine_salt`、`state.json`、可选日志）。
  本轮测试写入的临时设置（auto_checkin）已恢复默认，测试用账号记录已清理，原文件备份为
  `state.json.bak-before-cleanup`。

## 未验证

- 付费模型（`qmodel_38max` / `dmodel` / `gmodel` 等）的成功推理：该账号 Free 计划额度 0，
  上游一律 403 `code 112`（pricingUrl）；**免费模型 `qfmodel` 已实测成功**。
- 签到**成功领取**（国际版无签到计划，返回 `no_campaign`）。
- 多账号同时故障转移；Windows/macOS 构建。
- Codex（`/v1/responses`）协议的真实流式：分帧已按宿主契约实现并有单测，
  但本轮只对 OpenAI 与 Claude 两条协议做了真实客户端验证。
