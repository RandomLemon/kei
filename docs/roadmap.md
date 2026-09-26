# kei 实现阶段与交付物现状

本文记录第 14 章（实现阶段与任务清单）与第 20 章（最终交付物）在仓库中的落地现状：逐条标注每个阶段任务与每项交付物的实际状态，并给出可核对证据（包路径、文件与行、符号名）。

不覆盖：各模块的接口设计与实现细节（见 `docs/architecture.md`、`docs/domain-model.md`、`docs/adapter.md`、`docs/engine.md`、`docs/plugin.md`、`docs/grpc.md`、`docs/configuration.md`），以及面向用户的使用方式（见 `../README.md`）。测试与构建的执行结果不在本文断言范围内，完成定义见 `docs/testing.md`，硬性规则见 `AGENTS.md`。

---

## 14. 实现阶段与任务清单

### 阶段 1：项目初始化与核心接口

- [x] 已完成：创建 `go.mod`，模块路径 `github.com/RandomLemon/kei`。
  - 证据：`go.mod:1`（`module github.com/RandomLemon/kei`，`go 1.25.0`；直接依赖 `google.golang.org/grpc`、`google.golang.org/protobuf`、`gopkg.in/yaml.v3`）。
- [x] 已完成：创建目录结构。
  - 证据：`pkg/`（`bot`、`message`）、`internal/`（`engine`、`adaptermgr`、`adaptermgr/external`、`grpcsrv`、`router`、`middleware`、`eventbus`、`pluginmgr`、`pluginmgr/external`、`config`、`metrics`、`ratelimit`、`dedup`、`reply`、`storage`）、`adapters/`（`feishu`、`onebot`、`mock`）、`plugins/`（`echo`、`manage`）、`proto/`（含 `pluginpb`）、`cmd/`（`bot`、`example-adapter`、`example-plugin`）、`configs/`。
- [x] 已完成：在 `pkg/bot` 定义 Event、Message、Segment、User、Channel、Command。
  - 证据：`pkg/bot/event.go:27`（Event）、`pkg/bot/event.go:80`（Command）、`pkg/bot/message.go:19`（Message）、`pkg/bot/message.go:76`（Segment）、`pkg/bot/user.go:4`（User）、`pkg/bot/user.go:14`（Channel）。
- [x] 已完成：在 `pkg/bot` 定义 Adapter、Plugin、Registrar、Reply、BotAPI、Storage。
  - 证据：`pkg/bot/adapter.go:10`（Adapter）、`pkg/bot/adapter.go:28`（EventSink）、`pkg/bot/plugin.go:13`（Plugin）、`pkg/bot/registrar.go:98`（Registrar）、`pkg/bot/reply.go:9`（Reply）、`pkg/bot/api.go:36`（BotAPI）、`pkg/bot/api.go:23`（Storage）。
- [x] 已完成：在 `pkg/bot` 定义 Adapter 注册表：`AdapterFactory`、`AdapterContext`、`AdapterMetadata`、`RegisterAdapter`、`RegisteredAdapters`、`LookupAdapter`（与插件注册表 API 对称）。
  - 证据：`pkg/bot/adapter_registry.go:19`、`:25`、`:41`、`:122`、`:154`、`:167`；插件侧对应 `pkg/bot/plugin.go:79`（RegisterPlugin）、`pkg/bot/plugin.go:89`（RegisteredPlugins）。
- [x] 已完成：编写接口单元测试或编译测试。
  - 证据：`pkg/bot/adapter_registry_test.go`、`api_test.go`、`event_test.go`、`registrar_test.go`，共 16 个 `Test*`；`pkg/message/message_test.go` 3 个。
- [x] 已完成：验收：`go build ./...` 通过。
  - 证据：`go list ./...` 可解析出 26 个包（含全部 `pkg/`、`internal/`、`adapters/`、`plugins/`、`cmd/`、`proto/pluginpb`；`examples/` 下是独立 module，不计入）；命令执行属质量门，见 `docs/testing.md`。

交付物：`go.mod`、`go.sum`、`pkg/bot/`、`pkg/message/`、`configs/config.yaml`、`flake.nix`、`flake.lock`、`.envrc`、`.gitignore`、`LICENSE`。

### 阶段 2：核心引擎与事件总线

- [x] 已完成：实现 `internal/eventbus`，支持 Publish/Subscribe、worker pool、去重、保序。
  - 证据：`internal/eventbus/bus.go:161`（Publish）、`:207`（Subscribe）、`:118`（New）、`:269`（Stats）；worker pool 为分片 worker（`Options.Workers`，默认 4，`bus.go:26`），按 `bot.Event.SessionKey` 哈希入同一分片串行处理（`bus.go:280`、`:334`）保序；去重复用 `internal/dedup`（默认 TTL 5m、容量 4096）；队列满丢弃并返回 `ErrQueueFull`。测试 23 个（`internal/eventbus/bus_test.go`）。
- [x] 已完成：实现 `internal/router`，支持命令、正则、关键词、事件类型匹配。
  - 证据：`internal/router/registrar.go:35`（OnCommand）、`:47`（OnRegex）、`:61`（OnKeyword）、`:70`（OnEvent）、`:78`（OnAll 兜底）。测试 27 个（`internal/router/router_test.go`）。
- [x] 已完成：实现 `internal/middleware`，至少包含 Recover、Logger、Timeout、Dedup。
  - 证据：`internal/middleware/middleware.go:56`（Recover）、`:91`（Logger）、`:164`（Timeout）、`:192`（Dedup）；另有 `:144`（Metrics）、`:216`（RateLimit）、`:246`（Auth），组合器 `internal/middleware/chain.go:13`（Chain）、`:32`（Apply）。测试 22 个。
- [x] 已完成：实现 `internal/engine`，串联 Adapter、EventBus、Router、Plugin；引擎只接收 `AdapterBinding`，不感知平台名。
  - 证据：`internal/engine/engine.go:48`（AdapterBinding）、`:72`（`Options.Adapters`）、`:144`（New）、`:305`（Run）；`internal/` 与 `cmd/` 的非测试代码中不存在平台名分支（唯一出现的平台名是 `cmd/bot/main.go:33-37` 的空导入与 `pkg/bot/adapter.go:11` 的注释示例）。测试 24 个。
- [x] 已完成：编写单元测试。
  - 证据：`internal/eventbus/bus_test.go`（23）、`internal/router/router_test.go`（27）、`internal/middleware/middleware_test.go`（22）、`internal/engine/engine_test.go` 与同包的 `adapter_catalog_test.go`/`pluginapi_test.go`（合计 24）。
- [x] 已完成：验收：`go test ./...` 通过。
  - 证据：上述四个包的测试文件齐备；执行结果属质量门，见 `docs/testing.md`。

交付物：`internal/eventbus/`、`internal/router/`、`internal/middleware/`、`internal/engine/`，以及被引擎复用的 `internal/dedup/`、`internal/ratelimit/`、`internal/storage/`、`internal/reply/`、`internal/metrics/`。

### 阶段 3：Mock Adapter 与 Echo 插件

- [x] 已完成：实现 `adapters/mock`，支持手动注入事件和捕获发送消息，并在 `init()` 中注册工厂。
  - 证据：`adapters/mock/mock.go:192`（Inject）、`:211`（InjectText）、`:329`（`POST /inject`）、`:330`（`GET /sent`）；`adapters/mock/register.go:15` 在 `init()` 中 `bot.RegisterAdapter`。测试 13 个。
- [x] 已完成：实现 `plugins/echo`。
  - 证据：`plugins/echo/echo.go:12`（Plugin）、`:18`（Metadata）、`:44`（`init()`，`:45` 调用 `bot.RegisterPlugin`）。测试 3 个。
- [x] 已完成：在 `cmd/bot/main.go` 中通过空导入 + `internal/adaptermgr` 装配 Mock Adapter 并启动 Engine，不得出现平台名分支。
  - 证据：`cmd/bot/main.go:24`（导入 `internal/adaptermgr`）、`:33-37`（空导入 `adapters/mock`、`plugins/echo` 等）；适配器按 `bots[].adapter` 经 `adaptermgr.Build` 装配。
- [x] 已完成：编写集成测试：注入 `/echo hello`，断言回复 `hello`。
  - 证据：`internal/engine/engine_test.go:172`（TestEchoIntegrationRoundTrip，注入 `/echo hello` 后断言发送内容为 `hello`、目标为 `mock-main`/`mock`/`mock-group`）。
- [x] 已完成：验收：`go test -race ./...` 通过。
  - 证据：全仓 40 个测试文件共 405 个顶层 `Test*`（根 module 394 个 + `examples/kei-adapter-myim` 11 个）；执行结果属质量门，见 `docs/testing.md`。

交付物：`adapters/mock/`、`plugins/echo/`、`cmd/bot/main.go`、`internal/engine/engine_test.go`。

### 阶段 4：飞书 Adapter 或 OneBot Adapter

- [x] 已完成：选择飞书或 OneBot 之一实现 Adapter，并在包内 `init()` 中 `RegisterAdapter`。
  - 证据：两者**都已实现**（超出「之一」）：`adapters/feishu/register.go:19`、`adapters/onebot/register.go:19` 各自在 `init()` 中注册工厂（`adapters/feishu` 36 个测试、`adapters/onebot` 40 个测试）。
- [x] 已完成：飞书：实现事件订阅 HTTP 回调、challenge 校验、签名校验、快速 ACK、异步事件投递。
  - 证据：`adapters/feishu/feishu.go:323`（handleEvent：校验 → ACK → 异步投递）、`:353-360`（`url_verification` 用 body 内 token 校验后回写 challenge）、`:342`（签名失败返回 401）、`:375`（快速 ACK）、`:210`/`:269`（异步投递 worker 与 `wg` 回收）、`adapters/feishu/crypto.go`（AES 解密）。
- [x] 已完成：OneBot：实现 WebSocket 或 HTTP 接入，解析事件，调用发送 API。
  - 证据：`adapters/onebot/onebot.go:225`（New，按 `mode` 归一拓扑）、`:64`（parseMode）、`:385`（Start，按拓扑只监听/拨号所需的入口）、`adapters/onebot/ws.go`（自建 WebSocket，服务端与客户端双向：握手校验、鉴权、分片重组、心跳、掩码方向、`dialWebSocket`）、`adapters/onebot/reverse.go`（反向连接会话）、`adapters/onebot/forward.go`（正向连接与 1s→30s 退避重连）、`adapters/onebot/event.go`（事件解析）、`adapters/onebot/message.go`（消息互转）。
- [x] 已完成：实现消息段与平台消息互转，如实声明 Capabilities 与 Permissions。
  - 证据：`adapters/feishu/event.go:161`（convert）、`:227`（parseContent）、`adapters/feishu/feishu.go:160`（Capabilities）、`adapters/onebot/onebot.go:367`（Capabilities，能力值在 `:307` 构造）；权限声明见 `adapters/feishu/register.go:25`、`adapters/onebot/register.go:25`（均为 `network` + `net_listen`）。
- [x] 已完成：实现 Capabilities 降级（复用 `bot.Degrade`）。
  - 证据：`pkg/bot/degrade.go:13`（Degrade）；调用点 `adapters/feishu/send.go:111`、`adapters/onebot/onebot.go:644`、`internal/engine/send.go:46`。
- [x] 已完成：编写适配器单元测试。
  - 证据：`adapters/feishu/feishu_test.go`、`crypto.go` 相关测试、`register_test.go`（共 36 个）；`adapters/onebot/onebot_test.go`、`mode_test.go`、`ws.go` 相关、`reverse_test.go`、`forward_test.go`、`register_test.go`（共 40 个）；均基于 `httptest` 与测试内自建 WebSocket 客户端/服务端，无真实平台依赖。
- [x] 已完成：验收：可本地 Mock 平台请求，事件进入 Engine 并触发插件回复；新增平台未改动 `cmd/` 与 `internal/`。
  - 证据：`cmd/bot/thirdparty_adapter_test.go:72`（TestThirdPartyAdapterEndToEnd）在不修改核心代码的前提下只注册第三方适配器并跑通「事件 → echo 回复」；独立 module 形态另有 `examples/kei-adapter-myim/`。

交付物：`adapters/feishu/`（`feishu.go`、`event.go`、`send.go`、`crypto.go`、`register.go`）、`adapters/onebot/`（`onebot.go`、`event.go`、`message.go`、`ws.go`、`reverse.go`、`forward.go`、`register.go`）。

### 阶段 5：配置与生命周期

- [x] 已完成：实现 `internal/config`，加载 YAML，支持环境变量覆盖。
  - 证据：`internal/config/config.go:143`（Load）、`:159`（LoadBytes）、`internal/config/env.go:20`（applyEnv，前缀 `KEI_`，`env.go:13`），支持 `KEI_BOTS_<BOT>_<KEY>`、`KEI_PLUGINS_<PLUGIN>_<KEY>`、`KEI_ADAPTERS_<NAME>_<KEY>` 三类覆盖。测试 25 个。
- [x] 已完成：在 `cmd/bot/main.go` 中实现优雅启动和关闭。
  - 证据：`cmd/bot/main.go:68`（`signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`）、`:198`（HTTP 服务 `Shutdown`）；引擎侧排空由 `internal/engine/engine.go:367`（shutdown）实现，`internal/engine/engine_test.go:198`（TestGracefulShutdownDrainsQueuedEvents）覆盖。
- [x] 已完成：支持多 bot、多 adapter 配置；`bots[].adapter` 经注册表解析。
  - 证据：`internal/adaptermgr/adaptermgr.go:211`（Build 逐 bot 查表）、`:320`（查表失败的保护性报错）；启动期未知适配器报错见 `:134`；`internal/engine/engine_test.go:458`（TestMultiBotSamePlatformNeedsExplicitBotID）、`internal/adaptermgr/adaptermgr_test.go:131`（TestBuildTrimsDependencies 覆盖工厂多实例隔离）。
- [x] 已完成：支持插件启用/禁用。
  - 证据：`cmd/bot/main.go:208`（enabledPlugins 按 `plugins.<name>.enabled` 过滤，未列出的插件不启用）、`internal/engine/engine.go:418`（applyBotPluginFilter 按 `bots[].plugins` 白名单）、`internal/engine/engine_test.go:343`（TestPerBotPluginWhitelist）。
- [x] 已完成：支持 `adapters.<name>.enabled` 启用/禁用适配器（缺省启用，禁用则跳过其 bot）；支持 `adapters:` 段声明外部适配器；未知适配器名、重复注册名、缺 `Name`/`Platforms` 必须启动报错。
  - 证据（启用/禁用与外部声明）：`internal/config/config.go:358`（AdapterConfig）、`:380`（IsEnabled）、`:387`（AdapterEnabled）；`internal/adaptermgr/adaptermgr.go:117`（Validate 跳过被禁用适配器）、`:244`（禁用时告警并跳过 bot）；测试 `internal/adaptermgr/adaptermgr_test.go:278`（TestBuildSkipsDisabledAdapter）、`:321`（TestBuildSkipsDisabledInProcessAdapter）、`:355`（TestValidateSkipsDisabledUnknownAdapter）。
  - 证据（未知适配器名报错并列出已注册与已声明名单）：`internal/adaptermgr/adaptermgr.go:117-134`；测试 `internal/adaptermgr/adaptermgr_test.go:94`（TestValidateUnknownAdapter）。
  - 证据（重复注册名、缺 `Platforms`、适配器声明 `PermAll` 报错）：`internal/adaptermgr/adaptermgr.go:157-204`（ValidateRegistry / validateRegistryOf）；测试 `internal/adaptermgr/adaptermgr_test.go:51`（TestValidateRegistryRules）。
  - 证据（外部适配器配置校验：`grpc_addr` 存在时 `token` 必填、未声明 `grpc_addr` 时只允许 `enabled`、存在外部适配器时 `grpc.addr` 必须为固定端口）：`internal/config/config.go:612-654`。
- [x] 已完成：缺 `Name`/`factory` 的注册必须在启动校验阶段报错。`RegisterAdapter` 拒绝空注册名与 nil 工厂、不入表，但把该次注册记入 `bot.AdapterRegistrationErrors()`；`adaptermgr.ValidateRegistry` 在 `validateRegistryOf` 之外追加 `validateRegistrationRejects`，非空即以「适配器注册被拒绝」终止启动。这是对签名不返回错误的妥协：公开 SDK 的 `RegisterAdapter` 签名保持稳定，失败改为延迟到启动校验报错，不再静默失效。
  - 证据：`pkg/bot/adapter_registry.go:97`（AdapterRegistrationError）、`:122`（RegisterAdapter 的两条拒绝分支：`"注册名为空"`、`"工厂为 nil"`）、`:137`（recordAdapterReject）、`:147`（AdapterRegistrationErrors）；`internal/adaptermgr/adaptermgr.go:157`（ValidateRegistry）、`:165`（validateRegistrationRejects）；测试 `pkg/bot/adapter_registry_test.go:19`（TestRegisterAdapterRejectsInvalid，断言拒绝记录内容）、`internal/adaptermgr/adaptermgr_test.go:520`（TestValidateRegistrationRejects）。缺 `Platforms` 仍在 `internal/adaptermgr/adaptermgr.go:187` 报错。
- [x] 已完成：`bots[].enabled`（实例级开关）。语义与适配器同向：`BotConfig.Enabled *bool` 为 nil（未写）视为启用，只有显式 `enabled: false` 才跳过该实例。被停用的实例不装配、不建通道、不接收事件、不参与适配器校验（含保留键校验），日志记 info `bot 已禁用，跳过`（且不再对其私有键告警未知），也不进入 `Rule.BotIDs`（因此不参与 `bots[].plugins` 白名单收窄）。环境变量 `KEI_BOTS_<NAME>_ENABLED` 可覆盖（值无法解析为布尔时忽略、保留原值）。与 `adapters.<name>.enabled` 的区别：后者停用整个平台（该适配器下所有 bot 一并跳过并记 warn），前者只停用单个实例。
  - 证据：`internal/config/config.go:108`（BotConfig）、`:118`（`Enabled *bool`）、`:127`（IsEnabled）、`:275`（decodeBot 的 `case "enabled"`；`Settings` 注释已改为「除 name/adapter/enabled/plugins 外」）；`internal/config/env.go:123`（applyBotEnv 的 `case "enabled"`，经 `envBool` 解析）；`internal/adaptermgr/adaptermgr.go:126`（Validate 跳过条件 `!bc.IsEnabled() || !cfg.AdapterEnabled(...)`）、`:237`（Build 先判实例开关、`:239` 记 info）；`internal/engine/engine.go:418`（applyBotPluginFilter 两处 `if !b.IsEnabled() { continue }`）。
  - 测试：`internal/config/config_test.go:941`（TestBotEnabled）、`internal/adaptermgr/adaptermgr_test.go:457`（TestBuildSkipsDisabledBot）、`:494`（TestValidateDisabledBotSkipsReservedOption）、`internal/engine/engine_test.go:537`（TestDisabledBotExcludedFromPluginWhitelist）、`:560`（TestDisabledBotWhitelistNarrowing）、`cmd/bot/main_test.go:358`（TestExampleConfigLoads，断言飞书实例停用且 `enabled` 不落入 `Settings`）。
  - 示例配置已生效：`configs/config.yaml` 中 `feishu-main` 的 `enabled: false` 现在只停用该实例（不装配、不监听 18081），注释同时说明「要停用整个平台用 `adapters.feishu.enabled: false`」。
- [x] 已完成：验收：`go run ./cmd/bot -config configs/config.yaml` 可启动并优雅退出。
  - 证据：`configs/config.yaml` 可被加载并校验（`cmd/bot/main_test.go:358` TestExampleConfigLoads）；插件仅启用 `echo` 与 `manage`。

交付物：`internal/config/`（`config.go`、`env.go`）、`cmd/bot/main.go`、`cmd/bot/external.go`、`configs/config.yaml`。

### 阶段 6：外部插件 gRPC

- [x] 已完成：编写 `proto/plugin.proto`，生成 Go 代码。
  - 证据：`proto/plugin.proto`（`package plugin`，`go_package = github.com/RandomLemon/kei/proto/pluginpb`）；生成物 `proto/pluginpb/plugin.pb.go`、`proto/pluginpb/plugin_grpc.pb.go`（protoc-gen-go v1.36.11）；生成配置 `proto/buf.gen.yaml`、规范校验 `proto/buf.yaml`。
- [x] 已完成：实现核心 gRPC Server（`internal/grpcsrv`），提供 BotService。
  - 证据：`internal/grpcsrv/server.go`；`BotService` 提供 `SendMessage`、`GetConfig`、`Log`、`EmitEvent`（`proto/plugin.proto:231`）。测试 34 个（`internal/grpcsrv/server_test.go`）。
- [x] 已完成：实现外部插件加载器。
  - 证据：`internal/pluginmgr/external/external.go:86`（New）、`:113`（NewWithConn）、`internal/pluginmgr/external/plugin.go:42`（Plugin，单条兜底规则接收全部事件）；插件生命周期由 `internal/pluginmgr/manager.go:89/114/136/158` 管理。测试 20 个。
- [x] 已完成：提供 Python 或 Go 外部插件示例。
  - 证据：`cmd/example-plugin/main.go`、`cmd/example-plugin/plugin.go`（Go 实现 `PluginService` 并以客户端身份调用核心 `BotService`）；测试 21 个（含 bufconn 与 localhost TCP）。非 Go 语言示例未实现（第 20 章第 10 条将其列为可选）。
- [x] 已完成：支持 token/mTLS。
  - 证据：token 身份与权限 `internal/grpcsrv/server.go:62`（TokenInfo）、`:76`（Tokens）、`:140`（非 `plugin`/`adapter` 类别拒绝）；TLS `internal/grpcsrv/server.go:84`、`:170`；`cmd/bot/external.go:306`（serverTLSConfig，配置 `ca_file` 即启用 mTLS：`:321` `RequireAndVerifyClientCert`）、`:327`（clientTLSConfig）；配置项 `internal/config/config.go:78`（GrpcConfig，字段 `cert_file`/`key_file`/`ca_file` 见 `:82`/`:84`/`:86`）。测试 `cmd/bot/main_test.go:302`（TestTLSConfigs）、`internal/grpcsrv/server_test.go:1030`（TestTLSListenerAcceptsTrustedClient）、`cmd/example-plugin/integration_test.go:457`（TestEndToEndMutualTLS）。
- [x] 已完成：验收：外部插件可连接核心、接收事件、发送回复。
  - 证据：`cmd/example-plugin/integration_test.go:219`（TestEndToEndCommandRoundTrip）、`:246`（TestEndToEndRejectsBadToken）；核心侧 token 与 `send_message` 权限校验见 `internal/grpcsrv/server_test.go:479/494`。

交付物：`proto/plugin.proto`、`proto/pluginpb/`、`internal/grpcsrv/`、`internal/pluginmgr/`、`internal/pluginmgr/external/`、`cmd/example-plugin/`。

### 阶段 7：可观测性与限流

- [x] 已完成：实现 Prometheus 指标。
  - 证据：`internal/metrics/registry.go:175`（Handler，输出 `text/plain; version=0.0.4` 的 Prometheus 文本格式）、`internal/metrics/metrics.go:20`（Recorder 接口）、`:81`（counter）、`:90`（histogram）；`go.mod` 未引入 prometheus 客户端库，文本暴露为手写实现。测试 9 个（`internal/metrics/metrics_test.go`）。指标族与标签见 `../README.md`「可观测性」。
  - 适配器链路指标：`internal/metrics/metrics.go:31`（AdapterReconnected）、`:33`（AdapterReconnectFailed）、`:35`（AdapterDisabled）三个方法，对应 `:48`（`kei_adapter_reconnects_total`，标签 `adapter`+`result`）与 `:49`（`kei_adapter_disabled_total`，标签 `adapter`）两个指标族，渲染见 `internal/metrics/registry.go:246`、`:253`；上报点在外部适配器 supervisor（`internal/adaptermgr/external/client.go:334` 起），测试 `internal/adaptermgr/external/client_test.go:642`（TestSupervisorReportsAdapterMetrics）、`:750`（TestSupervisorWithoutRecorderDoesNotPanic）。
  - `internal/middleware/middleware.go:144`（Metrics 中间件）在 `:151` 上报 `RuleMatched`，`kei_rules_matched_total` 不再是死指标。
- [x] 已完成：实现日志规范，使用 `log/slog`。
  - 证据：根 module 44 个 Go 文件导入 `log/slog`（`examples/kei-adapter-myim` 另 2 个）；中间件 `internal/middleware/middleware.go:91`（Logger）字段化输出 `plugin`/`rule`/`event_id`/`duration_ms`/`error`；日志级别与格式由 `internal/config/config.go:64`（LogConfig）校验。
- [x] 已完成：实现令牌桶限流。
  - 证据：`internal/ratelimit/ratelimit.go:13`（Limiter）、`:32`（New）、`:54`（Allow），空闲桶回收；中间件入口 `internal/middleware/middleware.go:216`（RateLimit）；配置键 `limits.handler_rate`/`handler_burst`/`send_rate`/`send_burst`。测试 7 个。
- [x] 已完成：实现消息去重 TTL 缓存。
  - 证据：`internal/dedup/dedup.go:13`（Set）、`:26`（New，默认容量 4096，`ttl <= 0` 表示永不过期）、`:42`（Add）；事件总线与 Dedup 中间件共用。测试 7 个。
- [x] 已完成：实现管理命令，如 `/ping`、`/version`、`/plugins`、`/adapters`。
  - 证据：`plugins/manage/manage.go:45`（`/ping`）、`:50`（`/version`）、`:59`（`/plugins`，可经 `plugins_admin_only` 限定为管理员）、`:63`（`/adapters`）、`:67`（`/admin`，本阶段清单之外的额外命令）。测试 5 个。
- [x] 已完成：验收：指标可暴露，限流可测试，`/adapters` 能列出注册表与绑定关系。
  - 证据：指标 HTTP 端点 `cmd/bot/main.go:176`（serveMetrics，注册 `/metrics` 与 `/healthz`），端点开关为 `metrics.addr`；`/adapters` 输出 `plugins/manage/manage.go:63` → `describeAdapters(pc.Adapters, bot.RegisteredAdapters())`，即「已注册适配器注册表 + 各 bot 绑定（含进程内/外部标记）」。

交付物：`internal/metrics/`、`internal/ratelimit/`、`internal/dedup/`、`plugins/manage/`，以及 `internal/middleware/` 中的 Metrics/RateLimit/Auth 中间件。

### 阶段 8：第三方适配器支持（外部 gRPC 适配器）

- [x] 已完成：编写 `proto/adapter.proto`，生成 Go 代码；`BotService` 增加 `EmitEvent`。
  - 证据：`proto/adapter.proto`（与 `plugin.proto` 同属 `package plugin`、共用 `go_package`，`import "plugin.proto"` 复用 `Event`/`Message`/`Target`/`Empty`/`SendResponse`；`service AdapterService` 含 `Init`/`Start`/`Stop`/`Send`/`Shutdown`）；生成物 `proto/pluginpb/adapter.pb.go`、`proto/pluginpb/adapter_grpc.pb.go`；`EmitEvent` 定义见 `proto/plugin.proto`（`service BotService`，需 `receive_event` 权限）。
- [x] 已完成：实现 `internal/adaptermgr`：注册表查表、`AdapterContext` 装配、按 `Permissions` 裁剪 Storage/HTTPClient、保留键校验。
  - 证据：`internal/adaptermgr/adaptermgr.go:117`（Validate）、`:157`（ValidateRegistry）、`:211`（Build）、`:397`（Permissions）；保留键 `bot.OptListenAddr` 校验见 `internal/adaptermgr/external/client.go:436`（checkReservedOptions，要求 `net_listen`）；权限裁剪测试 `internal/adaptermgr/adaptermgr_test.go:131`（TestBuildTrimsDependencies）、`internal/adaptermgr/external/client_test.go:188`（TestNewRestrictsPermissionsToGranted）。
- [x] 已完成：实现核心侧外部适配器通道：dial `AdapterService`，调用 `Init`/`Start`/`Stop`/`Send`，并把 `EmitEvent` 接入事件总线（token + `receive_event` 校验）。
  - 证据：`internal/adaptermgr/external/client.go:112`（New dial + Init）、`:204`（Instance per bot）、`internal/adaptermgr/external/adapter.go:49/69/75`（Start/Stop/Send）；`EmitEvent` 的 token 与 `receive_event` 校验在核心侧 `internal/grpcsrv/server.go:470`，事件入口经 `internal/engine/engine.go:512`（Emit）进入总线；测试 `internal/grpcsrv/server_test.go:685`（TestEmitEventDeliversToHook）、`:714`（缺 `receive_event` 被拒）、`:727`（插件 token 被拒）。
- [x] 已完成：处理生命周期与容错：一进程多实例、优雅 `Shutdown`、断连退避重连、达上限只停用该 bot。
  - 证据：一进程多实例 `internal/adaptermgr/external/adapter.go:18`（instance 按 `bot_id` 隔离）；优雅 `Shutdown` `client.go:236`（Close → `AdapterService.Shutdown`）；退避重连 `client.go:40/42`（100ms 起、上限 5s）、`client.go:334`（supervise）；达上限只停用该 bot `client.go:365`（`setDisabled(true)`）、`client.go:421`、`adapter.go:79`（禁用后 Send 直接拒绝）；测试 `internal/adaptermgr/external/client_test.go:481`（TestSupervisorReconnectsAfterAdapterCrash）、`:410`（TestReconnectRestartsInstances）、`:445`（TestCloseStopsInstancesAndShutsDownAdapter）。
- [x] 已完成：提供 Go 外部适配器示例 `cmd/example-adapter`。
  - 证据：`cmd/example-adapter/main.go`（实现 `AdapterService`，HTTP 控制面 `POST /inject` 经 `BotService.EmitEvent` 上行、`GET /sent` 记录发送；`-listen`/`-platform`/`-control` 参数）；测试 `cmd/example-adapter/integration_test.go` 4 个。
- [x] 已完成：注册表版第三方适配器示例的「独立 module」形态：`examples/kei-adapter-myim/`。
  - 证据：`examples/kei-adapter-myim/go.mod`（module `github.com/example/kei-adapter-myim`，`require github.com/RandomLemon/kei v0.0.0` + `replace => ../..` 便于本地联调）、`register.go:36`（`init()` 中 `bot.RegisterAdapter`，Name `myim`、Platforms `[myim]`、权限 `network`+`net_listen`、Options `api_base`/`listen_addr`/`path`/`self_id`）、`myim.go`（实现 `bot.Adapter`：HTTP 回调先 ACK 再异步上行、发送走 `api_base`）、`README.md`。测试 11 个（`examples/kei-adapter-myim/myim_test.go`，如 `:165` TestRegistryLookup、`:223` TestFactoryRequiresHTTPClient、`:244` TestNewFromOptionsValidation）。
  - 「不改动根 module 任何文件」的证据：该 module 只 import `github.com/RandomLemon/kei/pkg/bot`（`register.go:25`、`myim.go:18`、`myim_test.go:17`），`grep -rn "kei/internal" examples/` 无结果；根 module 未新增构建配置（无 `go.work`），`git status` 中根 module 的改动全部来自阶段 5/7 的功能修复，与示例无关。真实第三方仓库把 `replace` 换成 `require` 的版本号即可（README 已说明）。
- [x] 已完成：编写测试（bufconn）：注册表装配、权限裁剪、保留键校验、`EmitEvent` 入队、`Send` 出站、token/权限不足被拒、适配器崩溃不影响核心。
  - 证据：`internal/adaptermgr/external/client_test.go:481`（TestSupervisorReconnectsAfterAdapterCrash 用 bufconn 模拟崩溃与重启）、`:306`（TestInstanceStartSendStop）、`:153`（TestNewInitAndMetadata）；`internal/adaptermgr/external_build_test.go:86`（TestBuildKeepsExternalChannelUsable）、`:161`（TestBuildFailureReleasesPartialAssembly）；`internal/adaptermgr/adaptermgr_test.go:107`（TestValidateReservedOptionRequiresNetListen）、`:400`（TestEmitFuncChecksOwnership）；`cmd/example-adapter/integration_test.go:231/337/360/388`（端到端、缺权限被拒、非法 Init、Start 幂等）。
- [x] 已完成：更新 `README.md`：适配器注册表用法、第三方适配器接入步骤（独立 module + 空导入 + 配置）、`adapters:` 段与外部适配器示例。
  - 证据：`../README.md`「写一个适配器」一节（注册表用法、权限裁剪表、`adapters.<name>.enabled` 停用示例、独立 module 布局）、「外部适配器（gRPC）」一节（`adapters:` 段配置示例、启动核心与适配器进程、注入事件并观察发送）。
- [x] 已完成：验收：示例外部适配器可被核心 dial、投递事件并被插件回复；`go test ./...`、`go test -race ./...`、`go vet ./...` 全绿。
  - 证据：`cmd/example-adapter/integration_test.go:231`（TestExternalAdapterEndToEnd，核心 dial 适配器 + `EmitEvent` 上行 + 回复断言）；三条命令的执行结果属质量门，见 `docs/testing.md`。

交付物：`proto/adapter.proto`、`proto/pluginpb/adapter*.go`、`internal/adaptermgr/`、`internal/adaptermgr/external/`、`cmd/example-adapter/`、`examples/kei-adapter-myim/`、`../README.md`。

---

## 20. 最终交付物

1. 完整 Go 项目源码。**存在**：根 module 的 `go list ./...` 解析出 26 个包，覆盖 `pkg/`、`internal/`、`adapters/`、`plugins/`、`cmd/`、`proto/pluginpb`；另有独立 module `examples/kei-adapter-myim/`。
2. `AGENTS.md`：全局硬性规则、代码约定、质量门、项目结构概览与文档索引（AGENTS.md 已不再承载设计与实现内容，详见 `docs/README.md`）。**存在**：仓库根 `AGENTS.md`。
3. `README.md` 使用说明。**存在**：仓库根 `README.md`（安装与快速开始、写插件、写适配器、外部适配器、外部插件、可观测性、测试与验证、已知限制）。
4. `configs/config.yaml` 示例配置。**存在**：`configs/config.yaml`（`mock-main`、`feishu-main`、`qq-main` 三个 bot 示例，`adapters:` 段与外部插件示例以注释给出）；加载校验测试 `cmd/bot/main_test.go:358`。`feishu-main` 通过实例级 `enabled: false` 停用，实测该 bot 不再装配、不监听 18081（见阶段 5 的 `bots[].enabled`）。
5. `plugins/echo` 示例插件。**存在**：`plugins/echo/echo.go`（`:44` 注册，`:12` Plugin）；测试 `plugins/echo/echo_test.go` 3 个。
6. `adapters/mock` Mock 适配器（经注册表接入）。**存在**：`adapters/mock/`（`register.go:15` 注册工厂、`mock.go` 实现、HTTP 控制面 `POST /inject` 与 `GET /sent`）；测试 13 个。
7. 至少一个真实平台适配器（飞书或 OneBot），经注册表接入。**存在且超额**：`adapters/feishu/`（`register.go:19`）与 `adapters/onebot/`（`register.go:19`）均已实现并注册。
8. 第三方适配器示例：只依赖 `pkg/bot` 的独立 module 注册示例 + `cmd/example-adapter` 外部 gRPC 适配器示例。**存在**：`examples/kei-adapter-myim/`（独立 module，只依赖 `pkg/bot`，11 个测试，不新增 `go.work`、不改根 module 任何文件）+ `cmd/example-adapter/`（外部 gRPC 适配器示例 + 4 个集成测试）。
9. 测试用例。**存在**：40 个测试文件、405 个顶层 `Test*`（`adapters/onebot` 40、`adapters/feishu` 36、`internal/grpcsrv` 34、`internal/router` 27、`internal/config` 25、`internal/engine` 24、`internal/eventbus` 23、`internal/middleware` 22、`cmd/example-plugin` 21、`internal/pluginmgr/external` 20、`internal/adaptermgr` 18、`pkg/bot` 16、`adapters/mock` 13、`cmd/bot` 12、`internal/adaptermgr/external` 10、`internal/metrics` 9、`internal/ratelimit` 7、`internal/dedup` 7、`internal/storage` 6、`internal/pluginmgr` 6、`plugins/manage` 5、`cmd/example-adapter` 4、`plugins/echo` 3、`pkg/message` 3、`internal/reply` 3；另 `examples/kei-adapter-myim` 11 个，属独立 module，不计入根 module 的 394 个）。
10. `proto/plugin.proto`、`proto/adapter.proto` 与外部插件/适配器示例（非 Go 语言实现可选）。**存在**：两份 proto 与生成代码 `proto/pluginpb/`；外部插件示例 `cmd/example-plugin/`（Go）、外部适配器示例 `cmd/example-adapter/`（Go）与 `examples/kei-adapter-myim/`（Go 独立 module）。非 Go 语言示例未提供，属该条明确可选项。

## 相关文档

- [docs/README.md](README.md)：文档索引与阅读顺序。
- [docs/architecture.md](architecture.md)：项目概述、总体架构与目录结构。
- [docs/testing.md](testing.md)：测试要求、构建与运行命令、完成定义。
- [../README.md](../README.md)：面向用户的安装、快速开始与接入指南。
