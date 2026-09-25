# kei 架构总览

覆盖 kei 的项目概述（目标与非目标）、总体架构（组件拓扑、事件链路、核心原则）与仓库实际目录结构（含第三方适配器独立 module 布局），即第 1、3、4 章。
不覆盖：领域模型的类型与接口定义（见 domain-model.md）、适配器/插件/引擎的实现细节（见 adapter.md、plugin.md、engine.md）、配置键表（见 configuration.md）、测试与构建要求（见 testing.md）。
面向用户的安装、快速开始与示例用法见 [../README.md](../README.md)。

---

## 1. 项目概述

项目名称：`kei`
模块路径：`github.com/RandomLemon/kei`

目标：构建一个 Go 语言编写的 chatbot 框架，满足：

1. 可接入不同 IM 平台：QQ、微信、飞书等。
2. 平台协议与业务逻辑解耦。
3. 第三方开发者可以方便快捷地开发功能插件。
4. 插件支持消息处理、回复、命令触发、正则触发、关键词触发、事件监听等能力。
5. 第三方开发者可以像开发插件一样开发平台适配器：在自己的包内注册到注册表即可接入，无需改动核心代码。
6. 核心引擎稳定，平台适配器与插件均可进程内加载，也可扩展到外部 gRPC 进程。

非目标：

1. 不实现个人微信逆向协议。
2. 不实现完整管理后台 UI。
3. 不实现复杂的自然语言处理模型。
4. 不追求一次性覆盖所有 IM 平台：仓库内置适配器为 `adapters/mock`、`adapters/onebot`（OneBot v11）、`adapters/feishu` 三种，其余平台按 [adapter.md](adapter.md) 第 6 章的适配器契约由第三方以独立包或独立 module 形式接入。

当前内置适配器的注册与启用方式：`cmd/bot/main.go` 空导入三个内置适配器包，各包在自己的 `init()` 中调用 `bot.RegisterAdapter`，是否实例化由配置中的 `bots[].adapter` 决定。

---

## 3. 总体架构

```text
飞书 / OneBot / Mock / 第三方适配器
        │
        ▼
┌──────────────────┐
│   Adapter 适配层  │  签名校验、协议解析、事件转换、API 调用
└──────────────────┘
        │ 统一 Event（bot.Event）
        ▼
┌──────────────────┐
│   EventBus 事件总线 │  分片队列、并发、按会话保序、去重
└──────────────────┘
        │
        ▼
┌──────────────────────┐
│   Engine 核心引擎      │
│  Middleware 链        │  recover/logger/metrics/timeout/dedup/ratelimit/auth
│  Router 路由          │  命令、正则、关键词、事件类型
│  Plugin Handler       │  第三方插件处理
└──────────────────────┘
        │ Reply / BotAPI
        ▼
┌──────────────────┐
│   Adapter.Send    │  统一消息段转平台消息（发送前经 bot.Degrade 降级）
└──────────────────┘
```

与上图对应的实际包：

- 适配层：`adapters/mock`、`adapters/onebot`、`adapters/feishu`；外部适配器经 `internal/adaptermgr/external` 代理。
- 事件总线：`internal/eventbus`，按 `bot.Event.SessionKey` 哈希分片，同一会话由同一 goroutine 串行处理，去重由 `internal/dedup` 提供。
- 引擎：`internal/engine`（BotAPI 实现与发送链路）、`internal/middleware`（中间件）、`internal/router`（路由与 Registrar 实现）、`internal/pluginmgr`（插件生命周期与外部插件加载）。
- 回复与发送：`internal/reply` 提供 `bot.Reply` 默认构建器；发送统一经 `bot.Degrade` 降级后交给 `bot.Adapter.Send`。
- 外部进程通道：`internal/grpcsrv` 提供 `BotService` 服务端，插件与外部适配器共用。

核心原则：

1. 平台无关：插件不直接依赖 QQ、飞书、微信 SDK。
2. 插件轻量：第三方只需实现接口、注册命令/正则/关键词。
3. 适配器可插拔：新增平台只需实现 Adapter 并在自己的包（可以是独立 module）内注册，核心代码零改动。
4. 插件与适配器同构：同样的注册表、元信息与权限声明、配置驱动装配、以及外部 gRPC 进程形态。
5. 消息段统一：文本、图片、At、Markdown、卡片等统一为 Segment。
6. 动态扩展优先用外部进程/gRPC，而不是 Go `plugin`。

---

## 4. 目录结构

```text
kei/
├── AGENTS.md                     # 全局硬性规则与文档索引
├── README.md                     # 面向用户的介绍、安装与快速开始
├── LICENSE                       # 开源许可证
├── go.mod                        # module github.com/RandomLemon/kei
├── go.sum                        # 依赖校验和
├── flake.nix                     # Nix devShell / 构建 / 运行入口
├── flake.lock                    # flake 输入锁定
├── .envrc                        # direnv 集成：进入目录自动加载 devShell
├── .gitignore                    # 忽略构建产物与本地环境
├── cmd/
│   ├── bot/                      # 入口：加载配置、空导入内置适配器与插件、装配、优雅退出，不含平台分支
│   ├── example-plugin/           # Go 外部插件示例（独立进程，实现 PluginService）
│   └── example-adapter/          # Go 外部适配器示例（独立进程，实现 AdapterService）
├── pkg/
│   ├── bot/                      # 公开 SDK：Event、Message、Adapter 注册表、Plugin、Registrar、Reply、BotAPI、Storage、Config
│   └── message/                  # 消息与消息段构建器
├── internal/
│   ├── engine/                   # 核心引擎：串联适配器、事件总线、路由与插件，对外提供 BotAPI
│   ├── adaptermgr/               # 适配器装配：注册表查表、权限裁剪、保留键校验
│   │   └── external/             # 外部适配器通道：作为客户端 dial AdapterService，代理 Send/Start/Stop
│   ├── pluginmgr/                # 插件注册与生命周期：Setup/Start 顺序执行，Stop 逆序，panic 隔离与超时保护
│   │   └── external/             # 外部插件加载通道：作为客户端 dial PluginService，投递事件
│   ├── grpcsrv/                  # BotService gRPC 服务端（插件与外部适配器共用）
│   ├── router/                   # 路由匹配与 Registrar 实现：规则表不可变快照 + 原子替换
│   ├── middleware/               # 中间件：Recover、Logger、Metrics、Timeout、Dedup、RateLimit、Auth
│   ├── eventbus/                 # 事件总线：分片保序、去重、优雅关闭前排空队列
│   ├── config/                   # YAML 配置加载、合并与校验，环境变量覆盖
│   ├── storage/                  # bot.Storage 内存实现与权限拒绝实现
│   ├── dedup/                    # 带 TTL 与容量上限的去重集合
│   ├── ratelimit/                # 按 key 的令牌桶：非阻塞 Allow 与阻塞 Wait
│   ├── metrics/                  # 指标注册表与 Recorder 实现（Prometheus 文本端点）
│   └── reply/                    # bot.Reply 默认构建器：链式累积消息段，Send 时一次发送
├── adapters/                     # 内置适配器；第三方适配器不放在这里（见 4.1）
│   ├── mock/                     # 本地测试适配器：HTTP 控制面注入事件、观察发送
│   ├── onebot/                   # OneBot v11：HTTP 上报 + HTTP API + 反向 WebSocket
│   └── feishu/                   # 飞书开放平台：事件订阅回调 + 消息发送
├── plugins/
│   ├── echo/                     # 示例插件：/echo
│   └── manage/                   # 管理命令：/ping、/version、/plugins、/adapters、/admin
├── proto/
│   ├── plugin.proto              # 外部插件协议 + BotService（含 EmitEvent）
│   ├── adapter.proto             # 外部适配器协议（同 package/go_package，import plugin.proto 复用消息）
│   ├── buf.yaml                  # buf 模块与 lint/breaking 配置
│   ├── buf.gen.yaml              # 代码生成配置：输出到 pluginpb/，paths=source_relative
│   └── pluginpb/                 # 生成代码：plugin.pb.go、plugin_grpc.pb.go、adapter.pb.go、adapter_grpc.pb.go
├── configs/
│   └── config.yaml               # 示例配置（log、metrics、limits、auth、grpc、adapters、bots、plugins）
└── docs/                         # 设计文档集，入口为 docs/README.md 文档索引
```

`pkg/bot`、`internal/`、`adapters/`、`plugins/` 的职责划分是硬约束：`cmd/` 与 `internal/` 中不得出现平台名分支，所有平台细节位于 `adapters/<platform>` 或第三方适配器包内。

### 4.1 第三方适配器独立 module 布局

第三方适配器放在独立包或独立 module 中，只依赖 `pkg/bot`，核心仓库不为它改动任何文件（与第三方插件完全一致）：

```text
kei-adapter-myim/         # 独立仓库/模块
├── go.mod                # require github.com/RandomLemon/kei
├── myim.go               # 实现 bot.Adapter
├── register.go           # func init() { bot.RegisterAdapter(meta, New) }
└── README.md
```

接入方式只有两步：主程序空导入该 module，配置里 `bots[].adapter: myim`（外部进程形态则在 `adapters:` 段声明 `grpc_addr`）。

---

## 相关文档

- [domain-model.md](domain-model.md)：`pkg/bot` 的公开领域模型与消息段。
- [adapter.md](adapter.md)：适配器接口、注册表与工厂、元信息与权限、能力降级。
- [engine.md](engine.md)：Engine、EventBus、路由与中间件。
- [configuration.md](configuration.md)：配置键、默认值与环境变量覆盖规则。
