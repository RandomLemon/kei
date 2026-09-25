# AGENTS.md

`kei` 仓库的全局硬性规则与结构概览。**具体设计、接口契约、实现方式不在这里**：按领域拆到 [`docs/`](docs/README.md)，见第 6 节文档索引。改动某领域前，先读该领域文档与既有实现。

## 1. 项目定位

- 项目名 `kei`，模块路径 `github.com/RandomLemon/kei`。
- 目标：Go 编写的 chatbot 框架。平台协议与业务逻辑解耦；插件开发轻量；适配器与插件**同构**——同一套注册表、元信息、权限声明、配置驱动装配，既可进程内加载，也可扩展为独立 gRPC 进程（`proto/` 是唯一契约）。
- 非目标：个人微信逆向协议、完整管理后台 UI、NLP 模型、一次性覆盖所有 IM 平台。
- 现状：内置适配器 `mock`/`onebot`/`feishu`（`wecom` 未实现），插件 `echo`/`manage`，外部进程示例 `cmd/example-plugin`、`cmd/example-adapter`。逐项状态与已知缺口见 [`docs/roadmap.md`](docs/roadmap.md)。

## 2. 硬性规则

### 2.1 分层与依赖

- 公开 SDK 只放 `pkg/bot`（唯一的公开辅助包是 `pkg/message` 消息段构建器）；`pkg/bot` 禁止 import `internal/`。
- 核心逻辑只放 `internal/`；平台专属代码只允许出现在 `adapters/<platform>/` 或第三方适配器包/module 中；插件代码只放 `plugins/` 或第三方包/module，且只依赖 `pkg/bot`。
- `cmd/` 与 `internal/` 中禁止出现平台名分支（`switch adapter`、`if platform == "feishu"` 等）。适配器只能经 `bot.LookupAdapter` + `internal/adaptermgr` 装配，插件只能经注册表 + `internal/pluginmgr` 装配。
- 适配器与插件同等对待：任何「为某平台写死」的改动都说明注册表被绕过，先修设计再加平台。新增平台的自检问题：只加一个新包（或新 module）+ 一段配置能否跑通？完整示例见 `examples/kei-adapter-myim/`（独立 module，只依赖 `pkg/bot`）。
- 第三方适配器/插件不得要求核心仓库改动任何文件；核心仓库不为第三方改代码。新增/改动注册表 API 时必须同步维护 `examples/kei-adapter-myim/`。
- 配置与密钥只能经配置对象读取（插件 `PluginContext.Config`、适配器 `AdapterContext.Config`）；适配器/插件不得直接读环境变量或文件。

### 2.2 技术栈与依赖

- Go 1.25+（`go.mod` 为 `go 1.25.0`）。工具链、`gopls`、`golangci-lint`、`dlv`、`protoc`/`buf` 由 `flake.nix` + direnv 提供，`GOTOOLCHAIN=local`。
- 优先标准库：`context`、`log/slog`、`net/http`、`encoding/json`、`sync`、`time`。
- 允许的第三方依赖仅 `gopkg.in/yaml.v3`（配置）、`google.golang.org/grpc`、`google.golang.org/protobuf`。新增依赖必须先说明理由，并同步 [`docs/architecture.md`](docs/architecture.md) 与 [`README.md`](README.md)。
- 禁止用 Go `plugin` 作为插件机制；动态扩展走独立进程 + gRPC。
- `proto/` 由 `buf` 管理（`proto/buf.yaml`、`proto/buf.gen.yaml`，生成物 `proto/pluginpb`）。改 proto 必须同步生成代码与 [`docs/grpc.md`](docs/grpc.md)。

### 2.3 运行时契约

- 所有阻塞操作必须接收 `context.Context`，不得存在无超时等待。
- 插件 Handler 必须支持超时与 panic recover；所有 goroutine 必须有退出机制，不得泄漏。
- 平台回调必须快速 ACK（飞书 3 秒内），业务异步投递；`EventSink.Emit` / `BotService.EmitEvent` 的返回值只表示「已入队」。
- 所有事件必须支持去重；`Event.ID` 必须稳定且平台内唯一。
- 所有共享状态必须加锁或用 `sync.Map`；注册表与装配必须并发安全。
- 禁止包级可变状态承载实例数据：适配器/插件状态挂在实例上，同平台多实例必须互相隔离。
- 错误必须返回处理（仅启动阶段不可恢复错误可中止），不得用 panic 表达运行期失败。
- 同一会话必须保序（键 `platform:botID:channelID:userID`）。

### 2.4 质量门（合并前必须全绿）

```bash
gofmt -l .                    # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...
cd proto && buf lint          # 改动 proto 时
```

- 公开接口必须有文档注释；注释与实现不符视为缺陷。
- 不破坏 `pkg/bot` 的向后兼容性。
- 行为变更（公共 API、配置键、proto）必须同步更新对应 `docs/` 文档与 `README.md`；修掉缺口后同步删除文档中「附：已知缺口与实现边界」的对应条目。

## 3. 代码约定

1. 包名小写、简短，不使用下划线。
2. 公开接口必须有注释。
3. 所有错误必须返回，不得 panic，除非是启动阶段不可恢复错误。
4. 所有 goroutine 必须有退出机制，避免泄漏。
5. 所有 map 并发访问必须加锁或使用 `sync.Map`。
6. 所有时间使用 `time.Time`，时区统一 UTC。
7. 所有 ID 使用字符串，不假设是数字。
8. 日志使用 `log/slog`，字段化输出。
9. 测试使用标准库 `testing`，必要时使用 `testify`。
10. 禁止在 `pkg/bot` 中引入 `internal/` 包。
11. 适配器与插件只依赖 `pkg/bot`，不得依赖 `internal/`；平台专属代码只允许出现在 `adapters/` 或第三方适配器包内。
12. 适配器/插件的实例状态挂在实例上，禁止包级可变状态；注册表与装配必须并发安全。

## 4. 测试约定

- 每个核心包必须有单元测试；核心链路必须有集成测试（Mock Adapter → EventBus → Router → 插件 → Reply）。
- 必须覆盖：命令/正则/关键词/事件触发、规则优先级、中间件顺序、panic recover、超时、去重、同会话保序、优雅关闭排空、发送限流与重试、能力降级、适配器注册表校验（未知名并列出已注册名 / 重复注册名 / 缺 `Platforms`）、多实例隔离、权限裁剪、适配器启用开关。
- gRPC 相关测试用 `bufconn`。
- 测试断言可观察行为，不断言实现细节（wiring、字段拷贝、日志文案）。
- 完整矩阵、命令与完成定义见 [`docs/testing.md`](docs/testing.md)。

## 5. 项目结构

```text
cmd/bot/                 入口：加载配置、装配适配器与插件、优雅退出（无平台分支）
cmd/example-plugin/      Go 外部插件示例（独立进程）
cmd/example-adapter/     Go 外部适配器示例（独立进程）
examples/kei-adapter-myim/  独立 module 的第三方适配器示例（只依赖 pkg/bot，等价独立仓库形态）
pkg/bot/                 公开 SDK：Event/Message/Segment、Adapter 注册表、Plugin、Registrar、Reply、BotAPI、Storage
pkg/message/             消息段构建器
internal/engine/         核心引擎：BotAPI 实现、发送限流与重试、优雅关闭
internal/eventbus/       事件总线：分片保序、去重、drain
internal/router/         路由匹配与 Registrar 实现
internal/middleware/     中间件：Recover/Logger/Metrics/Timeout/Auth/RateLimit/Dedup
internal/config/         YAML 配置加载与环境变量覆盖
internal/adaptermgr/     适配器装配：注册表查表、权限裁剪、外部适配器通道（external/）
internal/pluginmgr/      插件生命周期管理、外部插件加载（external/）
internal/grpcsrv/        BotService gRPC 服务端（插件与外部适配器共用）
internal/storage/        bot.Storage 内存实现
internal/dedup/          带 TTL 与容量的去重集合
internal/ratelimit/      按 key 的令牌桶
internal/metrics/        Prometheus 文本指标（标准库实现）
internal/reply/          Reply 构建器实现
adapters/mock/           本地测试适配器（HTTP 控制面注入事件、观察发送）
adapters/onebot/         OneBot v11 适配器
adapters/feishu/         飞书开放平台适配器
plugins/echo/            示例插件：/echo
plugins/manage/          管理命令：/ping、/version、/plugins、/adapters
proto/                   plugin.proto、adapter.proto 与生成代码 pluginpb/
configs/config.yaml      示例配置
docs/                    设计文档集，入口 docs/README.md
```

内置适配器放 `adapters/<platform>/`；第三方适配器/插件放独立包或独立 module（只依赖 `pkg/bot`），主程序空导入 + 配置即接入。目录职责与架构分层见 [`docs/architecture.md`](docs/architecture.md)。

## 6. 文档索引

| 文档 | 内容 |
| --- | --- |
| [`docs/README.md`](docs/README.md) | 文档索引、章节↔文件对应、阅读顺序、文档约定 |
| [`docs/architecture.md`](docs/architecture.md) | 第 1、3、4 章：项目概述、总体架构、目录结构与模块职责 |
| [`docs/domain-model.md`](docs/domain-model.md) | 第 5 章：`pkg/bot` 领域模型与 `Segment.Data` 键约定 |
| [`docs/adapter.md`](docs/adapter.md) | 第 6 章：适配器接口、注册表与工厂、权限、能力与降级、第三方接入、内置适配器现状 |
| [`docs/plugin.md`](docs/plugin.md) | 第 9、11 章：插件接口、Registrar/Option、Reply、BotAPI、PluginContext、Storage |
| [`docs/engine.md`](docs/engine.md) | 第 7、8、10 章：Engine、EventBus、Router、中间件与限流 |
| [`docs/grpc.md`](docs/grpc.md) | 第 12 章：外部插件与外部适配器 gRPC 协议、代码生成、崩溃隔离与重连 |
| [`docs/configuration.md`](docs/configuration.md) | 第 13 章：配置结构、键语义、环境变量覆盖、校验与告警 |
| [`docs/testing.md`](docs/testing.md) | 第 16、17、19 章：测试要求、构建与运行命令、完成定义 |
| [`docs/roadmap.md`](docs/roadmap.md) | 第 14、20 章：实现阶段回顾与交付物现状（含未实现项） |

独立 module 示例（不属于根 module，自带 `go.mod`）：`examples/kei-adapter-myim/` —— 只依赖 `pkg/bot` 的第三方适配器，注册与接入方式即第三方真实形态。改动注册表 API 时同步维护它。

## 7. 工作流

- 改代码前先读对应领域文档与既有实现，沿用仓库既有模式；禁止并存第二套约定。
- 修改导出符号前先查全部调用点（`lsp references`），迁移所有调用方并删除被替代的旧路径，不留兼容垫片。
- 设计歧义时以 `docs/` 为准；文档未覆盖时选最简单、可测试的方案。
- 优先保证 `pkg/bot` 接口稳定：接口稳定比功能多更重要，不过度设计。
