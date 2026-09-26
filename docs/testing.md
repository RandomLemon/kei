# 测试、构建与完成定义

本文覆盖 kei 的测试要求（第 16 章）、构建与运行命令（第 17 章）与完成定义（第 19 章）。
不覆盖各功能模块的行为规格（见 `architecture.md`、`engine.md`、`adapter.md` 等），
也不重复 `../README.md` 中的安装、快速开始与联调示例。

---

## 16. 测试要求

1. 每个核心包必须有单元测试。
2. 必须包含集成测试：Mock Adapter -> EventBus -> Router -> Echo 插件 -> Reply。
3. 必须运行 `go test -race ./...`。
4. 必须测试：
   - 命令触发
   - 正则触发
   - 关键词触发
   - 中间件顺序
   - panic recover
   - 超时
   - 去重
   - 同一会话保序
   - 优雅关闭
   - 适配器注册表：未知名字报错并列出已注册名、重复注册名报错、缺 `Name`/`Platforms` 报错
   - 工厂多实例隔离：同平台两个 bot 的实例状态互不影响
   - 权限裁剪：未声明 `storage`/`network` 时注入拒绝式 Storage / nil HTTPClient，未声明 `net_listen` 时 `listen_addr` 导致启动失败
   - 适配器启用开关：`enabled: false` 时不建立外部通道（地址不可达也不 dial）、其 bot 被跳过、其他 bot 不受影响；禁用条目不参与校验
5. 适配器测试使用 `httptest` 或 Mock WebSocket；Capabilities 降级用表驱动测试覆盖 6.4 的每条规则。
6. gRPC 插件测试使用 bufconn。
7. 外部适配器用 bufconn 测试：`Init`/`Start`/`Stop`/`Send`、`EmitEvent` 入队、token 错误被拒、缺 `receive_event` 被拒、适配器崩溃后核心继续服务其他 bot。
8. 第三方适配器接入必须有一个「不改核心代码」的验收测试：只空导入示例适配器包 + 配置 `bots[].adapter` 即可跑通事件到回复。

### 核对结论（按仓库当前实现）

1. **每个核心包必须有单元测试** —— 含源码的 27 个包中 26 个有 `*_test.go`；唯一例外是 `proto/pluginpb`（`buf generate` 产出的生成代码）。模块布局：根 `go.mod`（`github.com/RandomLemon/kei`）之外另有独立 module `examples/kei-adapter-myim`（第三方适配器示例，`replace github.com/RandomLemon/kei => ../..`），它不在根 module 内，因此 `go list ./...` / `go build ./...` / `go test ./...` 在仓库根目录不会覆盖它。（源码/测试文件数见「实际测试分布」。）
2. **Mock Adapter -> EventBus -> Router -> Echo 插件 -> Reply 集成测试** —— `internal/engine/engine_test.go` 的 `TestEchoIntegrationRoundTrip`：注入 `/echo hello`，断言回复文本 `hello`、`Target`（`mock-main` / `mock` / `mock-group`）与 `MessageGroup` 会话类型；`internal/engine/adapter_catalog_test.go` 的 `TestEngineEmitFeedsEventBusAndPlugins` 覆盖外部适配器事件经 `Emit` 进入总线并送达插件。
3. **`go test -race ./...`** —— 统计口径上无覆盖率门槛或 CI workflow 文件（仓库内无 `.github/`）；该命令由 `flake.nix` 的 devShell `shellHook` 与 `../README.md`「测试与验证」列出，作为手动执行约定。
4. **必测项逐条对应**：
   - 命令触发 / 正则触发 / 关键词触发：`internal/router/router_test.go`（`TestDispatchParsesCommand`、`TestOnRegexAndRegistrationErrors`、`TestRegexCaptures`、`TestOnKeywordCaseInsensitive`）、`pkg/bot/registrar_test.go`（`TestRuleMatches` 表驱动）、`plugins/echo/echo_test.go`。
   - 中间件顺序：`internal/middleware/middleware_test.go`（`TestChainOnionOrder`、`TestCombinedChain`）、`internal/router/router_test.go`（`TestMiddlewareOrder`、`TestUseScope`）、`internal/engine/pluginapi_test.go`（`TestGlobalMiddlewareChainOrder`）。
   - panic recover：`internal/middleware/middleware_test.go`（`TestRecoverPanic`、`TestRecoverKeepsError`、`TestRecoverLogsStack`）、`internal/engine/engine_test.go`（`TestHandlerPanicIsIsolated`）、`internal/eventbus/bus_test.go`（`TestHandlerPanicDoesNotKillWorker`、`TestPanicInOneHandlerDoesNotBlockOthers`）、`internal/pluginmgr/manager_test.go`（Setup panic 被隔离为错误）。
   - 超时：`internal/middleware/middleware_test.go`（`TestTimeoutBlocks`、`TestTimeoutFastHandler`、`TestTimeoutDisabled`）、`internal/engine/engine_test.go`（`TestHandlerTimeoutCancelsContext`）、`internal/pluginmgr/external/external_test.go`（`TestHandleEventTimesOutOnSlowPlugin`）、`internal/grpcsrv/server_test.go`（`TestEmitEventCancelledContextReportsContextError`）。
   - 去重：`internal/dedup/dedup_test.go`、`internal/eventbus/bus_test.go`（`TestDuplicateIDHandledOnce`、`TestDedupTTLExpiry`、`TestConcurrentPublishSameIDHandledOnce`）、`internal/middleware/middleware_test.go`（`TestDedupColdAndHot`、`TestDedupFallbackKey`）、`internal/engine/engine_test.go`（`TestDuplicateEventIDIsDropped`）。
   - 同一会话保序：`internal/eventbus/bus_test.go`（`TestPublishPreservesOrderPerSession`、`TestSameSessionLandsOnSingleShard`）、`internal/engine/engine_test.go`（`TestSessionOrderingIsPreserved`）。
   - 优雅关闭：`internal/eventbus/bus_test.go`（`TestCloseDrainsQueuedEvents`、`TestCloseIsIdempotentAndRejectsPublish`、`TestCloseRespectsContext`、`TestCloseWaitsForWorkerExit`）、`internal/engine/engine_test.go`（`TestGracefulShutdownDrainsQueuedEvents`）、`internal/grpcsrv/server_test.go`（`TestServerTCPStartStop`、`TestStopBeforeStartIsSafe`）、`adapters/feishu/feishu_test.go`（`TestStopIdempotent`、`TestStopStopsWorkers`）。
   - 适配器注册表：未知名字报错并列出已注册名 —— `internal/adaptermgr/adaptermgr_test.go` 的 `TestValidateUnknownAdapter`（断言错误含适配器名与「已注册」）；重复注册名报错、缺 `Platforms` 报错 —— 同文件 `TestValidateRegistryRules`（表驱动：注册名重复 / 未声明 Platforms / 声明 `PermAll` / 合法）；缺 `Name` 与 nil 工厂报错 —— `pkg/bot.RegisterAdapter` 把这类注册记入 `bot.AdapterRegistrationErrors()`（`AdapterRegistrationError{Name, Reason}`，原因分别为「注册名为空」「工厂为 nil」，不返回错误，公开 SDK 签名保持稳定），由启动校验 `internal/adaptermgr.ValidateRegistry` 经 `validateRegistrationRejects` 报错终止启动。测试：`pkg/bot/adapter_registry_test.go` 的 `TestRegisterAdapterRejectsInvalid`（断言不入表但被记录 Name/Reason）与 `internal/adaptermgr/adaptermgr_test.go` 的 `TestValidateRegistrationRejects`（无被拒记录不报错；有记录时错误含 `<空名>`/`注册名为空`/`half`/`工厂为 nil`）。
   - 工厂多实例隔离：根 module 内**无直接覆盖**，独立 module `examples/kei-adapter-myim` 的同名用例已覆盖。根 module 最接近的用例有三个：`internal/adaptermgr/adaptermgr_test.go` 的 `TestBuildTrimsDependencies` 用一个配置装配两个 bot，断言各自拿到独立的 `AdapterContext`（`bare-bot` 得到 nil `HTTPClient` 与拒绝式 Storage、`full-bot` 得到注入的依赖，且绑定顺序按配置排列）——隔离的是不同适配器工厂产出的实例，非同一工厂的两次调用；`internal/engine/engine_test.go` 的 `TestPerBotPluginWhitelist`（`mock-a`/`mock-b` 各自独立实例，白名单外 bot 不产生回复）与 `TestMultiBotSamePlatformNeedsExplicitBotID`（同平台多 bot 必须显式指定 `Target.BotID`，两个 bot 的发送记录互不串台）覆盖同平台多 bot 的行为隔离，但经 `startEngine` 直接注入 `*mock.Adapter`，不走注册表工厂。`internal/adaptermgr/external_build_test.go` 的 `TestBuildFailureReleasesPartialAssembly` 是根 module 内唯一「同一适配器名配两个 bot」的装配用例，且属于外部 gRPC 通道（一个 `Client` + 每 bot 一个实例）。真正的同一工厂多实例隔离由 `examples/kei-adapter-myim/myim_test.go` 的 `TestMultiInstanceIsolation` 覆盖：对同一个已注册工厂调用两次，断言返回不同实例、各自监听不同地址、各自只收到 `BotID` 匹配的事件、停止其一不影响另一个（该文件属独立 module，见第 8 条）。
   - 权限裁剪：`internal/adaptermgr/adaptermgr_test.go` 的 `TestBuildTrimsDependencies` 断言未声明 `network` 时 `AdapterContext.HTTPClient` 为 nil、未声明 `storage` 时注入拒绝式存储（`storage.ErrPermissionDenied`），已声明时分别注入真实依赖；`TestValidateReservedOptionRequiresNetListen` 断言未声明 `net_listen` 却配置保留键 `listen_addr` 时 `Validate` 报错；`internal/adaptermgr/external/client_test.go` 的 `TestNewRestrictsPermissionsToGranted` 覆盖外部适配器同一条保留键校验。
   - 适配器启用开关：`internal/adaptermgr/adaptermgr_test.go` 的 `TestBuildSkipsDisabledAdapter`（`enabled: false` 的外部适配器 `grpc_addr` 指向不可达地址也不 dial、其 bot 被跳过并记 warn、其他 bot 正常装配）、`TestBuildSkipsDisabledInProcessAdapter`（进程内适配器仅写 `enabled` 即可禁用，重新启用后立即可用）、`TestValidateSkipsDisabledUnknownAdapter`（禁用条目即使名字不存在也不校验、不阻塞装配，启用后必须报错）。
   - 实例级 `bots[].enabled` 开关（与上一项是两层不同开关，缺省均为 `true`）：配置层 `config.BotConfig.Enabled *bool` + `IsEnabled()`，`enabled` 是核心保留键、不落入 `Settings`，可被 `KEI_BOTS_<NAME>_ENABLED` 覆盖 —— `internal/config/config_test.go` 的 `TestBotEnabled`（缺省启用、显式 false/true、环境变量覆盖、不进 `Settings`）；装配层停用实例不建通道、不装配、也不记未知键告警 —— `internal/adaptermgr/adaptermgr_test.go` 的 `TestBuildSkipsDisabledBot`（同适配器两个 bot 只装配启用的那个，工厂只为启用实例调用，停用实例的未知适配器不参与校验）与 `TestValidateDisabledBotSkipsReservedOption`（启用时配置保留键报错、停用后跳过且可装配）；引擎层停用实例不参与插件白名单收窄、也不出现在规则白名单 —— `internal/engine/engine_test.go` 的 `TestDisabledBotExcludedFromPluginWhitelist`（唯一配白名单的实例被停用后，未配白名单的 bot 视为允许全部插件）与 `TestDisabledBotWhitelistNarrowing`（启用实例的白名单照常收窄，停用实例不出现在 `Rules()` 的 `BotIDs`）；示例配置的飞书 bot 即通过该开关停用 —— `cmd/bot/main_test.go` 的 `TestExampleConfigLoads`（`feishuBot.IsEnabled()` 为 false 且 `enabled` 未落入 `Settings`）。
5. **适配器测试使用 `httptest` 或 Mock WebSocket** —— `httptest` 出现在 `adapters/feishu/feishu_test.go`、`adapters/onebot/onebot_test.go`、`adapters/mock/mock_test.go`、`internal/metrics/metrics_test.go`；WebSocket 用例使用测试内自写的最小 RFC 6455 实现：反向 WebSocket 用 `adapters/onebot/reverse_test.go` 的客户端（`dialWS`/`testWSClient`），正向 WebSocket 用 `adapters/onebot/forward_test.go` 的服务端（`newTestWSServer`/`testWSServerConn`，独立计算 `Sec-WebSocket-Accept`、强制客户端帧带掩码）；两条路径都不复用生产 `wsConn`，避免掩码方向上的对称错误互相掩盖，也未引入第三方 websocket 库。**降级测试并非逐规则表驱动**：`pkg/bot/registrar_test.go` 的 `TestDegrade` 用三个子测试 + 一条多段消息断言 Markdown→纯文本、图片→`[image] url`、At→`@名字`、卡片→`[card] json`、文件→`[file] name url`、引用段被丢弃，以及全能力时原样返回（不拷贝）与 `nil` 入参；`TestSegmentSupports` 才是表驱动（含未知段类型返回 false）。因此 6.4 中 `SegFace` → `[face] id` 与未知段类型 → `[类型]` 两条降级分支在仓库内无断言。
6. **gRPC 插件测试使用 bufconn** —— `internal/pluginmgr/external/external_test.go`（`startBufconn`、`newTestPlugin` 等，覆盖 Init/HandleEvent/Stop/Close）与 `cmd/example-plugin/plugin_test.go`（`bufconn.Listen` + `passthrough:///bufconn`）使用 bufconn；`cmd/example-plugin/integration_test.go` 刻意改用真实 localhost TCP，以覆盖真实监听、拨号与握手路径。
7. **外部适配器 bufconn 测试** —— bufconn 用于 `internal/adaptermgr/external/client_test.go` 的 `TestSupervisorReconnectsAfterAdapterCrash`（`Init`/`Start`/`Send` 的正常路径多走 `127.0.0.1:0` 真实 TCP，由 `startService` 启动）。逐项对应：`Init`/`Start`/`Stop`/`Send` —— `TestNewInitAndMetadata`、`TestInstanceStartSendStop`、`internal/adaptermgr/external_build_test.go` 的 `TestBuildKeepsExternalChannelUsable`；`EmitEvent` 入队 —— `internal/grpcsrv/server_test.go` 的 `TestEmitEventDeliversToHook` 与 `internal/adaptermgr/adaptermgr_test.go` 的 `TestEmitFuncChecksOwnership`（归属校验）、`cmd/example-adapter/integration_test.go` 的 `TestExternalAdapterEndToEnd`（上行到核心钩子）；token 错误被拒 —— `internal/grpcsrv/server_test.go` 的 `TestSendMessageUnauthenticated`/`TestEmitEventUnauthenticated`、`internal/adaptermgr/external/client_test.go` 的 `TestNewRejectsInvalidInit`（「适配器拒绝初始化」）、`cmd/example-adapter/integration_test.go` 的 `TestExternalAdapterRejectsInvalidInit`（缺 token 的 `Init` 被拒）；缺 `receive_event` 被拒 —— `internal/grpcsrv/server_test.go` 的 `TestEmitEventRequiresReceiveEventPermission`、`cmd/example-adapter/integration_test.go` 的 `TestExternalAdapterRejectsMissingReceiveEventPermission`；崩溃容错 —— `TestSupervisorReconnectsAfterAdapterCrash` 断言适配器崩溃期间 `Send` 失败、核心进程继续存活、进程重启后重连并重新 `Start` 实例且 `Send` 恢复；该用例只部署一个 bot，未断言「同时服务其他 bot」。
8. **第三方适配器「不改核心代码」验收测试** —— 两条路径都有覆盖。
   - 同 module 验收：`cmd/bot/thirdparty_adapter_test.go` 的 `TestThirdPartyAdapterEndToEnd`：在测试文件内实现只依赖 `pkg/bot` 的适配器，调用 `bot.RegisterAdapter`（等价于第三方包在 `init()` 中注册），再给出 `bots[]` 配置，经 `adaptermgr.Build` + `engine` + `echo` 插件跑通「适配器 `Emit` 事件 → 回复经适配器 `Send` 返回」。
   - 独立 module 验收：`examples/kei-adapter-myim/` 是一个独立 Go module（`module github.com/example/kei-adapter-myim`，`require github.com/RandomLemon/kei v0.0.0` + `replace github.com/RandomLemon/kei => ../..`），含 `register.go`（`init()` 注册工厂）、`myim.go`（适配器实现）、`myim_test.go`（11 个测试）、`README.md`（真实第三方仓库的目录布局）。它只依赖 `pkg/bot` 与标准库，`grep -rn "kei/internal" examples/` 无结果；`examples/kei-adapter-myim/myim_test.go` 的 `TestMultiInstanceIsolation` 是仓库内唯一对「同一工厂构造出多个互相隔离的实例」的断言（见第 4 条）。它不在根 module 内，`go build/vet/test` 需在该目录下单独执行（`go test ./...` 在仓库根目录不覆盖它）。

### 实际测试分布

统计口径：`*_test.go` 数量与顶层 `^func Test` 数量（不含 `t.Run` 子测试）。根 module（`github.com/RandomLemon/kei`）共 39 个测试文件、394 个测试函数、约 16490 行测试代码；独立 module `examples/kei-adapter-myim` 另有 1 个测试文件、11 个测试函数、744 行（下表末行单列，不计入根 module 合计，`go test ./...` 在仓库根目录不会执行它）。

| 包 | 测试文件 | 测试函数 | 覆盖的关键行为（由测试函数名归纳） |
| --- | --- | --- | --- |
| `adapters/feishu` | `feishu_test.go`、`register_test.go` | 36 | 元信息与能力清单、工厂装配与缺 `network` 权限拒绝；challenge、签名校验、AES 加密事件、非法 token 拒绝；文本/At/图片/文件/富文本转换、未知事件类型按 notice、事件 ID 回退；Start/Stop 生命周期与幂等、队列溢出不阻塞、Stop 等待投递；发送（群/私聊/回复/卡片/图片上传/超长分段/中途失败/API 错误）、令牌缓存与并发单飞、令牌提前刷新、图片大小限制 |
| `adapters/mock` | `mock_test.go`、`register_test.go` | 13 | 元信息与工厂（缺省 platform、不要求 network）；未启动时注入报错、注入填充默认字段、发送记录与 `WaitSent`/`Reset`、HTTP 控制面 `/inject` `/sent` `/healthz` 与非法 JSON、无 `listen_addr` 时无控制面、停止后拒绝注入、默认全能力、`Send` 尊重 ctx |
| `adapters/onebot` | `onebot_test.go`、`mode_test.go`、`register_test.go`、`reverse_test.go`、`forward_test.go` | 40 | 群消息数组与私聊 CQ 串解析、事件类型与确定性 ID、鉴权与畸形请求、先响应后投递、发送降级（Markdown→文本、卡片→文本）、私聊与目标推断、发送错误、自定义路径与停止；`ws_path`/`ping_interval` 落到实例、缺 `network` 拒绝；`mode` 归一与必填键校验（`forward_http`/`reverse_http`/`forward_ws`/`reverse_ws`、大小写与空白不敏感、无关键忽略）、按拓扑只挂实际入口（HTTP 上报 204 且 `ws_path` 404、反向 WS 握手 101 且 `path` 404、`forward_ws` 不监听任何端口）；反向 WebSocket 握手与 `Accept` 校验、非法握手拒绝、鉴权、事件上行与分片、经连接发送、优先 WebSocket、并发响应按 echo 匹配、role 回退、`self_id` 过滤、多连接、心跳与半开连接回收、协议错误、停止关闭连接、客户端断开解除发送阻塞；正向 WebSocket 客户端握手（请求行与升级头、`Authorization`、`Sec-WebSocket-Accept` 校验失败的三类拒绝）、掩码方向、事件上行、按 echo 发送、断线重连后仍可发送、停止关闭连接 |
| `cmd/bot` | `main_test.go`、`thirdparty_adapter_test.go` | 12 | 装配表驱动（mock/onebot/feishu/未注册适配器）、插件启用筛选、权限解析、外部插件 spec 校验（token 必填、通道键不透传）、缺 `grpc.addr` 报错、令牌重复/空令牌/插件与适配器同名拒绝、忽略非通道适配器、TLS 与 mTLS 配置、示例配置可加载且 mock bot 设置可用、示例配置飞书 bot 经 `enabled: false` 停用且 `enabled` 不落入 `Settings`；第三方适配器端到端 |
| `cmd/example-adapter` | `integration_test.go` | 4 | 外部适配器端到端（`Send` 经适配器进程、`EmitEvent` 上行、未知 bot 拒绝、关闭下发 `Stop` 与 `Shutdown`）、缺 `receive_event` 被拒、非法 `Init` 被拒、`Start`/`Stop` 幂等与关闭后拒绝 |
| `cmd/example-plugin` | `integration_test.go`、`plugin_test.go` | 21 | 命令往返端到端、坏 token 拒绝、坏 token 阻断 `SendMessage`、mTLS 与缺客户端证书拒绝、真实二进制端到端、插件先于核心启动、缺必填参数报错；bufconn 上的插件 Init 校验、命令/关键词回复、未匹配忽略、发送失败上报、`Shutdown`、回复内容规则、选项校验 |
| `internal/adaptermgr` | `adaptermgr_test.go`、`external_build_test.go` | 18 | 注册表校验表驱动、未知适配器报错并列出已注册名、被拒注册报错（`TestValidateRegistrationRejects`）、保留键需 `net_listen`、权限裁剪、未知配置键与未声明平台告警、工厂失败与 nil 实例、禁用适配器跳过（外部不 dial / 进程内跳过 / 其他 bot 不受影响）、禁用条目跳过校验、实例级 `bots[].enabled` 停用（`TestBuildSkipsDisabledBot`、`TestValidateDisabledBotSkipsReservedOption`）、`Emit` 归属校验、`Close` 幂等；外部通道装配后仍可用、失败时不返回部分绑定且可重新装配 |
| `internal/adaptermgr/external` | `client_test.go` | 10 | `Init` 与元信息、权限取授予集合、非法 `Init` 表驱动、显式平台必须被上报、实例 `Start`/`Send`/`Stop`、重连重启实例与停用后拒绝发送、`Close` 停止实例并通知 `Shutdown`、bufconn 模拟崩溃后重连、重连指标计数（`TestSupervisorReportsAdapterMetrics`：崩溃→失败、恢复→成功、停用计数）与 `Deps.Recorder` 为 nil 时不 panic（`TestSupervisorWithoutRecorderDoesNotPanic`） |
| `internal/config` | `config_test.go` | 25 | 完整 YAML 与默认值、插件标量简写与引号布尔、JSON 往返、环境变量覆盖与类型推断、最长 bot 名匹配、校验错误表驱动（缺 bot/空名/非法 adapter/重名/未知键）、日志级别、gRPC TLS、限流与 admin 环境覆盖、`adapters` 段默认/启用/环境覆盖/校验、实例级 `bots[].enabled`（`TestBotEnabled`：缺省启用、显式 false/true、`KEI_BOTS_<NAME>_ENABLED` 覆盖、`enabled` 不落入 `Settings`） |
| `internal/dedup` | `dedup_test.go` | 7 | 重复检测、空 ID 不去重、TTL 过期复用、GC、容量淘汰最旧、nil 安全、并发无竞态 |
| `internal/engine` | `engine_test.go`、`pluginapi_test.go`、`adapter_catalog_test.go` | 24 | Echo 全链路集成、优雅关闭排空、同会话保序、重复 ID 丢弃、panic 隔离、超时取消 ctx、每 bot 插件白名单、Options 校验、发送解析与重试、空消息/降级后为空报错、同平台多 bot 需显式 `BotID`、重复 `Run`、无事件回复失败、适配器启动失败上抛、实例级 `enabled: false` 的 bot 不参与白名单收窄也不进规则白名单（`TestDisabledBotExcludedFromPluginWhitelist`、`TestDisabledBotWhitelistNarrowing`）；权限门（`send_message`）、全局中间件顺序、管理员判定、发送限流、`Emit` 指标与 nil 拒绝、模板默认值；适配器目录快照隔离与插件可见目录、`Emit` 进入总线并送达插件 |
| `internal/eventbus` | `bus_test.go` | 23 | 会话内保序、同会话落同一分片、不同会话并发、重复 ID 只处理一次、空 ID 不去重、TTL 过期、队列满不阻塞、`Close` 排空/幂等/遵守 ctx/等待 worker 退出、订阅顺序、nil 拒绝、panic 不影响 worker、`Emit` 等价 `Publish`、`Stats` 计数与快照、并发同 ID 只处理一次、分片路由确定且在范围内、`Publish` 不改事件、默认值 |
| `internal/grpcsrv` | `server_test.go` | 34 | `SendMessage` 成功/回复路径/无 `RequestSender` 回退/携带 `BotID`/私聊需 `user_id`/非法段数据/未认证/权限不足/业务错误、`Target` 与事件 proto 往返、`GetConfig` 作用域、`Log` 字段与坏字段、`EmitEvent` 钩子/权限/令牌类别/缺钩子/缺事件/钩子错误/未认证/ctx 取消、适配器令牌可见配置、未知令牌类别拒绝、TCP 启停与 goroutine 不泄漏、TLS 接受受信客户端、Stop 幂等与注入监听器关闭、Options 校验、ctx 取消强制停止 |
| `internal/metrics` | `metrics_test.go` | 9 | Content-Type、计数器、直方图、空注册表（含 `kei_adapter_reconnects_total`/`kei_adapter_disabled_total` 两个新族的 HELP/TYPE 断言）、外部适配器重连与停用计数器标签与累加（`TestAdapterMetrics`）、标签转义、nil 接收者、接口满足、并发记录 |
| `internal/middleware` | `middleware_test.go` | 22 | 洋葱顺序、空链直通、Recover（panic/保留错误/日志堆栈）、Logger 字段与级别、Timeout（阻塞/快速/禁用）、Dedup（冷热/无路由/回退键/禁用）、RateLimit（耗尽/作用域/禁用）、Auth、Metrics（进入链时上报 `RuleMatched`，`TestMetricsMiddleware`/`TestCombinedChain` 断言命中次数；禁用时不上报）、组合链 |
| `internal/pluginmgr` | `manager_test.go` | 6 | 生命周期顺序与逆序停止、重复与非法插件、Setup/Start 失败阻断启动（含 panic 隔离）、权限作用域上下文、插件目录、缺 Registrar 工厂报错 |
| `internal/pluginmgr/external` | `external_test.go` | 20 | bufconn 上 `Init` 请求与配置 JSON 边界、插件拒绝/`Init` RPC 失败/nil 连接、空地址、地址不可达快速失败、等待晚启动的插件、元信息权限隔离、注册单一兜底规则、事件转换与结果、慢插件超时、上游取消传播、`Stop` 只通知一次 Shutdown、`Close` 幂等与连接资源释放 |
| `internal/ratelimit` | `ratelimit_test.go` | 7 | 禁用放行、突发与补充、`AllowN`、`Wait` 阻塞与 ctx、桶回收、并发 `Allow` |
| `internal/reply` | `reply_test.go` | 3 | 累积段发送、错误累积、默认私聊 |
| `internal/router` | `router_test.go` | 27 | 命令解析与自定义前缀、优先级顺序、多规则命中、无匹配、注册校验与错误、正则/关键词（大小写不敏感）、事件类型与平台过滤、`OnAll` 兜底、中间件顺序与作用域、中间件读取路由、触发描述、正则捕获、per-rule Reply、Reply 工厂回退、错误聚合、自动 ID、规则副本隔离、`UpdateRules` 重排与 nil 拒绝、并发派发与注册 |
| `internal/storage` | `memory_test.go` | 6 | 内存往返、TTL、ctx 取消、`Close` 幂等、拒绝式存储、并发访问 |
| `pkg/bot` | `adapter_registry_test.go`、`api_test.go`、`event_test.go`、`registrar_test.go` | 16 | 非法注册被记录并由启动校验报错（`TestRegisterAdapterRejectsInvalid`：不入表但记录 Name/Reason）、注册表快照隔离（入参/出参均不污染）、权限判定（`PermAll` 不适用于适配器）、`Config` 访问器、`BotAPI`/`Storage` 接口满足、`PluginContext` 往返、`NoopReply`、`TargetFromEvent`、`SessionKey`/命令解析/事件文本、段支持判定、能力降级、规则匹配表驱动、元信息权限、插件注册忽略 nil |
| `pkg/message` | `message_test.go` | 3 | 段构建器（文本/图片/At/表情/引用/卡片）、消息构建器、`New` 复制段 |
| `plugins/echo` | `echo_test.go` | 3 | 拼接参数回复、无参数显示用法、元信息与生命周期 |
| `plugins/manage` | `manage_test.go` | 5 | `/ping` 与 `/version`、`/plugins` 列表、`/adapters` 列表、管理员规则、优先级胜过兜底插件 |
| `proto/pluginpb` | 无 | 0 | `buf generate` 生成代码，无测试 |
| `examples/kei-adapter-myim`（独立 module） | `myim_test.go` | 11 | 注册表查表、工厂缺 HTTPClient 拒绝、`Options` 校验、`Start`/`Stop` 生命周期与停止前调用安全、回调先 ACK 再 `Emit`、事件上行、事件 ID 回退、**同一工厂多实例隔离**（`TestMultiInstanceIsolation`：两次调用返回不同实例、各自监听不同地址、事件 `BotID` 不串台、停其一不影响另一个）、发送路径与请求体（含降级）、发送错误 |

---

## 17. 构建与运行命令

工具链全部由 `flake.nix` 提供，无需在系统安装 Go：devShell 包含 `go`、`gopls`、`gotools`、`golangci-lint`、`delve`、`protobuf`（`protoc`）、`protoc-gen-go`、`protoc-gen-go-grpc`、`buf`、`jq`、`curl`、`git`，并通过 `env.GOTOOLCHAIN = "local"` 禁止 `go` 自动下载其它 toolchain。进入环境有两种方式：`nix develop`，或 `direnv allow .` 后由 `.envrc` 的 `use flake` 在进入目录时自动加载。

| 命令 | 执行目录 | 说明 |
| --- | --- | --- |
| `gofmt -w .` | 仓库根目录 | 格式化 Go 源码 |
| `go build ./...` | 仓库根目录 | 构建全部包 |
| `go vet ./...` | 仓库根目录 | 静态检查 |
| `go test ./...` | 仓库根目录 | 单元测试与集成测试 |
| `go test -race ./...` | 仓库根目录 | 竞态检测（第 16 章第 3 条） |
| `golangci-lint run ./...` | 仓库根目录 | 附加静态检查；仓库无 `.golangci.yml`，使用默认 linter 集 |
| `cd proto && buf lint` | `proto/` | proto 规范校验；`proto/buf.yaml` 使用 `STANDARD`，并显式关闭 `PACKAGE_DIRECTORY_MATCH`、`PACKAGE_VERSION_SUFFIX`、`RPC_REQUEST_STANDARD_NAME`、`RPC_RESPONSE_STANDARD_NAME`、`RPC_REQUEST_RESPONSE_UNIQUE` |
| `cd proto && buf generate` | `proto/` | 按 `proto/buf.gen.yaml` 重新生成 `proto/pluginpb`（生成代码已提交） |
| `go run ./cmd/bot -config configs/config.yaml` | 仓库根目录 | 运行入口；`-config` 相对当前目录，另有 `-version` |
| `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` | `examples/kei-adapter-myim/` | 独立 module 必须单独构建与测试；在仓库根目录执行上述命令不会覆盖它（`replace github.com/RandomLemon/kei => ../..` 指向本地根 module，无需先发布） |
| `nix develop` | 仓库根目录 | 进入 devShell |
| `nix develop --command <cmd>` | 仓库根目录 | 在 devShell 内一次性执行命令，例如 `nix develop --command go test -race ./...` |
| `nix build` | 仓库根目录 | 构建 `result/bin/bot` 与 `result/bin/example-plugin` |
| `nix run . -- -config configs/config.yaml` | 仓库根目录 | 直接运行默认 app；`nix run .#example-plugin` 运行示例插件 |
| `nix flake check` | 仓库根目录 | 触发 `checks.default`（即 `packages.kei`） |
| `nix fmt` | 仓库根目录 | 用 `nixfmt-tree` 格式化 `.nix` 文件 |

辅助进程同样用 `go run` 启动（完整用法与 `curl` 联调流程见 `../README.md`）：

```bash
# 仓库根目录，devShell 内：外部适配器示例进程
go run ./cmd/example-adapter -listen 127.0.0.1:19071 -platform example -control 127.0.0.1:19081

# 仓库根目录，devShell 内：外部插件示例进程（-listen/-core-addr/-token 必填）
go run ./cmd/example-plugin -listen 127.0.0.1:19071 -core-addr 127.0.0.1:19070 -token change-me
```

仓库内没有 `Makefile`、`justfile`、`Taskfile`，也没有 `.github/` 工作流：全部命令直接调用 `go`、`buf` 或 `nix`。`../README.md` 的「测试与验证」用 `nix develop --command ...` 前缀给出 `go build`、`go vet`、`go test`、`go test -race` 与 `cd proto && buf lint`，与在 devShell 内直接执行等价；`gofmt -w .` 与 `golangci-lint run ./...` 只在 `AGENTS.md` 与 flake 工具链清单中出现，README 未把它们列为命令。

---

## 19. 完成定义（Definition of Done）

一个功能被认为完成，必须满足：

- 代码已实现并合并到主分支。
- 有单元测试或集成测试覆盖。
- `go build ./...` 通过。
- `go test ./...` 通过。
- `go test -race ./...` 通过。
- `go vet ./...` 通过。
- 公开接口有注释。
- 示例配置可运行。
- 文档或注释说明如何使用。
- 不引入未声明的外部依赖。
- 不破坏 `pkg/bot` 的向后兼容性。
- 新适配器可在不修改核心代码的前提下接入：进程内注册（独立包/独立 module）或外部 gRPC 进程，并有测试覆盖。

**合并前硬性门槛**：`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 四条必须全部通过，缺一不可；仓库无 CI workflow，这四条在 devShell 内手动执行。

### 逐条可自动化验证情况

| 条目 | 验证方式 |
| --- | --- |
| 代码已实现并合并到主分支 | 人工与 git 状态，不可自动验证 |
| 有单元测试或集成测试覆盖 | 人工评审；仓库无覆盖率门槛与覆盖率工具配置 |
| `go build ./...` 通过 | 可自动化 |
| `go test ./...` 通过 | 可自动化 |
| `go test -race ./...` 通过 | 可自动化 |
| `go vet ./...` 通过 | 可自动化 |
| 公开接口有注释 | 人工评审；无启用的 lint 规则强制（仓库无 `.golangci.yml`） |
| 示例配置可运行 | 部分自动：`cmd/bot/main_test.go` 的 `TestExampleConfigLoads` 校验 `configs/config.yaml` 可加载、含 `mock`/`feishu`/`onebot` 三个适配器示例与启用的 `echo`/`manage` 插件、且 mock bot 配了 `listen_addr` 与 `platform: mock` |
| 文档或注释说明如何使用 | 人工评审 |
| 不引入未声明的外部依赖 | 可自动核对：`go.mod` 直接依赖只有 `google.golang.org/grpc`、`google.golang.org/protobuf`、`gopkg.in/yaml.v3`；测试不使用 `testify`，全部基于标准库 `testing` 与 `t.Run` |
| 不破坏 `pkg/bot` 的向后兼容性 | 人工评审；仓库无 API 兼容性差异检查工具 |
| 新适配器可在不修改核心代码的前提下接入 | 可自动化：`cmd/bot/thirdparty_adapter_test.go` 的 `TestThirdPartyAdapterEndToEnd`（进程内注册 + 配置）、`cmd/example-adapter/integration_test.go` 的 `TestExternalAdapterEndToEnd`（外部 gRPC 进程）、`examples/kei-adapter-myim`（独立 module：只依赖 `pkg/bot`，`grep -rn "kei/internal" examples/` 无结果，含 11 个测试） |

## 相关文档

- [项目概述与架构](architecture.md)
- [核心引擎与事件总线](engine.md)
- [实现阶段与交付物](roadmap.md)
- [文档索引](README.md)
