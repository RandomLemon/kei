# kei 实现阶段与交付物现状

本文记录第 13 章（实现阶段与任务清单）与第 19 章（最终交付物）在仓库中的落地现状：逐条标注每个阶段任务与每项交付物的实际状态，并给出可核对证据（包路径、文件与行、符号名）。

各模块的接口设计、语义与实现细节不在这里：见 [architecture.md](architecture.md)、[domain-model.md](domain-model.md)、[adapter.md](adapter.md)、[plugin.md](plugin.md)、[engine.md](engine.md)、[configuration.md](configuration.md)、[plugins/manage.md](plugins/manage.md)；测试统计口径与命令见 [testing.md](testing.md)；硬性规则见 [`../AGENTS.md`](../AGENTS.md)。

---

## 13. 实现阶段与任务清单

### 阶段 1：项目初始化与核心接口

- [x] 已完成：创建 `go.mod`，模块路径 `github.com/RandomLemon/kei`。
  - 证据：`go.mod:1`（`module github.com/RandomLemon/kei`，`go 1.25.0`；直接依赖 `gopkg.in/yaml.v3`、`gorm.io/gorm`、`gorm.io/driver/sqlite`、`gorm.io/driver/mysql`）。
- [x] 已完成：创建目录结构。
  - 证据：`pkg/`、`internal/`、`adapters/`、`plugins/`、`cmd/`、`configs/`；各目录职责见 [architecture.md](architecture.md) 第 4 章。
- [x] 已完成：在 `pkg/bot` 定义 Event、Message、Segment、User、Channel、Command。
  - 证据：`pkg/bot/event.go:27`、`:80`，`pkg/bot/message.go:19`、`:76`，`pkg/bot/user.go:4`、`:14`；字段与方法语义见 [domain-model.md](domain-model.md) 5.2–5.5。
- [x] 已完成：在 `pkg/bot` 定义 Adapter、Plugin、Registrar、Reply、BotAPI、Storage。
  - 证据：`pkg/bot/adapter.go:10`、`:28`，`pkg/bot/plugin.go:13`，`pkg/bot/registrar.go:98`，`pkg/bot/reply.go:9`，`pkg/bot/api.go:23`、`:36`；接口语义见 [adapter.md](adapter.md) 6.1 与 [plugin.md](plugin.md) 9.1–9.3、11.1–11.2。
- [x] 已完成：在 `pkg/bot` 定义 Adapter 注册表：`AdapterFactory`、`AdapterContext`、`AdapterMetadata`、`RegisterAdapter`、`RegisteredAdapters`、`LookupAdapter`（与插件注册表 API 对称）。
  - 证据：`pkg/bot/adapter_registry.go:19`、`:25`、`:41`（注册表类型）、`:120`（RegisterAdapter）、`:152`（RegisteredAdapters）、`:165`（LookupAdapter）；语义与签名见 [adapter.md](adapter.md) 6.2。
- [x] 已完成：编写接口单元测试或编译测试。
  - 证据：`pkg/bot/*_test.go`（5 个文件，17 个 `Test*`）、`pkg/message/message_test.go`（3 个）；统计口径见 [testing.md](testing.md) 第 15 章「实际测试分布」。
- [x] 已完成：验收：`go build ./...` 通过。
  - 证据：`go list ./...` 可解析出 `pkg/`、`internal/`、`adapters/`、`plugins/`、`cmd/` 下的全部包（`examples/` 下是独立 module，不计入）；命令执行属质量门，见 [testing.md](testing.md)。

交付物：`go.mod`、`go.sum`、`pkg/bot/`、`pkg/message/`、`configs/config.yaml`、`flake.nix`、`flake.lock`、`.envrc`、`.gitignore`、`LICENSE`。

### 阶段 2：核心引擎与事件总线

- [x] 已完成：实现 `internal/eventbus`，支持 Publish/Subscribe、worker pool、去重、保序。
  - 证据：`internal/eventbus/bus.go:118`、`:161`、`:207`、`:269`（New/Publish/Subscribe/Stats）；分片 worker、保序、去重与队列满语义见 [engine.md](engine.md) 8.1–8.6。测试 23 个（`internal/eventbus/bus_test.go`）。
- [x] 已完成：实现 `internal/router`，支持命令、正则、关键词、事件类型匹配。
  - 证据：`internal/router/registrar.go:35`、`:47`、`:61`、`:70`、`:78`（注册入口与兜底）；匹配与优先级语义见 [engine.md](engine.md) 10.3。测试 27 个（`internal/router/router_test.go`）。
- [x] 已完成：实现 `internal/middleware`，至少包含 Recover、Logger、Timeout、Dedup。
  - 证据：`internal/middleware/middleware.go:56`、`:91`、`:144`、`:164`、`:192`、`:216`、`:246`，`internal/middleware/chain.go:13`、`:32`；7 个中间件的签名与执行顺序见 [engine.md](engine.md) 10.4–10.5。测试 22 个。
- [x] 已完成：实现 `internal/engine`，串联 Adapter、EventBus、Router、Plugin；引擎只接收 `AdapterBinding`，不感知平台名。
  - 证据：`internal/engine/engine.go`（AdapterBinding、`Options.Adapters`、New、Run）；事件链路与装配见 [engine.md](engine.md) 7.1–7.4，分层约束见 [`../AGENTS.md`](../AGENTS.md) 第 2.1 节。测试 24 个。
- [x] 已完成：编写单元测试。
  - 证据：`internal/eventbus`（23）、`internal/router`（27）、`internal/middleware`（22）、`internal/engine`（24，含 `engine_test.go`、`pluginapi_test.go`、`adapter_catalog_test.go`）；逐包口径见 [testing.md](testing.md) 第 15 章「实际测试分布」。
- [x] 已完成：验收：`go test ./...` 通过。
  - 证据：上述四个包的测试文件齐备；执行结果属质量门，见 [testing.md](testing.md)。

交付物：`internal/eventbus/`、`internal/router/`、`internal/middleware/`、`internal/engine/`，以及被引擎复用的 `internal/dedup/`、`internal/ratelimit/`、`internal/storage/`、`internal/reply/`、`internal/metrics/`。

### 阶段 3：Mock Adapter 与 Echo 插件

- [x] 已完成：实现 `adapters/mock`，支持手动注入事件和捕获发送消息，并在 `init()` 中注册工厂。
  - 证据：`adapters/mock/mock.go:192`、`:211`、`:329`、`:330`，`adapters/mock/register.go:15`；控制面与行为见 [adapter.md](adapter.md) 6.7 › mock。测试 13 个。
- [x] 已完成：实现 `plugins/echo`。
  - 证据：`plugins/echo/echo.go:12`、`:18`、`:44`；实现全文见 [plugin.md](plugin.md) 9.4。测试 3 个。
- [x] 已完成：通过空导入 + `internal/adaptermgr` 装配 Mock Adapter 并启动 Engine，不得出现平台名分支。装配实现位于公开包 `pkg/kei`，`cmd/bot/main.go` 只保留空导入与 `kei.Run` 调用。
  - 证据：`cmd/bot/main.go`（空导入）；装配路径见 [architecture.md](architecture.md) 3.1 与 [engine.md](engine.md) 7.2。
- [x] 已完成：编写集成测试：注入 `/echo hello`，断言回复 `hello`。
  - 证据：`internal/engine/engine_test.go:172`（TestEchoIntegrationRoundTrip，断言发送内容为 `hello`、目标为 `mock-main`/`mock`/`mock-group`）。
- [x] 已完成：验收：`go test -race ./...` 通过。
  - 证据：根 module 与独立 module 的测试文件齐备（统计口径见 [testing.md](testing.md)）；执行结果属质量门。

交付物：`adapters/mock/`、`plugins/echo/`、`pkg/kei/`（当前装配门面）、`cmd/bot/main.go`、`internal/engine/engine_test.go`。

### 阶段 4：飞书 Adapter 或 OneBot Adapter

- [x] 已完成：选择飞书或 OneBot 之一实现 Adapter，并在包内 `init()` 中 `RegisterAdapter`。
  - 证据：两者**都已实现**（超出「之一」）：`adapters/feishu/register.go:19`、`adapters/onebot/register.go:19` 各自在 `init()` 中注册工厂；现状见 [adapter.md](adapter.md) 6.7，测试数（36 / 40）见 [testing.md](testing.md) 第 15 章「实际测试分布」。
- [x] 已完成：飞书：实现事件订阅 HTTP 回调、challenge 校验、签名校验、快速 ACK、异步事件投递。
  - 证据：`adapters/feishu/feishu.go:210`、`:269`、`:323`、`:342`、`:353-360`、`:375`，`adapters/feishu/crypto.go`；协议细节见 [adapter.md](adapter.md) 6.7 › feishu。
- [x] 已完成：OneBot：实现 WebSocket 或 HTTP 接入，解析事件，调用发送 API。
  - 证据：`adapters/onebot/onebot.go:64`、`:225`、`:385`，`adapters/onebot/ws.go`、`reverse.go`、`forward.go`、`event.go`、`message.go`；四种 `mode` 的拓扑与忽略规则见 [adapter.md](adapter.md) 6.7 › onebot。
- [x] 已完成：实现消息段与平台消息互转，如实声明 Capabilities 与 Permissions。
  - 证据：`adapters/feishu/event.go:161`、`:227`，`adapters/feishu/feishu.go:160`，`adapters/onebot/onebot.go:307`、`:367`，`adapters/feishu/register.go:25`、`adapters/onebot/register.go:25`；能力位与权限清单见 [adapter.md](adapter.md) 6.3、6.7。
- [x] 已完成：实现 Capabilities 降级（复用 `bot.Degrade`）。
  - 证据：`pkg/bot/degrade.go:13`；调用点 `adapters/feishu/send.go:111`、`adapters/onebot/onebot.go:644`、`internal/engine/send.go:46`；降级规则表见 [adapter.md](adapter.md) 6.4。
- [x] 已完成：编写适配器单元测试。
  - 证据：`adapters/feishu`（36，`feishu_test.go`/`register_test.go` 及 `crypto.go` 相关测试）、`adapters/onebot`（40，`onebot_test.go`/`mode_test.go`/`reverse_test.go`/`forward_test.go`/`register_test.go`）；测试手法与覆盖清单见 [testing.md](testing.md) 第 15 章第 5 条与实际测试分布表。
- [x] 已完成：验收：可本地 Mock 平台请求，事件进入 Engine 并触发插件回复；新增平台未改动 `cmd/` 与 `internal/`。
  - 证据：`cmd/bot/thirdparty_adapter_test.go:79`（TestThirdPartyAdapterEndToEnd）在不修改核心代码的前提下只注册第三方适配器并跑通「事件 → echo 回复」；独立 module 形态另有 `examples/kei-adapter-myim/`。

交付物：`adapters/feishu/`（`feishu.go`、`event.go`、`send.go`、`crypto.go`、`register.go`）、`adapters/onebot/`（`onebot.go`、`event.go`、`message.go`、`ws.go`、`reverse.go`、`forward.go`、`register.go`）。

### 阶段 5：配置与生命周期

- [x] 已完成：实现 `internal/config`，加载 YAML，支持环境变量覆盖。
  - 证据：`internal/config/config.go:125`（Load）、`:141`（LoadBytes），`internal/config/env.go:19`（applyEnv，前缀常量在 `:12`）；加载顺序、覆盖规则与环境变量全表见 [configuration.md](configuration.md) 12.1、12.6。测试 28 个。
- [x] 已完成：实现优雅启动和关闭（装配实现位于 `pkg/kei`，`cmd/bot/main.go` 只做信号处理与 `kei.Run` 调用）。
  - 证据：`cmd/bot/main.go`（`signal.NotifyContext`）、`pkg/kei/assemble.go`、`internal/engine/engine.go`（shutdown）；关闭顺序见 [engine.md](engine.md) 7.3、8.6，`TestGracefulShutdownDrainsQueuedEvents` 覆盖排空。
- [x] 已完成：支持多 bot、多 adapter 配置；`bots[].adapter` 经注册表解析。
  - 证据：`internal/adaptermgr/adaptermgr.go:73`（Validate 的未知适配器报错）、`:184`、`:188`（Build 逐 bot 查表与保护性报错）；装配与 `Target.BotID` 解析语义见 [configuration.md](configuration.md) 12.2、12.8 第 7 条与 [adapter.md](adapter.md) 6.2；测试 `internal/engine/engine_test.go:458`（TestMultiBotSamePlatformNeedsExplicitBotID）、`internal/adaptermgr/adaptermgr_test.go:130`（TestBuildTrimsDependencies）。
- [x] 已完成：支持插件启用/禁用。
  - 证据：`pkg/kei/assemble.go`（`selectPlugins` 按 `plugins.<name>.enabled` 过滤）、`internal/engine/engine.go`（`applyBotPluginFilter` 按 `bots[].plugins` 白名单）；语义见 [configuration.md](configuration.md) 12.2–12.3 与 [engine.md](engine.md) 7.6；测试 `internal/engine/engine_test.go`（TestPerBotPluginWhitelist）。
- [x] 已完成：支持 `adapters.<name>.enabled` 启用/禁用适配器（缺省启用，禁用则跳过其 bot，该段只支持 `enabled` 键）；未知适配器名、重复注册名、缺 `Name`/`Platforms` 必须启动报错。
  - 证据（启用/禁用）：`internal/config/config.go:338`（AdapterConfig）、`:347`（IsEnabled）、`:354`（AdapterEnabled）；`internal/adaptermgr/adaptermgr.go:68`（Validate 跳过被禁用实例/适配器）、`:154`（info `bot 已禁用，跳过`）、`:159`（warn `适配器已禁用，跳过 bot`）；测试 `internal/adaptermgr/adaptermgr_test.go:243`（TestBuildSkipsDisabledAdapter）、`:278`（TestBuildSkipsDisabledInProcessAdapter）、`:310`（TestValidateSkipsDisabledUnknownAdapter）。
  - 证据（未知适配器名报错并列出已注册名单）：`internal/adaptermgr/adaptermgr.go:73`；测试 `internal/adaptermgr/adaptermgr_test.go:93`（TestValidateUnknownAdapter）。
  - 证据（重复注册名、缺 `Platforms`、适配器声明 `PermAll` 报错）：`internal/adaptermgr/adaptermgr.go:87`（ValidateRegistry）、`:112`（validateRegistryOf）；测试 `internal/adaptermgr/adaptermgr_test.go:50`（TestValidateRegistryRules）。
  - 语义与报错文案见 [configuration.md](configuration.md) 12.4、12.8 第 5–8 条与 [adapter.md](adapter.md) 6.3。
- [x] 已完成：缺 `Name`/`factory` 的注册必须在启动校验阶段报错。
  - 证据：`pkg/bot/adapter_registry.go:95`（AdapterRegistrationError）、`:120`（RegisterAdapter 的两条拒绝分支）、`:135`（recordAdapterReject）、`:145`（AdapterRegistrationErrors）；`internal/adaptermgr/adaptermgr.go:87`（ValidateRegistry）、`:95`（validateRegistrationRejects，非空即终止启动）；测试 `pkg/bot/adapter_registry_test.go:19`（TestRegisterAdapterRejectsInvalid）、`internal/adaptermgr/adaptermgr_test.go:415`（TestValidateRegistrationRejects）。机制与妥协原因见 [adapter.md](adapter.md) 6.2 与 [engine.md](engine.md) 附录第 1 条。
- [x] 已完成：`bots[].enabled`（实例级开关，与 `adapters.<name>.enabled` 同向、作用范围只到单个实例）。
  - 证据：`internal/config/config.go:90`（BotConfig）、`:100`（`Enabled *bool`）、`:109`（IsEnabled）、`:255`（decodeBot 的 `case "enabled"`），`internal/config/env.go:110`（applyBotEnv 的 `case "enabled"`），`internal/adaptermgr/adaptermgr.go:68`（Validate 跳过条件）、`:152`、`:154`（Build 先判实例开关并记 info），`internal/engine/engine.go:402`、`:416`（applyBotPluginFilter 的 `if !b.IsEnabled()`）。
  - 测试：`internal/config/config_test.go:789`（TestBotEnabled）、`internal/adaptermgr/adaptermgr_test.go:353`（TestBuildSkipsDisabledBot）、`:389`（TestValidateDisabledBotSkipsReservedOption）、`internal/engine/engine_test.go`（TestDisabledBotExcludedFromPluginWhitelist、TestDisabledBotWhitelistNarrowing）、`cmd/bot/main_test.go`（TestExampleConfigLoads）。
  - 语义、环境变量覆盖与两层开关的区别见 [configuration.md](configuration.md) 12.2、12.8 第 6 条与 [adapter.md](adapter.md) 6.7；示例配置 `configs/config.yaml` 的 `feishu-main: enabled: false` 即实测样本。
- [x] 已完成：可插拔存储后端 —— `storage.type` 选 `memory`（默认）/`sqlite`/`mysql`，新增类型只需一个包 + `init()` 注册，核心不改配置结构。
  - 证据：`internal/storage/registry.go`（`Register`/`Open`/`Kinds`）、`internal/storage/memory.go`（`init()` 注册）、`internal/storage/sqlstore/`（GORM 通用实现）、`internal/storage/sqlite/`、`internal/storage/mysql/`、`internal/config/config.go`（`StorageConfig`/`decodeStorage`/`StorageConfig.validate`）、`internal/config/env.go`（`KEI_STORAGE_*`）、`pkg/kei/assemble.go`（`buildStorage` 注入优先、否则按配置构造）。
  - 语义、参数与未知类型报错文案见 [configuration.md](configuration.md) 12.5；SQL 实现与内存实现的一致性见 [plugin.md](plugin.md) 11.2；测试 `internal/storage/registry_test.go`、`internal/storage/sqlite/sqlite_test.go`（`internal/storage/mysql/mysql_test.go` 默认跳过）。
- [x] 已完成：验收：`go run ./cmd/bot -config configs/config.yaml` 可启动并优雅退出。
  - 证据：`configs/config.yaml` 可被加载并校验（`cmd/bot/main_test.go` TestExampleConfigLoads）；插件仅启用 `echo` 与 `manage`。

交付物：`internal/config/`（`config.go`、`env.go`）、`internal/storage/`（`registry.go` 与 `sqlstore`/`sqlite`/`mysql`）、`pkg/kei/`（`kei.go`、`assemble.go`）、`cmd/bot/main.go`、`configs/config.yaml`。

### 阶段 6：可观测性与限流

- [x] 已完成：实现 Prometheus 指标。
  - 证据：`internal/metrics/registry.go:143`（Handler）、`internal/metrics/metrics.go:20`（Recorder）、`:71`（counter）、`:80`（histogram）；`go.mod` 未引入 prometheus 客户端库（文本格式为手写实现），`internal/middleware/middleware.go:144`、`:151` 上报 `RuleMatched`。指标族、标签与 `RuleMatched`/`EventHandled` 语义见 [engine.md](engine.md) 10.5 与 [`../README.md`](../README.md)「可观测性」。测试 9 个。
- [x] 已完成：实现日志规范，使用 `log/slog`。
  - 证据：`internal/middleware/middleware.go:91`（Logger 字段化输出）、`internal/config/config.go:58`（LogConfig 校验级别与格式）；字段集与级别映射见 [engine.md](engine.md) 10.5，配置键见 [configuration.md](configuration.md) 12.1。
- [x] 已完成：实现令牌桶限流。
  - 证据：`internal/ratelimit/ratelimit.go:13`、`:32`、`:54`，`internal/middleware/middleware.go:216`（RateLimit 中间件）；令牌桶语义与接线见 [engine.md](engine.md) 7.7、10.5，配置键见 [configuration.md](configuration.md) 12.1。测试 7 个。
- [x] 已完成：实现消息去重 TTL 缓存。
  - 证据：`internal/dedup/dedup.go:13`、`:26`、`:42`；总线层与中间件层两层去重的关系见 [engine.md](engine.md) 8.5、10.6。测试 7 个。
- [x] 已完成：实现管理命令 `/manage ping`、`/manage version`、`/manage plugins`、`/manage adapters`，另有阶段清单之外的 `/manage admin`。
  - 证据：`plugins/manage/manage.go:49`、`:54`、`:63`、`:67`、`:71`。测试 6 个。命令契约、规则 ID/优先级/管理员门槛、配置键与实测输出见 [plugins/manage.md](plugins/manage.md) 第 2–4 节。
- [x] 已完成：验收：指标可暴露，限流可测试，`/manage adapters` 能列出注册表与绑定关系。
  - 证据：`pkg/kei/assemble.go`（serveMetrics 注册 `/metrics` 与 `/healthz`，开关为 `metrics.addr`）、`plugins/manage/manage.go:133`（`describeAdapters`）；两段来源、不一致原因与样例输出见 [engine.md](engine.md) 7.9 与 [plugins/manage.md](plugins/manage.md) 3.4；端点开关语义见 [configuration.md](configuration.md) 12.1。

交付物：`internal/metrics/`、`internal/ratelimit/`、`internal/dedup/`、`plugins/manage/`，以及 `internal/middleware/` 中的 Metrics/RateLimit/Auth 中间件。

### 阶段 7：第三方适配器支持（进程内注册与独立 module）

- [x] 已完成：实现 `internal/adaptermgr`：注册表查表、`AdapterContext` 装配、按 `Permissions` 裁剪 Storage/HTTPClient、保留键校验。
  - 证据：`internal/adaptermgr/adaptermgr.go`（`Validate`/`ValidateRegistry`/`Build`/`Permissions`/`checkReservedOptions`）；四项职责的语义与报错文案见 [adapter.md](adapter.md) 6.2–6.3、6.5；测试 `internal/adaptermgr/adaptermgr_test.go`（TestBuildTrimsDependencies）。
- [x] 已完成：注册表版第三方适配器示例的「独立 module」形态：`examples/kei-adapter-myim/`。
  - 证据：`examples/kei-adapter-myim/go.mod`、`register.go:36`（`init()` 中 `bot.RegisterAdapter`）、`myim.go`、`README.md`；示例源码与接入约定见 [adapter.md](adapter.md) 6.5，测试清单（11 个）见 [testing.md](testing.md) 第 15 章第 6 条。
  - 「不改动根 module 任何文件」的证据：该 module 只 import `github.com/RandomLemon/kei/pkg/bot`（`register.go:25`、`myim.go:18`、`myim_test.go:17`），`grep -rn "kei/internal" examples/` 无结果；根 module 未新增构建配置（无 `go.work`）。真实第三方仓库把 `replace` 换成版本号即可（README 已说明）。
- [x] 已完成：更新 `README.md`：适配器注册表用法、第三方适配器接入步骤（独立 module + 空导入 + 配置）、`adapters.<name>.enabled` 停用示例。
  - 证据：`../README.md`「写一个适配器」一节。
- [x] 已完成：验收：第三方适配器在不改动核心代码的前提下经进程内注册表接入并被插件回复；`go test ./...`、`go test -race ./...`、`go vet ./...` 全绿。
  - 证据：`cmd/bot/thirdparty_adapter_test.go`（TestThirdPartyAdapterEndToEnd）与 `examples/kei-adapter-myim/myim_test.go`；三条命令的执行结果属质量门，见 [testing.md](testing.md)。

交付物：`internal/adaptermgr/`、`examples/kei-adapter-myim/`、`../README.md`。

---

## 19. 最终交付物

1. 完整 Go 项目源码。**存在**：根 module 的 `go list ./...` 解析出 `pkg/`、`internal/`、`adapters/`、`plugins/`、`cmd/` 下的全部包（逐目录职责见 [architecture.md](architecture.md) 第 4 章）；另有独立 module `examples/kei-adapter-myim/`。
2. `AGENTS.md`：全局硬性规则、代码约定与质量门，并指向 `docs/README.md` 作为唯一文档索引（设计与实现内容不在其中）。**存在**：仓库根 `AGENTS.md`。
3. `README.md` 使用说明。**存在**：仓库根 `README.md`（安装与快速开始、写插件、写适配器、可观测性、测试与验证、已知限制）。
4. `configs/config.yaml` 示例配置。**存在**：`configs/config.yaml`（`mock-main`、`feishu-main`、`qq-main` 三个 bot；`adapters:` 段示例以注释给出；`feishu-main` 经实例级 `enabled: false` 停用）。加载校验测试 `cmd/bot/main_test.go`（TestExampleConfigLoads）与 `pkg/kei/assemble_test.go`；bot 与键的语义见 [configuration.md](configuration.md) 12.2、12.7。
5. `plugins/echo` 示例插件。**存在**：`plugins/echo/echo.go`（`:44` 注册，`:12` Plugin）；实现全文见 [plugin.md](plugin.md) 9.4，测试 3 个。
6. `adapters/mock` Mock 适配器（经注册表接入）。**存在**：`adapters/mock/`（`register.go:15` 注册工厂、`mock.go` 实现）；行为见 [adapter.md](adapter.md) 6.7 › mock，测试 13 个。
7. 至少一个真实平台适配器（飞书或 OneBot），经注册表接入。**存在且超额**：`adapters/feishu/`（`register.go:19`）与 `adapters/onebot/`（`register.go:19`）均已实现并注册；现状见 [adapter.md](adapter.md) 6.7。
8. 第三方适配器示例：只依赖 `pkg/bot` 的独立 module 注册示例。**存在**：`examples/kei-adapter-myim/`（独立 module，11 个测试，不新增 `go.work`、不改根 module 任何文件）。
9. 测试用例。**存在**：根 module 各包均有 `*_test.go`，逐包统计与覆盖见 [testing.md](testing.md) 第 15 章「实际测试分布」；另有独立 module `examples/kei-adapter-myim`（11 个测试，不计入根 module 统计）。

## 相关文档

- [docs/README.md](README.md)：文档索引与阅读顺序。
- [docs/architecture.md](architecture.md)：项目概述、总体架构与目录结构。
- [docs/testing.md](testing.md)：测试要求、构建与运行命令、完成定义。
- [../README.md](../README.md)：面向用户的安装、快速开始与接入指南。
