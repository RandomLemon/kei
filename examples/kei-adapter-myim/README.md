# kei-adapter-myim

`MyIM` 平台适配器示例：一个**独立 Go module** 形态的第三方适配器，只依赖
`github.com/RandomLemon/kei/pkg/bot`（公开 SDK + 标准库），不 import 核心仓库
`internal/` 下的任何包，核心仓库不需要为它改动任何文件。

它演示了第三方适配器的完整形态：`init()` 中注册工厂、工厂按每个 `bots[]`
条目构造互相隔离的实例、入站 HTTP 回调先 ACK 再异步上行事件、出站发送前按
平台能力降级。

## 真实第三方仓库的布局

真实场景下它是一个独立仓库（例如 `github.com/example/kei-adapter-myim`），
目录结构与本示例一致：

```text
kei-adapter-myim/
├── go.mod       # require github.com/RandomLemon/kei vX.Y.Z
├── myim.go      # 实现 bot.Adapter（Options/NewFromOptions/Adapter）
├── register.go  # func init() { bot.RegisterAdapter(meta, New) }
├── myim_test.go # 只依赖 pkg/bot + 标准库
└── README.md
```

本仓库内的 `go.mod` 用 `replace` 指向根 module，便于本地一起开发：

```go
module github.com/example/kei-adapter-myim

go 1.25.0

require github.com/RandomLemon/kei v0.0.0

replace github.com/RandomLemon/kei => ../..
```

`replace` 只对本地开发有意义：`require` 的行内伪版本 `v0.0.0` 只有在 `replace`
存在时才可解析。发布成真实第三方仓库时，删掉 `replace`，把 `require` 换成实际
版本号（例如 `github.com/RandomLemon/kei v0.3.0`）：

```go
require github.com/RandomLemon/kei v0.3.0
```

## 接入核心只有两步

1）主程序空导入本 module，触发 `init()` 注册（注册只记录元信息与工厂，不构造实例）：

```go
import _ "github.com/example/kei-adapter-myim"
```

2）配置里把 bot 的 `adapter` 指向注册名 `myim`：

```yaml
bots:
  - name: myim-main          # 实例名，即 bot.AdapterContext.BotID
    adapter: myim            # 注册名，与 AdapterMetadata.Name 一致
    api_base: https://im.example.com   # 平台开放接口，Send 拼接 /message/send
    listen_addr: 127.0.0.1:19081       # 入站回调监听地址（保留键，需 PermNetListen）
    path: /myim/callback               # 入站回调路径，缺省 "/"
    self_id: "10001"                   # 平台侧机器人账号，用于生成兜底事件 ID
```

可同时配置多个 `adapter: myim` 的条目（例如两个 MyIM 机器人）：每个条目调用一次
工厂，得到独立实例，各自监听自己的 `listen_addr`、各自持有自己的 BotID。

私有配置键与 `AdapterMetadata.Options` 完全一致，也即 `Options` 的字段：

| 配置键 | 对应字段 | 必填 | 说明 |
| --- | --- | --- | --- |
| `api_base` | `Options.APIBase` | 否（不发送时必须） | 平台开放接口地址，为空时 `Send` 报错 |
| `listen_addr` | `Options.ListenAddr` | 是 | 入站监听地址，如 `127.0.0.1:19081`；声明了 `net_listen` 权限才允许出现 |
| `path` | `Options.Path` | 否 | 回调路径，缺省 `/`；非空时必须以 `/` 开头 |
| `self_id` | `Options.SelfID` | 否 | 机器人账号，仅用于生成兜底事件 ID，为空时用 BotID 代替 |

`Options` 另有三个非配置字段：`BotID`（来自 `AdapterContext.BotID`）、
`HTTPClient`（来自 `AdapterContext.HTTPClient`，需声明 `network` 权限）、
`Logger`（来自 `AdapterContext.Logger`）。它们由核心注入，不写进配置。

## 声明的权限与能力

元信息（`register.go`）：

| 项 | 值 |
| --- | --- |
| 注册名 / 平台 | `myim` |
| 版本 / 作者 | `v0.1.0` / `third-party` |
| 权限 | `network`（出站发送）、`net_listen`（入站回调） |

权限与实现的对应关系：未声明 `network` 时 `AdapterContext.HTTPClient` 为 `nil`，
工厂直接报错而不是回落到自带客户端；声明了 `net_listen` 才允许配置里出现保留键
`listen_addr`。

能力（`Capabilities()`）只声明本平台真实支持的子集，其余为 `false`，因此
`bot.Degrade` 的降级路径可被直接演示：

| 能力 | 值 | 效果 |
| --- | --- | --- |
| `Text` / `Private` / `Group` / `Reply` / `At` | `true` | 原样发送 |
| `Markdown` / `Image` / `Card` / `File` | `false` | 发送前降级为文本段（图片为 `[image] <url>`、文件为 `[file] <name> <url>`、卡片为 `[card] <json>`） |

降级后整条消息为空时 `Send` 返回错误，不会静默丢消息。

## 行为约定

- `Start` 先 `net.Listen` 再 `go srv.Serve`，因此它尚在阻塞时端口就已可接收请求；
  随后阻塞到 `ctx.Done()`。
- 平台回调先收到 `202 {"code":0}`，事件再经带缓冲队列交给独立 goroutine 的
  `sink.Emit`，回调线程不会因业务处理而阻塞；队列满时记 warn 日志。
- 事件 ID 优先取平台事件自带的 `id`，缺失时用 `myim-<self_id>-<seq>` 兜底，
  保证同实例内稳定唯一；`Event.Platform` 恒为 `myim`，`Event.BotID` 恒为构造时的 BotID。
- `Stop` 幂等：`srv.Shutdown(ctx)` + 关闭队列 + 等待投递 goroutine 退出，重复调用都返回 `nil`。
- 实例状态（监听器、队列、序号）全部挂在 `Adapter` 上，包级没有可变状态，因此多实例互不影响。

## 运行测试

```bash
cd examples/kei-adapter-myim
go test ./...
```

测试只依赖 `pkg/bot` 与标准库，覆盖：注册表元信息、工厂权限校验、构造参数校验、
`Start`/`Stop` 生命周期、回调先 ACK 后投递、事件上行（平台/BotID/ID 稳定性）、
ID 兜底、多实例隔离、发送请求与能力降级、发送错误路径。

本示例由本仓库的 `examples/` 目录承载，不参与根 module 的构建；根 module 的
`go build ./...` 不会编译它（独立 module 不在根 module 的包集合内）。
