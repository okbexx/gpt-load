# Pi 可选驱动：基线能力清单与差距审计

## 结论与证据边界

**完整 Pi 等价发布必须保持 BLOCKED。保留 CPA/Bifrost 是“不回归”，不是“Pi 已支持”。** 本次没有实现驱动，没有安装依赖、构建、运行目标程序或使用真实凭证，也没有提交/推送。所有基线引用来自 `git show b517f713204ea6507a0451e7988e08e428ca2bca:<path>`，不依赖并行工作者修改中的文件。

- 基线：`b517f713204ea6507a0451e7988e08e428ca2bca`；检查时分支 `feat/pi-driver`、HEAD 与基线相同且工作树干净。
- JSON 程序统计：**670 条唯一能力记录**；其中基线支持 **520** 条，基线不支持/明确边界 **150** 条。负面条目计入 inventory，不能算作功能新增需求。
- **24 个渠道**：四个订阅渠道及 **20 个保持原状的 API 渠道**。完整扫描所有 module 的 **306 个 route tuple**；其中订阅 **32 个**，没有只读 README 后手写一个聊天表。
- 包含 **71 个鉴权控制面入口**、**13 个数据面入口**，以及全部渠道 WS 的五个独立 flags。入口注册不代表每个渠道都可用。
- 所有记录 `pi_status=unverified`、`pi_supported=null`、`live_verification.status=not_run`。**零项 Pi 等价性已验证**。源码正面证据不是实时服务成功回执；源代码清单也不等于对无限参数组合的全覆盖证明。

## 机器可读契约

主产物：[`capabilities.json`](./capabilities.json)。顶层 `baseline_commit`、`counts`、`route_coverage`、`channels`、`capabilities` 数组。

每条记录有稳定唯一 `id`，以及 `channel/category/protocol/operation/baseline_supported/pi_status/evidence`。`evidence` 为仓库内源码路径数组；`provenance` 另存冻结基线行号与标识符。保留丰富的 `baseline.route_mode`、`possible_modes`、`transports`、`responses_store_handling`、`count_implementation`、Pi 验证与保护策略。

- `baseline_supported` 是源码实现/声明结论；`baseline_preservation_status=required_not_retested` 不是假定并行改造已经保持行为。
- `native/converted` 严格沿用 channel 声明，不把“Pi 自己支持该 provider”冒充“原生透传”。`not_applicable` 用于控制面/负面边界。
- `transports` 区分 HTTP、SSE、WS。Images 的部分 SSE 组合没有逐项证据时显式为 `unverified_per_operation`，不能由通用 `ExecuteStream` 自动推出全面支持。Antigravity Images SSE 明确不支持。
- `release_gate_required=baseline_supported`：每个基线支持项若未独立验证 Pi 就阻断“完整 Pi 等价发布”。API 渠道不需要迁移，但它们的原路径兼容回执必须独立保留；不能简单把 `preserved` 改名 `verified` 绕过门禁。
- `live_verification` 必须有独立 Pi 真链路回执；仅 mock、源码字符串、CPA 测试或相同 HTTP 200 均不充分。负面边界也应有拒绝测试。

## 四订阅渠道矩阵

|渠道|原执行器 / 原生协议|授权与导入|计数|主动配额/模型|WS|
|---|---|---|---|---|---|
|Codex|CPA / Responses；Images 与 search 专用路由|浏览器 OAuth、loopback 回调、OAuth 文件|本地受限文本估算；三个协议计数入口|账户配额、reset credits、模型发现/静态合并；HTTP/WS 被动观测|原生、continuation、prewarm；不支持 stored responses/multiplex|
|Claude|CPA / Anthropic|浏览器 OAuth、PKCE/state、loopback、OAuth 文件|上游 CountTokens，跨协议转换|账户/组织/套餐、usage/额外额度；HTTP 被动观测；模型发现|未声明支持|
|Antigravity|CPA / Gemini|浏览器 OAuth、loopback、OAuth 文件|上游 CountTokens，跨协议转换|账户/plan、模型 quota 与 credits、partial scope；模型发现/静态合并|未声明支持|
|Grok|CPA / Responses，`https://cli-chat-proxy.grok.com`|设备码 OAuth/轮询、OAuth 文件|本地受限文本估算；三个协议计数入口|账户、surface、credits 三类范围；模型发现/静态合并|未声明支持|

四渠道都保持 refresh 分类、身份校验、凭证版本锁/持久化、手动恢复、API root override 及出站 proxy。不要把订阅 `health` 等同于 `OperationProbe`：`internal/control/credential_probe.go:438-440` 明确禁止订阅 test，`internal/control/validation.go:291-294` 跳过订阅周期验证。订阅健康来自执行/认证/额度状态。

### 全部订阅 route tuple

|ID|模式|HTTP / SSE|计数/存储补充|证据|
|---|---|---|---|---|
|`route.codex.openai-completions.chat_completion`|converted|supported / supported|—|`internal/channel/modules/codex.go:48`|
|`route.codex.openai-images.images_generate`|native|supported / unverified_per_operation|—|`internal/channel/modules/codex.go:49`|
|`route.codex.openai-images.images_edit`|native|supported / unverified_per_operation|—|`internal/channel/modules/codex.go:50`|
|`route.codex.openai-responses.responses_create`|native|supported / supported|Stateless|`internal/channel/modules/codex.go:51`|
|`route.codex.openai-responses.responses_input_tokens`|native|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/codex.go:52`|
|`route.codex.openai-responses.web_search`|native|supported / not_applicable|—|`internal/channel/modules/codex.go:53`|
|`route.codex.anthropic.chat_completion`|converted|supported / supported|—|`internal/channel/modules/codex.go:54`|
|`route.codex.anthropic.count_tokens`|converted|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/codex.go:55`|
|`route.codex.gemini.chat_completion`|converted|supported / supported|—|`internal/channel/modules/codex.go:56`|
|`route.codex.gemini.count_tokens`|converted|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/codex.go:57`|
|`route.claude.anthropic.chat_completion`|native|supported / supported|—|`internal/channel/modules/claude.go:44`|
|`route.claude.anthropic.count_tokens`|native|supported / not_applicable|provider_count_tokens|`internal/channel/modules/claude.go:45`|
|`route.claude.openai-completions.chat_completion`|converted|supported / supported|—|`internal/channel/modules/claude.go:46`|
|`route.claude.openai-responses.responses_create`|converted|supported / supported|Stateless|`internal/channel/modules/claude.go:47`|
|`route.claude.openai-responses.responses_input_tokens`|converted|supported / not_applicable|provider_count_tokens|`internal/channel/modules/claude.go:48`|
|`route.claude.gemini.chat_completion`|converted|supported / supported|—|`internal/channel/modules/claude.go:49`|
|`route.claude.gemini.count_tokens`|converted|supported / not_applicable|provider_count_tokens|`internal/channel/modules/claude.go:50`|
|`route.antigravity.gemini.chat_completion`|native|supported / supported|—|`internal/channel/modules/antigravity.go:44`|
|`route.antigravity.gemini.count_tokens`|native|supported / not_applicable|provider_count_tokens|`internal/channel/modules/antigravity.go:45`|
|`route.antigravity.anthropic.chat_completion`|converted|supported / supported|—|`internal/channel/modules/antigravity.go:46`|
|`route.antigravity.anthropic.count_tokens`|converted|supported / not_applicable|provider_count_tokens|`internal/channel/modules/antigravity.go:47`|
|`route.antigravity.openai-completions.chat_completion`|converted|supported / supported|—|`internal/channel/modules/antigravity.go:48`|
|`route.antigravity.openai-images.images_generate`|converted|supported / unsupported|—|`internal/channel/modules/antigravity.go:49`|
|`route.antigravity.openai-responses.responses_create`|converted|supported / supported|Stateless|`internal/channel/modules/antigravity.go:50`|
|`route.antigravity.openai-responses.responses_input_tokens`|converted|supported / not_applicable|provider_count_tokens|`internal/channel/modules/antigravity.go:51`|
|`route.grok.openai-responses.responses_create`|native|supported / supported|Stateless|`internal/channel/modules/grok.go:43`|
|`route.grok.openai-responses.responses_input_tokens`|native|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/grok.go:44`|
|`route.grok.openai-completions.chat_completion`|converted|supported / supported|—|`internal/channel/modules/grok.go:45`|
|`route.grok.anthropic.chat_completion`|converted|supported / supported|—|`internal/channel/modules/grok.go:46`|
|`route.grok.anthropic.count_tokens`|converted|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/grok.go:47`|
|`route.grok.gemini.chat_completion`|converted|supported / supported|—|`internal/channel/modules/grok.go:48`|
|`route.grok.gemini.count_tokens`|converted|supported / not_applicable|local_estimator_restricted_text_input|`internal/channel/modules/grok.go:49`|

### 不能误报为基线支持的边界

四订阅均没有 retrieve/delete/cancel/input_items/compact/passthrough、embedding、rerank、OperationListModels、OperationProbe 的执行路由。模型发现是订阅 utility，不是遗漏的 `OperationListModels`。全局 /v1/models 入口可以列出项目配置模型，但不等于 provider 可执行 ListModels。

- 四订阅 Responses Create 均 `stateless`，不是完整 stored Responses 生命周期；Codex WS continuation 也不能据此宣称 HTTP 跨连接存储语义。
- Images：Codex generation/edit 是 native；Antigravity 仅 generation 转换，拒绝 edit、流式和不支持参数；Claude/Grok 未声明 Images 路由。
- 独立 `/v1/alpha/search` 仅 Codex，HTTP 非流式；不等于 Responses 内置 search tool。证据：`internal/execution/cpa/adapter.go:563-575`、`third_party/cpaembedded/embedded/codex_search.go:33-46`。
- Antigravity 拒绝 Responses image output、非 function 内置 tools、remote image URLs：`internal/execution/cpa/antigravity_provider.go:75-109`。不能为追求表面转换成功而丢弃字段。
- Codex/Grok 的本地 count 有模型与字段白名单，既不是上游真实 token 请求，也不是任意多模态 token estimator。证据：`internal/execution/cpa/codex_provider.go:93-145`、`internal/execution/cpa/grok_provider.go:114-160`。

## Pi 0.85.1 实际发布包：不可省略的差距

审计对象为 npm `@earendil-works/pi-ai@0.85.1`，非旧版印象；发布包仓库 `earendil-works/pi`，gitHead `d981de1229ef899957bbe968bc8dcda02a21f477`。通过注册表读取 tarball 到内存，校验 SHA-512 与 registry integrity 一致，没有安装或执行包。JSON 记录发布包 URL、版本和关键文件 SHA-256；以下 `package/dist/...` 路径指该固定 tarball 内成员，不是本仓库文件。

|必须解决的差距|发布包证据与基线差异|放行条件|
|---|---|---|
|**Antigravity 内置 provider/OAuth 缺口**|`package/dist/providers/all.js:67-109`、`package/dist/auth/oauth/load.js:24-58` 无 Antigravity；发布 JS/types 搜索无 antigravity。基线 module 有八条路由及完整 OAuth/计数/额度|不能用 Google API-key provider 顶替。必须实现并验证该订阅的执行及生命周期；否则仅允许明确不支持的部分 Pi 预览，CPA 保留|
|**Grok 不能再说“Pi 完全不支持”**|`package/dist/providers/xai.js:6-21` 和 `package/dist/auth/oauth/xai.js:158-189` 已有订阅 device OAuth/refresh；但 endpoint 是 `api.x.ai/v1`，基线是 CLI chat proxy；Pi 模型来自静态 `XAI_MODELS`|验证原有凭证 schema/账号 identity、OAuth scopes/client、CLI execution headers/conversation、模型发现和 account/surface/credit quota；存在 OAuth ≠ GPT-Load Grok 全量对齐|
|**图片生成/编辑接口不同**|`package/dist/types.d.ts` 的 KnownImagesApi/ProviderImages 与 `package/dist/providers/all.js:119-122`、`providers/images/register-builtins.js:27-31` 只注册 OpenRouter Images；不是 Codex Images 或 Antigravity 的实现|Codex generate/edit 的 JSON/multipart/usage/stream 边界及 Antigravity image-generation 转换逐项实现验证；输入图片能力不等于图片输出端点|
|**计数不在统一调用中**|整个发布 JS/types 无 countTokens/count_tokens 契约；基线四渠道分别存在本地/上游计数语义|保留准确的本地估算/上游计数区别与拒绝边界；不能拿一次聊天 usage 伪造 count_tokens|
|**独立 Codex search 不在包端点中**|发布 JS/types 无 `/alpha/search`；基线 `codex_search.go` 独立 HTTP 转发，保留 status/read failure metadata|实现专用 search 路径并验证原始响应/错误/取消；不能改写为 LLM 搜索工具|
|**Pi 的 WS 不等于网关 WS 契约**|`package/dist/api/openai-codex-responses.js:182-245` 在 WebSocket 建连失败时自动切 SSE；基线 `third_party/cpaembedded/embedded/codex_websocket_test.go` 有 `TestCodexWSSessionDoesNotFallbackHTTP`|原生 WS 的 handshake、每轮 deadline、prewarm、continuation、隔离、close/error/quota event 需要独立验证；失败不能悄悄退 HTTP。Pi 内部 WS→SSE 与 CPA 回退是两种不同但都必须显式约束的行为|
|**统一消息模型不是四种 wire 协议的无损往返**|`package/dist/types.d.ts` 的 Context/Message/AssistantMessageEvent 只表达归一化消息；GPT-Load `RouteMode` 已承诺 native 或 converted，并有 conversion fidelity 拒绝机制|对 tools/工具选择、thinking/signatures、缓存、别名、usage、server tools、错误和 SSE event 做双向保真与负向测试；不能把归一化生成称为 native raw passthrough|
|**控制面和治理不是 SDK 的流式聊天能力**|基线 `runtime.Driver/ModelDiscovery/QuotaObservation/ResetCreditAction` 为不同契约；`credential_manager.go` 维护锁、身份、持久化和 outcome unknown|保留并逐项适配授权 staging、导入/批量导入、刷新、模型发现、主动/被动 quota、history、reset、代理、健康/重试、日志/计费；若这些仍用 CPA，必须明示为“Pi 数据面 + CPA 控制面”，不可标为全 Pi|
|**多租户网络与缓存隔离**|Pi `ProviderRequestOptions.fetch` 注释明确不影响 WebSocket；包提供 env/timeout/retry/session，而基线按 attempt 冻结 proxy、credential version、continuity 与缓存亲和|HTTP/WS/OAuth/refresh/models/quota 都要覆盖 direct/environment/custom HTTP/SOCKS5；禁止进程级环境变量混用账户代理；核实 SDK retry 不绕过 gateway replay policy|

上述是“使用现成 pi-ai 统一 API 不能直接宣称全等价”的结构性缺口，不是声称通过新代码永远无法实现。其中 Antigravity 内置缺失、图片注册差异、独立 count/search 契约缺失是此版本发布包的静态结论；其余是必须验证的语义差异。所有记录的 Pi 状态仍统一 unknown/unverified，而不是以源码比对代替 live 验证。

## API 渠道保持范围

保留 Bifrost、现有 ProviderKind binding、凭证字段/endpoint policy、每个 native/converted tuple、WS flags 与 utility。`internal/container/container.go:271-294` 是真实组合根；CLIProxyAPI **API 渠道**不等于内部 CPA **执行器**，两者都不能删。Google Vertex 的 Gemini chat/probe 具有 model resolver，不能仅记录默认 converted：`internal/channel/modules/google_vertex.go:48-76`。

|API 渠道|route tuple 数|原始定义（全部 route 在 JSON）|
|---|---:|---|
|`alibaba`|11|`internal/channel/modules/alibaba.go`|
|`anthropic`|14|`internal/channel/modules/anthropic.go`|
|`aws_bedrock`|11|`internal/channel/modules/aws_bedrock.go`|
|`azure_openai`|11|`internal/channel/modules/azure_openai.go`|
|`cliproxyapi`|14|`internal/channel/modules/cliproxyapi.go`|
|`deepseek`|11|`internal/channel/modules/deepseek.go`|
|`gemini`|15|`internal/channel/modules/gemini.go`|
|`google_vertex`|11|`internal/channel/modules/google_vertex.go`|
|`gpt_load`|26|`internal/channel/modules/gpt_load.go`|
|`groq`|11|`internal/channel/modules/groq.go`|
|`moonshotai`|11|`internal/channel/modules/moonshotai.go`|
|`newapi`|16|`internal/channel/modules/newapi.go`|
|`openai`|24|`internal/channel/modules/openai.go`|
|`openai_compatible`|17|`internal/channel/modules/openai_compatible.go`|
|`openrouter`|13|`internal/channel/modules/openrouter.go`|
|`siliconflow`|11|`internal/channel/modules/siliconflow.go`|
|`sub2api`|14|`internal/channel/modules/sub2api.go`|
|`volcengine`|11|`internal/channel/modules/volcengine.go`|
|`xai`|11|`internal/channel/modules/xai.go`|
|`zhipuai`|11|`internal/channel/modules/zhipuai.go`|

## 控制面保持清单

以下列出完整鉴权控制面入口；`/api/groups/:group_id` PUT 为已退休 404 契约，不算支持功能。订阅 test/probe、reset credit 等仍执行原来的按渠道 gate。reveal/download 是受保护管理动作，审计未调用。

|方法|路径|源码路由标识|
|---|---|---|
|GET|`/api/auth/session`|`http.control.auth.session`|
|POST|`/api/credential-stages/authorizations`|`http.control.credential-stages.authorize`|
|POST|`/api/credential-stages/import`|`http.control.credential-stages.import`|
|POST|`/api/credential-stages/import-batch`|`http.control.credential-stages.import-batch`|
|GET|`/api/credential-stages/:stage_id`|`http.control.credential-stages.get`|
|POST|`/api/credential-stages/:stage_id/oauth-callback`|`http.control.credential-stages.oauth-callback`|
|POST|`/api/credential-stages/:stage_id/device-poll`|`http.control.credential-stages.device-poll`|
|DELETE|`/api/credential-stages/:stage_id`|`http.control.credential-stages.cancel`|
|GET|`/api/channels`|`http.control.channels.list`|
|GET|`/api/models`|`http.control.models.list`|
|GET|`/api/model-prices/:id`|`http.control.model-prices.detail`|
|GET|`/api/model-prices`|`http.control.model-prices.list`|
|POST|`/api/model-prices/sync`|`http.control.model-prices.sync`|
|PUT|`/api/model-prices/:id`|`http.control.model-prices.update`|
|POST|`/api/model-prices/:id/reset`|`http.control.model-prices.reset`|
|DELETE|`/api/model-prices/:id`|`http.control.model-prices.delete`|
|GET|`/api/home`|`http.control.home`|
|GET|`/api/home/subscription-accounts`|`http.control.home.subscription-accounts`|
|GET|`/api/home/statistics`|`http.control.home.statistics`|
|GET|`/api/health`|`http.control.health`|
|GET|`/api/logs`|`http.control.logs.list`|
|GET|`/api/logs/:request_id`|`http.control.logs.get`|
|GET|`/api/usage`|`http.control.usage`|
|POST|`/api/route/inspect`|`http.control.route.inspect`|
|GET|`/api/settings`|`http.control.settings.get`|
|PUT|`/api/settings`|`http.control.settings.update`|
|GET|`/api/system/info`|`http.control.system.info`|
|GET|`/api/system/update`|`http.control.system.update`|
|GET|`/api/modern/groups`|`http.control.modern.groups`|
|GET|`/api/modern/groups/usage`|`http.control.modern.groups.usage`|
|GET|`/api/modern/credentials/options`|`http.control.modern.credentials.options`|
|GET|`/api/modern/groups/:group_id/credentials`|`http.control.modern.credentials.list`|
|GET|`/api/modern/groups/:group_id/credentials/:credential_id`|`http.control.modern.credentials.get`|
|GET|`/api/groups`|`http.control.groups.list`|
|GET|`/api/groups/options`|`http.control.groups.options`|
|GET|`/api/groups/:group_id`|`http.control.groups.get`|
|GET|`/api/groups/:group_id/settings`|`http.control.groups.settings.get`|
|POST|`/api/groups`|`http.control.groups.create`|
|PUT|`/api/groups/:group_id/settings`|`http.control.groups.settings.update`|
|PUT|`/api/groups/:group_id`|`http.control.groups.retired-update`|
|GET|`/api/groups/:group_id/models`|`http.control.groups.models.get`|
|PUT|`/api/groups/:group_id/models`|`http.control.groups.models.update`|
|DELETE|`/api/groups/:group_id`|`http.control.groups.delete`|
|GET|`/api/groups/:group_id/credentials`|`http.control.group-credentials.list`|
|POST|`/api/groups/:group_id/credentials/download-all`|`http.control.group-credentials.download-all`|
|GET|`/api/groups/:group_id/credentials/:credential_id`|`http.control.group-credentials.detail`|
|GET|`/api/groups/:group_id/credentials/:credential_id/quota-history`|`http.control.group-credentials.quota-history`|
|POST|`/api/groups/:group_id/credentials/:credential_id/observation-refresh`|`http.control.group-credentials.observation-refresh`|
|POST|`/api/groups/:group_id/credentials/:credential_id/refresh`|`http.control.group-credentials.refresh`|
|POST|`/api/groups/:group_id/credentials/:credential_id/reset-credits/consume`|`http.control.group-credentials.reset-credit-consume`|
|POST|`/api/groups/:group_id/credentials/:credential_id/reveal`|`http.control.group-credentials.reveal`|
|POST|`/api/groups/:group_id/credentials/:credential_id/download`|`http.control.group-credentials.download`|
|PUT|`/api/groups/:group_id/credentials/:credential_id`|`http.control.group-credentials.update`|
|POST|`/api/groups/:group_id/credentials/:credential_id/restore`|`http.control.group-credentials.restore`|
|POST|`/api/groups/:group_id/credentials/:credential_id/test`|`http.control.group-credentials.test`|
|POST|`/api/groups/:group_id/credentials/:credential_id/test/restore`|`http.control.group-credentials.test-restore`|
|POST|`/api/groups/:group_id/credentials/batch`|`http.control.group-credentials.batch`|
|DELETE|`/api/groups/:group_id/credentials/:credential_id`|`http.control.group-credentials.delete`|
|POST|`/api/groups/:group_id/credentials/import`|`http.control.group-credentials.import`|
|POST|`/api/groups/:group_id/credentials/connect/inspect`|`http.control.group-credentials.connect.inspect`|
|POST|`/api/groups/:group_id/credentials/connect`|`http.control.group-credentials.connect`|
|POST|`/api/groups/:group_id/models/discover`|`http.control.group-models.discover`|
|POST|`/api/models/discover`|`http.control.models.discover`|
|POST|`/api/access-keys`|`http.control.access-keys.create`|
|GET|`/api/access-keys/options`|`http.control.access-keys.options`|
|POST|`/api/access-keys/:id/reveal`|`http.control.access-keys.reveal`|
|POST|`/api/access-keys/:id/rotate`|`http.control.access-keys.rotate`|
|GET|`/api/access-keys`|`http.control.access-keys.list`|
|PUT|`/api/access-keys/:id`|`http.control.access-keys.update`|
|POST|`/api/access-keys/:id/cost-limits/reset`|`http.control.access-keys.cost-limits.reset`|
|DELETE|`/api/access-keys/:id`|`http.control.access-keys.delete`|

## 独立验收与发布门禁

1. **默认兼容路径**：Pi 未启用时，CPA 四订阅和 Bifrost API 路径行为、配置、数据迁移、models/quota、管理 UI 全部保持；现有测试必须实际执行。此审计没有执行测试。
2. **逐项 Pi 真路径**：对 JSON 所有基线支持项建立与 ID 绑定的测试回执，记录基线 SHA、Pi 包完整性/版本、driver/backend 实际标识、渠道/operation/transport、输入 fixture 摘要、真实调用及结果、时间、错误/usage/取消等断言。
3. **独立验证**：Pi 与基线使用各自真实驱动；验证运行中断言 CPA 执行器未被调用。仅“返回成功”不够，需证明确实走 Pi。保留 CPA 做对照不属于静默 fallback。
4. **真实服务阶段必须另行授权测试凭证**：本次没有读取任何真实账号或 secrets；offline/mock 通过后，OAuth/refresh/models/quota/HTTP/SSE/WS 的 live 结果独立记录。不能默认沿用生产账号，更不能为了测试消费 reset credits。
5. **失败关闭**：Pi 不支持的 tuple/参数/transport 在发上游之前返回明确“不支持”，记录请求的 driver 与实际 driver；不允许自动进入 CPA 然后标记 Pi 成功。若产品显式支持用户选择的 hybrid，必须可见并保持逐项未验证，不能解除完整等价门禁。
6. **负向和边界**：非预期代理、跨账户 continuation、refresh identity changed、secret version race、处理后断流、计数受限字段、工具语义丢失、unknown usage、不支持 stored responses 都需要行为测试。图片/embedding/rerank 重放遵守 `execution.Operation.ReplayPolicy`。
7. **持久化与运营**：UI channel catalog/driver status、日志、费用、配额 history、健康状态的展示不能把“原能力仍由 CPA/Bifrost 提供”和“Pi 原生实现”混成一个绿色标记。

### 可复查统计（只验证清单，不等价于测试产品）

```python
import json
from collections import Counter
from pathlib import Path
x = json.loads(Path('docs/pi-driver/capabilities.json').read_text())
rows = x['capabilities']
assert len(rows) == len({r['id'] for r in rows}) == x['counts']['capabilities']
assert all(r['pi_status'] == 'unverified' for r in rows)  # 初始审计状态
assert all(v['source_route_declarations'] == v['inventory_route_records']
           for v in x['route_coverage'].values())
print(len(rows), Counter(r['category'] for r in rows))
print('full Pi blocked:', any(r['baseline_supported'] and
      (r['pi_status'] != 'verified' or r['live_verification']['status'] != 'verified'
       or not r['live_verification']['receipt']) for r in rows))
```

计数为冻结基线的审计 receipt，不建议将这些数字写成永久产品 change-detector tests。新增渠道/能力时应比较完整源集合与清单集合，保留唯一 ID，并对真实行为做测试。
