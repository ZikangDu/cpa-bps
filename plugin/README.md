# CPA BPS 插件重建版

`0.5.9-reconstructed` 是依据线上 BPS 0.5.31 的请求记录、配置和二进制符号重建的 Go 插件。它复用 CLIProxyAPI 的原生 C ABI，保留插件 ID `cpa-bps`，通过可配置的 Responses 上游运行。0.5.31 的原始 Go 文件仍需发行方提供。

Quota-aware routing now checks account quota before dispatch and refreshes the observation after an upstream HTTP 429. A confirmed zero balance places the entire credential, across models, into cooldown until all exhausted windows reset. CPA can fail over to another available credential and retain session affinity there. When every credential is cooling down, the current request returns HTTP 429 with `Retry-After` instead of repeatedly generating or keeping the connection open. Use `routing.retry.request-retry: 0` and `routing.retry.max-retry-credentials: 0` to inspect each eligible credential at most once in a request, including pools larger than two accounts; keep `session-affinity-rebind-on: any` for automatic failover. At the recovery time, the plugin checks quota again before sending work.

Missing or stale quota never becomes an invented zero balance. After a rejection, an unknown recovery state is rechecked at five-minute intervals without generation; ordinary rate limiting with confirmed remaining quota and HTTP 403 retain their own diagnoses. Manual and scheduled quality checks use the same quota gate without modifying the saved schedule. A successful reset card requests fresh confirmation; uncertain redemption does not unlock the account.

[0.5.3 local release and isolated validation](../docs/VALIDATION.md). Production was not changed.

## Account display in 0.5.7

Existing BPS OAuth accounts expose email from their stored grant as display
metadata, without reauthorization. The account list plan column prefers recent
quota observations and falls back to the grant plan when necessary. Authorization
status is placed below all login steps on desktop and mobile.
[Display verification](../docs/VALIDATION.md).

## Automatic documents in 0.5.6

PDF and Excel uploads are parsed automatically before model execution. Text,
page images, sheet names, cell coordinates and saved formula values retain
source labels. Standard file_id uploads use the accompanying thin CPA Files
API extension; inline file_data works through the plugin's local worker.

[Document behavior, installation, formats and explicit coverage limits](DOCUMENTS.md).
Mac and Linux each passed 25 end-to-end cases. Live model acceptance stopped
on upstream HTTP 403; it is not marked passed.
[0.5.6 validation report](../docs/VALIDATION.md).

## Automatic request routing in 0.5.5

`bps_tool_transport: auto` is now the default. Ordinary text, image and JSON-only
requests retain the lightweight streaming path. Simple functions initially use
text tools; code/command, custom, namespaced and multiple-tool requests initially
use native tools. The decision uses declared request structure, not prompt
keywords. A successful mode is remembered per account, caller, model, conversation
and tool capability family. A different request family is evaluated afresh.

Complete tool responses are validated before delivery. Alternate local decoding
first tries to interpret the same response without another generation. Only a
known, complete, unexecuted tool-protocol failure can switch transport, at most
once and with at most three generations per selected-account execution. CPA
account failover is a separate bound. HTTP 401/403/429, ambiguous
connections, partial/failed responses, refusals and unknown hosted actions never
trigger protocol replay. The original inline history and tool declarations are
retained. Already-used tool call IDs are rejected.

Observed usage includes discarded complete attempts. Missing/invalid aggregate
counters remain unknown, with `cpa_bps_usage_complete` and `cpa_bps_attempts`
metadata. Explicit `text` and `native` remain diagnostic overrides; users normally
keep `auto`.

[0.5.5 release validation](../docs/VALIDATION.md): 28 offline ABI cases / 73 checks and 8 live CPA groups / 11 client requests passed.

## 0.5.4 Excel 原生工具与协议兼容

新增可选 `bps_tool_transport: native`，与原版一样通过 `run_officejs` 中继客户端工具，支持原版的 FUNCTION_CODE/FUNCTION_CMD、动态工具目录、嵌套命名空间、完整历史与流式终态校验。插件本身不执行 OfficeJS，也不会直接操作工作簿。默认仍为已验证的 `text` 传输。

原生模式使用离线捕获确认的三个 Excel 协议选择头，不替换 OAuth 授权和账号身份。`native_tool_repair_attempts` 为 0–2（默认 2），只对尚未交付客户端的完整原生工具格式错误补充明确的未执行结果，最多总共三次生成；HTTP 拒绝、传输不确定和已经交付的流不会因此重放。原生模式的上下文默认阈值为 200000，显式空数组及自定义配置保留。

同时补齐 agent_message 可见协作上下文及密文省略说明、图片上传 purpose=vision、BPS 文件引用规范化、Anthropic/MCP 图片兼容，并在本地明确拒绝不支持的文档 input_file。[逐项兼容范围与限制](PROTOCOL-COMPATIBILITY.md)。 本轮原版离线对照 57 场景、122 项检查通过；独立临时 CPA 的真实原生模式 8 组、11 个客户端请求通过（gpt-6.1-sol）。[完整验收报告](../docs/VALIDATION.md)。

## 本地构建

需要 Go 1.26、C 编译器和对应平台的 CPA。Linux AMD64 上执行 `make build` 生成 `build/cpa-bps.so`。macOS 上生成 `build/cpa-bps.dylib`。

```sh
make test
make build
python3 test/integration.py --cpa /path/to/cli-proxy-api --plugin build/cpa-bps.so
python3 test/full_integration.py --cpa /path/to/cli-proxy-api --plugin build/cpa-bps.so --auto-refresh
python3 test/independent_bps_oauth.py --cpa /path/to/cli-proxy-api --plugin build/cpa-bps.so
```

集成测试应传入当前系统构建的动态库。测试通过临时目录启动独立 CPA，使用回环服务和合成凭据。基础测试覆盖模型映射与发现、Fast、流式与非流式响应、错误终态和单次上游调用。完整测试增加 OAuth 回调、手动和自动续期、Manager 资源、图片上传、额度查询和重置，以及客户端工具校验。

本地使用 CPA v8.0.75 通过了 macOS ARM64 的端到端验证。Linux AMD64 构建在 Debian Bookworm 中通过原生动态库加载、ABI 注册和关闭测试。

## 配置方式

1. 将动态库放在隔离 CPA 实例的插件目录。原版和重建版采用同一个插件 ID，联调时为重建版使用独立的 `plugins.dir` 和 `auth-dir`。
2. 合并 [配置示例](config.example.yaml) 中的 `plugins` 段。BPS 预设在 `models: []`、`auto_discover: true` 时按账号发现可用模型，`model_suffix: ""` 保留原模型名。手动 `models` 支持模型名或 `name` 与 `alias`。其他 Responses 服务可配置同源 `models_url` 获取模型目录。
3. 按 [凭据示例](credential.example.json) 在测试实例的 `auth-dir` 创建凭据记录。用进程环境变量注入该上游的 Bearer 凭据，`token_env` 保存变量名。可用 `upstream_origin` 将凭据限定在指定协议、主机和端口。填写正式服务时使用 HTTPS。显式的回环 HTTP 地址用于本地夹具。

## 糖果题检测与紧凑账号列表

账号列表在“最近请求”中保留业务请求记录，并增加独立的糖果检测条。主区支持多选本次手动检测模型，历史条汇总全部模型最近 20 条记录；每个选中模型都有独立记录，批次中止后未发送的模型明确标记为“未执行”。切换手动模型不会改写周期计划。默认题面与本地 Sub2API 的 `CandyPrompt` 一致，参考答案为 21；严格判断最终答案，不能因为推理中出现 21 就算通过。绿色表示答案符合，红色表示答案不符，橙色表示请求异常、超时或上游返回模型不一致，灰色表示未测或跳过。记录条与请求记录使用相同的 5×16px 尺寸和 4px 间距，整组靠右、最新一条在最右侧。鼠标悬浮或键盘聚焦可立即查看模型、时间、结果、答案与耗时，点击打开完整题目和记录；单题结果不能证明模型身份或整体能力变化。[题面与判分](internal/candy/README.md)

“周期设置”使用独立小弹窗，可按账号选择多个模型、推理强度、1–10080 分钟间隔及自定义题目/数字参考答案。新账号默认不启用周期；默认间隔为 360 分钟。检测固定在选中的账号，不自动借用其他账号；使用真实凭据和原有账号并发队列，全局一次运行一个检测批次，每个模型最多一次请求、90 秒期限。运行时不改业务账号、模型映射或业务路由。定时检测收到明确的上游使用策略拒绝时会暂停该账号的自动计划。点击“立即检测”始终是独立的手动操作：允许在自动暂停时执行，不修改定时开关、配置、暂停状态或下次计划时间；若上游仍拒绝则保留请求异常并停止本批，不重试或换号。

计划与每模型最近 20 条结果持久保存到 `quality_state_path`（默认 `plugins/cpa-bps-quality.json`），文件权限为 0600，不保存 OAuth 凭据。CPA 后台调度，关闭页面仍运行；进程停止时不执行，恢复后不会补跑所有错过周期。已发出请求的保留期和检测冷却会阻止重启后立即重复发包。页面轮询只读本地状态，模型目录只在首次进入或点击“刷新模型”时查询。

账号行合并了原来多占一格的额度/并发区域，默认紧凑显示。并发上限可直接修改保存；重置卡快捷图标在有新鲜适用额度时可进入确认，不可用时置灰，原因仅在悬停提示中显示。

模型选择优先读取 CPA 为当前凭据登记的目录，自动暂停不会再把目录缩成历史测过的模型。注册目录与实时调用权限分开，页面会说明来源；读取目录不向上游发请求，也不读取另一账号的模型。普通手动检测继续遵守账号启用状态、凭据有效期、并发、冷却及重启保留期。[暂停与模型目录修复验收](../docs/VALIDATION.md)

管理接口均使用 CPA 的管理鉴权与所选 `auth_index`：`GET/PUT /cpa-bps/quality/config`、`POST /cpa-bps/quality/run`、`GET /cpa-bps/quality/results`、`GET /cpa-bps/quality/models`。原生宿主集成验收覆盖异步 202、凭据更新、真实一分钟定时、重启及暂停；仅使用回环假上游。[原生调度验收](../docs/VALIDATION.md)

## 独立 OAuth 与 Manager 授权

独立 BPS 登录使用 `oauth_preset: bps`。预设内置参考项目公开的 BPS 客户端参数、注册回调与 scope，插件为每次登录生成新的 PKCE 和 state。配置示例采用这一模式，`responses_url` 默认使用 BPS 的 Responses 地址。需要代理时，在 `oauth_proxy_url` 填写本机可用代理，令牌交换和续期会使用该代理。

在 Manager 的插件菜单打开 **BPS 账号授权**，生成链接并打开授权页，用测试账号登录。登录完成后，把浏览器地址栏里以 `https://bps.openai.com/basispoints/extension/` 开头的完整回调地址提交到本地授权页。CPA 校验 state 后用该次会话的 PKCE 交换令牌，并把新账号保存到本地凭据目录。回调中的 code 和 state 只应提交到本次授权页。

CPA 保存授权凭据并调度续期，重建版对同一账号和凭据版本合并刷新请求，保存 ID Token、scope 和签发方返回的最早刷新时间。真实 BPS 账号的令牌交换与推理能力需在用户完成登录后验收。BPS 登录预设基于 [参考项目浏览器流程](https://github.com/sunboyzoey/bps-local-public/blob/main/internal/auth/browser.go) 和 [令牌交换](https://github.com/sunboyzoey/bps-local-public/blob/main/internal/auth/device.go)。

其他 OAuth 服务可继续在 `plugins.configs.cpa-bps.oauth` 配置注册客户端的端点、scope 和回调。独立打开 `/v0/resource/plugins/cpa-bps/index.html` 时可用该实例的 CPA 管理密钥。

## 图片、工具和额度

- BPS 预设会将输入转换为 BPS 请求格式，并把上游流式结果转换回 Responses。BPS 内联图片使用同源的附件上传接口，返回的文件引用继续用于该账号请求。
- 当前 BPS 工具目录使用客户端工具的 JSON 消息协议。模型生成调用名称和参数，插件校验声明与 Schema 后转换为 Responses 工具事件，工具结果按同一协议回传。含工具的回合等待终态校验后交付。
- 可选的 `files_url` 接受标准 Files API 格式的图片上传。配置后，内联图片会先校验格式，再上传并以 `file_id` 发送给 Responses 上游。现有 HTTPS 图片和文件 ID 可原样使用。 [Files API](https://developers.openai.com/api/reference/resources/files/methods/create) · [图片输入](https://developers.openai.com/api/docs/guides/images-vision)
- 客户端函数与自定义工具按声明校验名称、命名空间和参数封装。函数参数须为 JSON 对象，自定义工具输入为字符串。工具事件和后续依赖其索引的事件在核对完成响应之后交付。历史密文默认保留。显式开启 `omit_encrypted_history` 可省略旧的独立加密推理项，当前任务和压缩项保持原有内容。
- Manager 额度项展示插件进程观测到的完成请求数和 Token 用量。配置同源 `quota_url` 后，还可读取服务提供方返回的额度窗口与恢复时间，JSON 采用 CPA 的 `QuotaFetchResponse` 结构。SDK 本地重置清理该账号的本地计数和查询缓存，CPA 同时清理该账号的本地调度冷却。服务提供方额度由其自身管理。 [CPA 额度接口](../docs/VALIDATION.md)
- Manager 的账号并发区域通过 `GET/POST /v0/management/cpa-bps/concurrency` 读取处理数、排队数并保存上限。覆盖值保存在 `concurrency_policy_path`，默认文件为工作目录下的 `plugins/cpa-bps-concurrency.json`。上限为零时不限并发。
- BPS 的 `/cpa-bps/quota` 管理接口按选中账号读取真实套餐、5 小时及每周额度窗口、恢复时间、拥有及当前适用的重置卡数量，并保留来源及观察时间。`POST /cpa-bps/reset` 要求 `confirm: true` 和唯一 `redeem_request_id`；只有实时适用数量大于零时提交一次上游兑换。模糊结果不重放，拥有卡片不等于当前可以使用。

Manager 的历史用量和基于请求的健康判断需要完整 Manager 后台采集 CPA 用量队列。本机验收面板运行在 `http://127.0.0.1:18318/management.html`，推理 API 运行在 `http://127.0.0.1:8318/v1`。

套餐列和筛选需要配套 [Manager 源码补丁](../cpa-manager-plan/apps/manager-server/internal/service/proxy/bps_plan.go)。它从插件的 `/cpa-bps/plan-observations` 读取五分钟内的缓存观察值，按凭据索引匹配。插件专用额度卡可以直接读取额度接口。

## 输入缓存与账号亲和

0.4.2 为每个账号、调用方、模型和对话生成稳定的 `prompt_cache_key`。工具目录按名称排序，普通 auto 回合不再追加冗余的临时说明，使完整旧历史能保留为下一轮前缀；required、none 及缩小可调用范围的回合仍附带明确限制并严格校验。结构化输出说明放在历史之前。

配置示例采用粘性会话加自动兜底：`session-affinity: true`、`session-affinity-ttl: 24h`、`session-affinity-rebind-on: any`。同会话、同模型保持账号，使用时续期 24 小时；新会话仍轮询分布，子会话继承父账号。已绑定账号不可用时允许换号，成功后绑定到新账号，不因原账号恢复就跳回。`max-retry-credentials: 0` 允许每次请求遍历可用候选账号、同一账号最多尝试一次，`request-retry: 0` 禁止额外重试轮次。这个 TTL 是本地账号绑定时间，不能延长上游 KV 缓存寿命。

客户端应为每个对话提供唯一且稳定的 `X-Session-ID`，或保持正文 `prompt_cache_key` 不变。宿主确认的稳定会话头优先于每轮变化的正文缓存提示，避免缓存键随提示变化。完整历史被压缩替换后，显式会话标识尤其重要。不要给所有对话使用一个固定标识。

CPA 当前将绑定保存在进程内存；重启或改变路由配置会重建绑定。普通请求不会触发这类重建。部署时合并插件和路由变更，以减少重启次数。

同一上传端点、令牌及图片内容的附件引用最多缓存 5 分钟；令牌或账号变化后重新上传。缓存最多保留 512 个引用，并合并同时上传的相同图片。它只复用附件文件 ID，不缓存生成答案。

缓存率采用上游返回的 `Σ cached_tokens / Σ input_tokens`，冷热请求分别统计。写入缓存的 Token 不算读命中；服务端未返回的计数保留未知。BPS 当前拒绝公开 Responses API 的 `prompt_cache_options`，插件未强行设置保留时长。缓存最终仍受上游服务控制。

```sh
python3 -B test/cache_acceptance.py \
  --base-url http://127.0.0.1:8318 \
  --phase optimized \
  --evidence /path/to/cache-evidence
```

脚本从 `CPA_BPS_TEST_API_KEY` 和 `CPA_BPS_TEST_ADMIN_KEY` 环境变量读取本地密钥，在单个账号上完成 12 次对照请求并恢复原账号状态。

## 0.4.3 额度、重置卡与访问错误修复

详情抽屉的“标准额度”现已独立接入同一 BPS 额度查询接口，修复列表卡有数据而详情显示空窗口的问题。新增“刷新额度”，显示实际 5 小时和每周余量、重置时间、观测时间及暂停状态。保留原凭据总体历史统计；没有关联到窗口内的请求统计不会显示为零。切换账号、管理连接及乱序响应均按凭据作用域隔离，失败时明确标记旧快照。此次只更新面板 HTML，没有重启 CPA 或改变会话路由。[详情页验证](../docs/VALIDATION.md)

剩余五个模型各完成三次真实重复请求。合并前次结果，九个模型都验证了实际响应模型：GPT-5.6 系列、GPT-6 系列及 GPT-6.1 Sol 的热缓存约 99.98%，GPT-6.2 Sol/Astra 约 51.56%/51.57%。这些是相同夹具重复请求的热缓存比例，不是所有工作负载的保证。[九模型结果](../docs/VALIDATION.md)

账号 A 的上游 403 已确认是 `403: This request was blocked by our usage policy.`。插件现在返回 `bps_usage_policy_blocked`，保留真实 403，不再丢弃这个已确认的原因。A 已在本地暂停推理，凭据保留；B 正常服务。上游策略限制没有被解除，需要在官方 Excel/BPS 服务核查或联系支持，不能通过重置卡解除。

额度和卡查询现在独立进行，短暂失败时保留最多五分钟的旧观测并明确标记。页面区分拥有数量、当前适用数量和暂停状态；刷新主动查询，过期或适用性未知时禁用兑换。两个账号实际读取均为拥有 3 张、当前适用 0 张；暂停的账号仍可管理只读查询，推理和兑换保持禁用。

兑换后端及确认页面已实现。合成验证覆盖选定账号、实时适用性、确认/取消、单次提交、相同请求 ID 去重、歧义结果保护和刷新失败。真实 API 与页面读取验证通过，没有消费真实重置卡，因此尚未完成真实的成功兑换验收。歧义结果的本地保护当前在进程内保留一小时，重启后不持久化；页面提示先核对结果，勿重复兑换。

最终 macOS 动态库和面板已安装到本地，Linux AMD64 原生 ABI 验证、race/vet、隔离 OAuth 自动刷新、粘性与故障兜底回归通过。生产实例未修改。[最终报告](../docs/VALIDATION.md)

## 0.4.2 真实缓存复测与原版对照

2026-11-21 用同一份 1,397 词夹具完成 30 次本地正式缓存请求。`gpt-5.6-luna` 的 9 次热请求加权读命中率为 99.9257%，连续对话为 99.8782%；`gpt-6.2-sol` 同样 9 次热请求为 51.7611%。`gpt-6-sol` 和 `gpt-6.1-sol` 各测三次完全重复请求，后两次均约 99.9846%。均核对实际响应模型，首次请求不计入热缓存比例。[完整复测报告](../docs/VALIDATION.md)

原版 0.5.31 二进制在断网容器和合成 TLS 服务中完成 7 次请求构造捕获：首消息不变时 task 保持、turn 随历史变化；无正文缓存键时不发上游缓存键，相同正文换会话头也不改变 task；正文缓存键优先。它采用原生工具中继、尾部时间上下文及默认压缩参数，并将部分缓存写入总数归零，保留 input/cached 数字。[原版离线捕获](../docs/VALIDATION.md)

随后用同一个本地授权账号、`gpt-6.2-sol`、相同输入来源和普通 Go HTTP 传输，各重复三次原版捕获请求体与重建请求体，6/6 完成。原版请求体热命中 51.0991%，重建请求体 51.4162%。这个实验比较请求体，未复刻原版内部身份头、生产网络或原生工具执行，也不代表完整原版链路性能。[请求体 A/B](../docs/VALIDATION.md)

线上本次探针返回限流，已停止追加线上生成。历史原版约 99.15% 的记录实际响应模型为 Luna，不能与当前 Sol 直接同比。本地客户端请求全部完成，但其中四次账号 A 的上游尝试收到 403 后自动转账号 B；失败保留在报告里，A 保持宿主冷却，未手动清除。生产插件和配置均未修改。现有测量支持模型相关差异；Sol 固定未归类输入残差的上游归属仍未确认。

## 0.4.2 粘性会话验收

使用真实 CPA 宿主、0.4.2 动态库及本地合成上游，正文缓存键和会话头两种入口合计完成 38 次客户端请求、40 次上游尝试。同会话连续 6 轮及 4 并发保持同账号；账号 A 返回一次 503 后，当次请求由 B 成功完成，A 恢复后继续绑定 B；停用 B 后转 A，重新启用 B 不回跳。正常成功请求不重复执行，已经输出文字后的断流不自动重放。[隔离测试证据](../docs/VALIDATION.md)

全包 race/vet、隔离 OAuth 续期与 Responses 集成、Linux 原生 ABI 检查通过。0.4.2 已安装到本地实例，并读回确认 24 小时绑定及最多两个凭据的自动兜底配置。本轮没有新增真实 BPS 生成请求，不能把上一版本 51.67% 的热缓存结果当作本版的新测量。[安装与验证记录](../docs/VALIDATION.md)

## 0.4.1 缓存与新一轮验收

同一份 1,397 词夹具的优化前后对照各完成 12 次真实请求。预热请求加权缓存读命中率从 45.83% 提高到 51.67%；工具目录重排场景从 26.10% 提高到 52.10%；连续对话为 51.56%。热请求的缓存写入从 10,553 Token 减少到 104 Token。两批的实际延迟没有改善，不能由缓存命中率推断提速。[缓存对照](../docs/VALIDATION.md)

正文 `prompt_cache_key` 会话测试 8/8 通过，确认固定账号、新会话分流、停用绑定账号后换号保留上下文，以及恢复后的绑定。新一轮功能验收通过双账号 JSON/SSE、A→B→A 文本和图片、跨号工具、长上下文、结构化输出及排队。2 并发通过；5 并发为 3/5，两条收到上游 HTTP 403，停止后续 10/20 并发。独立 Python 客户端的只读诊断收到 Cloudflare 1010，明确为不可重试的浏览器签名拒绝；由于此前 Go 插件未保留错误正文，不能据此确认两条生成失败的具体上游原因。[本轮完整报告](../docs/VALIDATION.md)

最终构建另外修复了同图上传 owner 取消传播，并桥接 CPA 的头部会话身份。全包 race/vet、隔离 OAuth 自动刷新/图片/工具/额度测试及 Linux 原生 ABI 验证通过，最终二进制已安装到本地 CPA。因上述访问拒绝，最终补丁后未追加真实生成压测；本轮也未完成全部 9 模型的新一轮生成复测。两个账号均恢复启用，上限各 50，active/waiting 为零。以下较早验收记录保留其原始日期和结论，不替代本轮未完成项。

## 双账号、模型与并发验收

两个本地授权账号均返回 Team 套餐及 9 个模型。9 个模型已逐一完成真实 SSE 文本请求。双账号验收覆盖各账号流式和非流式、A→B→A 的上下文记忆、跨号工具结果、图片追问、12 轮上下文和 Schema 输出，并验证测试后恢复账号启用状态和原上限 50。[模型验收](../docs/VALIDATION.md) · [双账号验收](../docs/VALIDATION.md)

2、5、10 并发全数通过。首次 20 并发有 19 条在 45 秒测试期限内完成，另一条已经返回正确文本但未收到终态。保留该失败后，使用 90 秒观察窗口复测，20/20 完成，整轮 19.32 秒，观察到两个账号各 active=10。全部请求结束后 active 和 waiting 归零。[首次并发](../docs/VALIDATION.md) · [20 并发复测](../docs/VALIDATION.md)

客户端取消后队列释放和后续请求恢复通过。无效 Key、未知模型、不支持的远端压缩和缺失完整历史均返回明确错误。[边界验收](../docs/VALIDATION.md)

## 2026-11-21 独立 BPS 账号验收

用户在本地 CPA 完成独立 BPS 授权后，`gpt-6.2-sol` 完成了 8 组真实验收，共 11 条生成请求，全部通过。覆盖非流式文本、SSE、多轮记忆、函数工具及结果回传、自定义工具、JSON Schema、内联 PNG 上传识别和双请求排队。客户端工具以合成结果回传。[最终验收](../docs/VALIDATION.md)

并发上限为 1 时，观察到 active=1、waiting=1，全部完成后恢复为零。Manager 后台已采集真实成功请求，账号页显示正常可用，并发上限可保存。[并发证据](../docs/VALIDATION.md) · [Manager 与测试汇总](../docs/VALIDATION.md)

早期测试包含普通 Responses 请求被 BPS 返回 422、旧版原生工具目录导致的调用失败。对应记录保留在 `bps-research/evidence/independent-acceptance`。这轮早期验收使用单个模型，后续双账号验收已扩展模型和并发范围。Linux AMD64 动态库通过构建及原生加载检查，长期稳定性仍待持续观察。

## 用现有 BPS 账号做隔离联调

将正在持有 BPS OAuth 的 CPA 作为 Responses 上游。隔离实例使用已有的客户端 API Key 调用该 CPA，账号授权和刷新由原 CPA 管理。参考项目 `bps-local-public` 的独立 OAuth 和 WebSocket 链路使用单独的授权流程。[参考说明](../docs/VALIDATION.md)

已有上游 CPA 网关时，可将 `responses_url` 指向它的 `/v1/responses`，用 `token_env` 和 `upstream_origin` 绑定该网关的客户端 API Key。设置 `delegated_gateway: true` 后，管理页会显示当前授权由网关托管。

给联调进程注入 `CPA_BPS_LIVE_GATEWAY_TOKEN` 环境变量，值为该上游 CPA 的客户端 API Key。传入上游实际提供的模型名，脚本会创建一次性的本地 CPA 和凭据记录，发送一条流式请求，并记录 HTTP 状态、事件计数和完成时间。

```sh
python3 test/live_responses.py \
  --cpa /path/to/cli-proxy-api \
  --plugin build/cpa-bps.dylib \
  --upstream-url https://gateway.example.com/v1/responses \
  --model your-bps-model \
  --evidence live-responses.json
```

2026 年 11 月 21 日用现有网关的 `gpt-6.2-sol` 联调。首轮在 `response.in_progress` 后出现错误终态，记录不足以归因。复测返回 HTTP 200，3.535 秒收到首段文本，4.152 秒收到唯一的 `response.completed`，测试文本匹配。[首轮记录](../docs/VALIDATION.md) · [通过记录](../docs/VALIDATION.md)

## 行为和代码入口

| 入口 | 行为 |
| --- | --- |
| [cmd/cpa-bps](cmd/cpa-bps/) | CPA 原生动态库注册、RPC 调用、宿主回调和停止 |
| [internal/adapter](internal/adapter/) | 账号和模型路由、Fast 参数、完整历史校验、流式与非流式执行 |
| [internal/admission](internal/admission/) | 按账号独立 FIFO 排队。限额为零时不限并发 |
| [internal/upstream](internal/upstream/) | 标准 Bearer HTTPS、连接复用、模型目录、SSE 解析和 JSON 聚合 |

请求和响应沿用 Responses JSON。文本和常见嵌套图片写法会转换成标准内容项，工具结果要求携带先前的调用，已有的压缩项和加密字段按原值传给上游。SSE 按帧增量发送，明确区分成功终态、错误终态和提前断流。请求提交后的错误交给 CPA 的截断错误路径处理，插件自身只发起一次生成请求。

从 0.5.31 的符号追踪时，可以从 `Runtime.execute` 对照 [请求执行](internal/adapter/execute.go)，从 `accountConcurrency.acquire` 对照 [账号排队](internal/admission/queue.go)，从 `Client.Forward` 和 `Bridge.StreamWithRepairs` 对照 [HTTP 发包](internal/upstream/client.go)与 [SSE 解析](internal/upstream/responses.go)。

重建版采用可配置上游、环境变量凭据和可复用传输。独立接入时，需要用为目标服务签发的凭据验证模型名和正文格式。

## 0.5.31 兼容重建参考

| 功能 | 可参考的实现 | 原版兼容性待核对 |
| --- | --- | --- |
| OAuth 登录与刷新 | `bps-local-public` 有独立的设备码和浏览器登录、凭据过期处理，以及一次刷新函数。独立 CLI 运行时不自动刷新。 [独立授权](../docs/VALIDATION.md) · [刷新实现](../docs/VALIDATION.md) | 现有 CPA OAuth 的授权客户端、凭据持久化与续期归属，以及刷新令牌轮换时的迁移行为 |
| Excel 工具与图片 | 可参考客户端工具白名单、参数和工具结果校验、SSE 事件整理。图片可参考 HTTPS URL 输入和临时图片中转组件。 [事件整理](../docs/VALIDATION.md) · [图片中转](../docs/VALIDATION.md) | Excel 工作簿操作和 0.5.31 的原生文件上传握手 |
| 用量和额度 | 可参考本地并发队列、启动限速和 `Retry-After`。CPA SDK 定义了额度查询与重置的宿主接口。 [本地资源控制](../docs/VALIDATION.md) · [CPA 额度接口](../docs/VALIDATION.md) | 账号实际剩余额度和恢复时间的数据来源，以及原版重置操作影响的状态 |
| 加密历史 | 可参考加密推理和 `agent_message` 的不透明字段校验与保留，以及压缩触发项处理。 [历史转换](../docs/VALIDATION.md) · [消息校验](../docs/VALIDATION.md) | 0.5.31 的历史省略策略、具体密文的归属和上游可接受范围 |
| Manager 授权页 | 授权、回调和状态查询已有原版前端资源可供对照，后端可基于 CPA 原生插件接口实现。 [原版前端脚本](../docs/VALIDATION.md) · [CPA 授权接口](../docs/VALIDATION.md) | 与现有 Manager 的字段和状态语义，以及现有 BPS 授权凭据的兼容性 |

## 参考和授权

- [线上链路核查](../../../DEPLOYMENT.md#2026-11-21-bps-请求与源码核查)和 [0.5.31 Go 符号索引](../docs/VALIDATION.md)用于定位宿主调用、模型处理、并发与发包行为。
- [bps-local-public](../docs/VALIDATION.md) 用于对照输入内容归一化和 SSE 帧兼容行为。其许可文件注明部分协议代码的中间修改条款仍待核实。 [许可说明](../docs/VALIDATION.md)
- [CLIProxyAPI MIT 许可声明](LICENSE-CLIProxyAPI)随采用其原生 ABI 定义的桥接代码保留。
- BPS 请求与工具转换复用了 [oai-basispoints v0.2.10](../docs/VALIDATION.md) 的协议实现，并增加调用方隔离及结构化输出校验。 [组件许可](internal/bpsprotocol/LICENSE)
