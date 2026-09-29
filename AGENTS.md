# AGENTS.md

`kei` 仓库的全局硬性规则（第 2、14、17 章）。**具体设计、接口契约、实现方式不在这里**：全部在 [`docs/`](docs/README.md)，索引与阅读顺序见 [`docs/README.md`](docs/README.md)。改动某领域前，先读该领域文档与既有实现，沿用仓库既有模式，禁止并存第二套约定。

## 1. 项目定位

- 项目名 `kei`，模块路径 `github.com/RandomLemon/kei`。
- 目标、非目标、总体架构与核心原则见 [`docs/architecture.md`](docs/architecture.md) 第 1、3 章；实现状态、交付物与已知缺口见 [`docs/roadmap.md`](docs/roadmap.md)。

## 2. 硬性规则

### 2.1 分层与依赖

- 公开包只有三个：`pkg/bot`（唯一 SDK，禁止 import `internal/`）、`pkg/message`（消息段构建器，只依赖 `pkg/bot`）、`pkg/kei`（装配门面：唯一允许 import `internal/` 的公开包，只做装配，不含平台分支与业务逻辑）。
- 核心逻辑只放 `internal/`；平台专属代码只允许出现在 `adapters/<platform>/` 或第三方适配器包/module 中；插件代码只放 `plugins/` 或第三方包/module，且只依赖 `pkg/bot`。
- `cmd/` 与 `internal/` 中禁止出现平台名分支（`switch adapter`、`if platform == "feishu"` 等）。适配器只能经 `bot.LookupAdapter` + `internal/adaptermgr` 装配，插件只能经注册表 + `internal/pluginmgr` 装配。
- 适配器与插件同等对待：任何「为某平台写死」的改动都说明注册表被绕过，先修设计再加平台。新增平台的自检问题：只加一个新包（或新 module）+ 一段配置能否跑通？完整示例见 `examples/kei-adapter-myim/`（独立 module，只依赖 `pkg/bot`）。
- 第三方适配器/插件不得要求核心仓库改动任何文件；核心仓库不为第三方改代码。新增/改动注册表 API 时必须同步维护 `examples/kei-adapter-myim/`。
- 配置与密钥只能经配置对象读取（插件 `PluginContext.Config`、适配器 `AdapterContext.Config`）；适配器/插件不得直接读环境变量或文件。

### 2.2 技术栈与依赖

- Go 1.25+（`go.mod` 为 `go 1.25.0`）。工具链、`gopls`、`golangci-lint`、`dlv` 由 `flake.nix` + direnv 提供（`GOTOOLCHAIN=local`），可用命令见 [`docs/testing.md`](docs/testing.md) 第 16 章。
- 优先标准库：`context`、`log/slog`、`net/http`、`encoding/json`、`sync`、`time`。
- 允许的第三方依赖为 `gopkg.in/yaml.v3`（配置）与 `gorm.io/gorm`、`gorm.io/driver/sqlite`、`gorm.io/driver/mysql`（SQL 存储后端）。`gorm.io/driver/sqlite` 底层是 cgo 版 `github.com/mattn/go-sqlite3`，构建需 `CGO_ENABLED=1`（devShell 与 `nix build` 已启用）。新增依赖必须先说明理由，并同步更新 [`docs/architecture.md`](docs/architecture.md) 与 [`README.md`](README.md)。
- 禁止用 Go `plugin` 作为插件机制：适配器与插件都在编译期注册到各自注册表，由配置驱动装配。

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
```

- 公开接口必须有文档注释；注释与实现不符视为缺陷。
- 不破坏 `pkg/bot` 的向后兼容性（公开契约见 [`docs/domain-model.md`](docs/domain-model.md) 5.1）。
- 行为变更（公共 API、配置键）必须同步更新对应 `docs/` 文档与 [`README.md`](README.md)；文档口径与「附：已知缺口与实现边界」的增删规则见 [`docs/README.md`](docs/README.md) 的文档约定。

## 3. 代码约定

- 包名小写、简短，不使用下划线。
- 公开接口必须有文档注释（即第 2.4 节质量门的要求）。
- 时间统一 `time.Time`，时区统一 UTC；ID 一律用字符串，不假设是数字；日志用 `log/slog` 字段化输出。
- 测试使用标准库 `testing`，必要时可用 `testify`（现状：仓库测试全部基于标准库 `testing` 与 `t.Run`，见 [`docs/testing.md`](docs/testing.md) 第 18 章）。
- 错误必须返回不 panic、goroutine 必须可退出、map 并发访问加锁、依赖方向、实例状态与注册表并发安全等条款已在第 2.1、2.3 节定义，本节不重复。

## 4. 测试约定

- 测试断言可观察行为，不断言实现细节（wiring、字段拷贝、日志文案）。
- 每包单测与核心链路集成测试的要求、必测项矩阵、命令与完成定义见 [`docs/testing.md`](docs/testing.md)（第 15、16、18 章）。

## 5. 项目结构

目录结构、各目录职责与第三方适配器独立 module 布局见 [`docs/architecture.md`](docs/architecture.md) 第 4 章；内置适配器与第三方适配器/插件的放置规则见第 2.1 节。

## 6. 文档索引

文档索引、章节↔文件对应与建议阅读顺序见 [`docs/README.md`](docs/README.md)——本文件不再维护第二份索引表。

## 7. 工作流

- 修改导出符号前先查全部调用点（`lsp references`），迁移所有调用方并删除被替代的旧路径，不留兼容垫片。
- 设计歧义时以 `docs/` 为准；文档与代码冲突时以代码为准并同步修改文档（口径见 [`docs/README.md`](docs/README.md) 的文档约定）。文档未覆盖时选最简单、可测试的方案。
- 优先保证 `pkg/bot` 接口稳定：接口稳定比功能多更重要，不过度设计。
