# kei 设计文档

`kei` 的设计与实现说明。硬性规则（分层、依赖、运行时契约、质量门、代码约定）与目录概览在仓库根 [`AGENTS.md`](../AGENTS.md)；面向使用者的安装与用法在 [`README.md`](../README.md)。本目录是**实现口径的唯一文档来源**：文档以代码为准，与代码不符视为缺陷。

## 章节与文件对应

各文档沿用早期设计稿的章节号（1-20，`docs` 内连续编号），便于跨文档引用（如「见 6.4」「见 12.3」）。

| 章 | 主题 | 文档 |
| --- | --- | --- |
| 1、3、4 | 项目概述、总体架构、目录结构与模块职责 | [`architecture.md`](architecture.md) |
| 5 | `pkg/bot` 领域模型：Event/Message/Segment/Command/User/Channel、`Segment.Data` 键约定、消息段构建器 | [`domain-model.md`](domain-model.md) |
| 6 | 适配器平台适配层：接口、注册表与工厂、权限、能力与降级、第三方接入、内置适配器现状 | [`adapter.md`](adapter.md) |
| 7、8、10 | Engine、EventBus 事件总线、路由与中间件 | [`engine.md`](engine.md) |
| 9、11 | 插件系统、BotAPI 与插件上下文（Storage/权限/测试辅助） | [`plugin.md`](plugin.md) |
| 12 | 外部插件与外部适配器 gRPC 协议、代码生成、崩溃隔离与重连 | [`grpc.md`](grpc.md) |
| 13 | 配置：结构、键语义、环境变量覆盖、校验与告警 | [`configuration.md`](configuration.md) |
| 14、20 | 实现阶段回顾与交付物现状（含未实现项） | [`roadmap.md`](roadmap.md) |
| 16、17、19 | 测试要求、构建与运行命令、完成定义（Definition of Done） | [`testing.md`](testing.md) |

第 2 章（技术栈与约束）、第 15 章（代码约定）、第 18 章（实现守则）是硬性规则，保留在 [`AGENTS.md`](../AGENTS.md)。第 19 章的完成定义在 `testing.md`，`AGENTS.md` 只保留合并前必须通过的门槛命令。

## 建议阅读顺序

1. [`architecture.md`](architecture.md)：先建立分层与数据流模型（适配器 → 事件总线 → 引擎 → 插件 → 回复）。
2. [`domain-model.md`](domain-model.md)：`pkg/bot` 是唯一公开 SDK，先掌握类型与 `Segment.Data` 键约定。
3. 按角色选读：
   - 写平台适配器 → [`adapter.md`](adapter.md)；独立进程形态 → [`grpc.md`](grpc.md)。
   - 写插件 → [`plugin.md`](plugin.md)；事件投递与路由语义 → [`engine.md`](engine.md)。
   - 部署与运维 → [`configuration.md`](configuration.md) + [`README.md`](../README.md)。
4. [`testing.md`](testing.md)：动手前确认质量门与测试要求。
5. [`roadmap.md`](roadmap.md)：确认哪些能力已实现、哪些是已知缺口。

仓库内的独立 module 示例 [`../examples/kei-adapter-myim/`](../examples/kei-adapter-myim/)（自带 `go.mod`，只依赖 `pkg/bot`）是第三方适配器的真实形态；它与 `adapter.md` 第 6 章、`architecture.md` 4.1 节配套阅读。

## 文档约定

- 章号与节号沿用早期设计稿：`## 6. …` 是章，`### 6.4 …` 是节，跨文档引用使用相对链接 + 节号锚点（如 `adapter.md#64-能力与降级`）。
- 文档中的接口签名、字段名、配置键、默认值、错误文案均取自源码；描述与代码冲突时以代码为准，并同步修改文档。
- 「附：已知缺口与实现边界」小节记录与理想设计不符的现状（未接指标、未实现的开关等），修掉缺口后必须同步删除该条。
- 新增文档时同步更新本索引与 [`AGENTS.md`](../AGENTS.md) 的文档索引表。

## 相关文档

- [`AGENTS.md`](../AGENTS.md)：全局硬性规则、代码约定、质量门、项目结构概览
- [`README.md`](../README.md)：安装、快速开始、配置示例、插件/适配器开发指南、可观测性
