# kei

可插拔的 Go chatbot 框架：平台协议与业务逻辑解耦，第三方可以只依赖 `pkg/bot`
开发插件，新增平台只需实现一个 `Adapter`。

```
QQ / 飞书 / OneBot / Mock
        │  平台回调（签名校验、协议解析、快速 ACK）
        ▼
┌──────────────────┐
│   Adapter 适配层  │  事件转换 → 统一 bot.Event ─┐
└──────────────────┘                            │
        ▲                                       ▼
        │ Send                      ┌──────────────────────┐
        │                           │  EventBus 事件总线    │ 分片 worker 池
        │                           │  按会话哈希保序 + 去重 │
        │                           └──────────────────────┘
        │                                       │
        │                                       ▼
        │                           ┌──────────────────────┐
        │                           │  Engine 核心引擎      │
        │                           │  Middleware 链（洋葱）│
        │                           │  Router 路由匹配      │
        └───────────────────────────│  Plugin Handler       │
                                    └──────────────────────┘
```

- **统一消息段**：文本、Markdown、图片、At、表情、引用、卡片、文件都归一为 `bot.Segment`，
  平台不支持时由 `bot.Degrade` 自动降级。
- **事件总线**：`platform:botID:channelID:userID` 哈希分片，同一会话严格保序，不同会话并行；
  `事件 ID + TTL` 去重；优雅关闭前排空已入队事件。
- **中间件**：Recover / Logger / Metrics / Timeout / Auth / RateLimit / Dedup 可组合，洋葱模型。
- **插件**：编译期插件通过 `init()` 注册、配置启用；也可以作为**独立进程**通过 gRPC 接入
  （token/mTLS 鉴权），核心不因插件崩溃而退出。
- **零第三方运行时依赖**：仅 `gopkg.in/yaml.v3`、`google.golang.org/grpc`、`google.golang.org/protobuf`。

## 目录结构

```
cmd/bot/                 入口：加载配置、装配适配器与插件、优雅退出
cmd/example-plugin/      Go 外部插件示例（独立进程）
cmd/example-adapter/     Go 外部适配器示例（独立进程）
pkg/bot/                 公开 SDK：Event/Message/Adapter 注册表/Plugin/Registrar/Reply/BotAPI
pkg/message/             消息段构建器
internal/engine/         核心引擎（BotAPI 实现、发送限流与重试、优雅关闭）
internal/eventbus/       事件总线（分片保序、去重、drain）
internal/router/         路由匹配与 Registrar 实现
internal/middleware/     中间件（Recover/Logger/Metrics/Timeout/Auth/RateLimit/Dedup）
internal/metrics/        Prometheus 文本指标（标准库实现）
internal/pluginmgr/      插件生命周期管理 + gRPC 外部插件加载
internal/adaptermgr/     适配器装配：注册表查表、权限裁剪、外部适配器通道
internal/grpcsrv/        BotService gRPC 服务端（插件与外部适配器共用）
internal/config/         YAML 配置加载与环境变量覆盖
internal/storage/        bot.Storage 内存实现
internal/dedup/          带 TTL 与容量的去重集合
internal/ratelimit/      按 key 的令牌桶（非阻塞 Allow + 阻塞 Wait）
adapters/mock/           本地测试适配器（HTTP 控制面注入事件、观察发送）
adapters/onebot/         OneBot v11（HTTP 上报 + HTTP API）
adapters/feishu/         飞书开放平台（事件订阅回调 + 消息发送）
plugins/echo/            示例插件：/echo
plugins/manage/          管理命令：/ping、/version、/plugins、/adapters、/admin
proto/plugin.proto       外部插件 gRPC 协议 + BotService（含生成代码 proto/pluginpb）
proto/adapter.proto      外部适配器 gRPC 协议（同 package，复用 plugin.proto 消息）
configs/config.yaml      示例配置
```

## 环境准备（Nix + direnv）

Go 工具链、`gopls`、`golangci-lint`、`dlv`、`protoc`/`buf` 全部由 flake 提供，不需要在系统里装 Go。

```bash
# 方式一：direnv（推荐，进入目录自动加载 / 离开自动还原）
direnv allow .          # 首次执行一次；之后 cd 进目录即自动生效
go version              # go1.26.7（来自 flake devShell）

# 方式二：显式进入 devShell
nix develop
nix develop --command go test -race ./...

# 可选：安装 nix-direnv 可让 devShell 复用缓存与 GC root（.envrc 会自动识别）
```

`flake.nix` 提供的输入：

| 用途 | 命令 |
| --- | --- |
| 开发环境 | `nix develop` |
| 构建二进制（含 `go test`） | `nix build` → `result/bin/bot` |
| 直接运行 | `nix run . -- -config configs/config.yaml` |
| 格式化 flake | `nix fmt` |
| 校验 flake | `nix flake check` |

`GOTOOLCHAIN=local` 已在 devShell 中固定，Go 不会去下载别的 toolchain。

## 快速开始

```bash
# 1) 启动（示例配置默认只跑 mock 适配器 + echo/manage 插件）
nix develop --command go run ./cmd/bot -config configs/config.yaml

# 2) 另开一个终端：注入一条命令事件
curl -sS -XPOST 127.0.0.1:18080/inject \
  -H 'content-type: application/json' \
  -d '{"text":"/echo hello kei"}'
# {"event_id":"mock-..."}

# 3) 查看机器人实际发出的消息
curl -sS 127.0.0.1:18080/sent | jq '.[0].Request.Message.Segments'
# [{"Type":"text","Data":{"text":"hello kei"}}]

# 4) 查看指标
curl -sS 127.0.0.1:19090/metrics | grep kei_events

# 5) Ctrl-C 优雅退出（停止接收 → 排空已入队事件 → 停止适配器 → 停止插件）
```

`adapters/mock` 的 HTTP 控制面（仅在配置了 `listen_addr` 时启动）：

| 方法/路径 | 说明 |
| --- | --- |
| `POST /inject` | 注入事件，JSON 字段：`text`/`user_id`/`user_name`/`channel_id`/`kind`/`id`/`type`；返回 `202` |
| `GET /sent` | 已发送消息记录（含结果与错误） |
| `DELETE /sent` | 清空记录 |
| `GET /healthz` | 存活检查 |

## 配置

`configs/config.yaml` 是完整示例。结构：

```yaml
log:     { level: info, format: text }     # text | json
metrics: { addr: 127.0.0.1:19090 }         # 空表示不暴露
limits:  { handler_rate: 0, send_rate: 0 } # 0 表示不限流
auth:    { admin_users: [] }               # 配合 bot.WithAdmin() 规则
grpc:    { addr: "", cert_file: "", key_file: "", ca_file: "" }  # 外部插件/适配器通道
adapters:                                        # 可选：启用/禁用、外部适配器声明
  feishu:  { enabled: false }                    # 示例：禁用内置适配器（其 bot 一并跳过）
  myim:    { enabled: true, grpc_addr: 127.0.0.1:50071, token: change-me }  # 示例：外部适配器
bots:
  - name: mock-main
    adapter: mock
    platform: mock
    listen_addr: 127.0.0.1:18080
plugins:
  echo:    { enabled: true }
  manage:  { enabled: true }
```

要点：

- `bots[].adapter` 决定使用哪个适配器，其余键按适配器要求传入（拼错会打 warn 日志）。
- 同一个平台可以配置多个 bot（例如两个飞书应用），此时发送必须显式指定 `Target.BotID`。
- `bots[].plugins` 是该 bot 的插件白名单；只要有一个 bot 配置了非空白名单，规则就会按
  「哪些 bot 允许它」收窄（未配置白名单的 bot 允许全部）。
- `plugins.<name>` 未列出 = 不启用；想启用必须写 `enabled: true`。
- `adapters.<name>.enabled` 控制适配器开关，**缺省 true**（与插件相反）：写
  `enabled: false` 即不加载该适配器，其 `bots[]` 条目一并跳过（每个跳过的 bot 记 warn）。
  声明 `grpc_addr` 表示该适配器是外部进程，外部条目还要求 `token`。

### 环境变量覆盖

任意配置项都可以用 `KEI_` 前缀覆盖：路径段用 `_` 连接、忽略大小写，`-`/`.` 等价于 `_`。

```bash
KEI_LOG_LEVEL=debug
KEI_METRICS_ADDR=127.0.0.1:9090
KEI_LIMITS_SEND_RATE=5
KEI_AUTH_ADMIN_USERS=ou_xxx,123456
KEI_BOTS_FEISHU_MAIN_APP_SECRET=xxx      # bot 名 feishu-main → 其 app_secret
KEI_PLUGINS_WEATHER_API_KEY=xxx
```

值的类型按 YAML 规则推断（`true`→bool、`3`→int、其余→string），所以 `enabled: "false"` 也能生效。
未匹配任何已知路径的 `KEI_*` 变量会被忽略。

## 写一个插件

插件只依赖 `pkg/bot`，通过 `init()` 注册，由配置启用。

```go
package hello

import (
	"context"

	"github.com/RandomLemon/kei/pkg/bot"
)

type Plugin struct{}

func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name: "hello", Version: "v0.1.0", Author: "you",
		Description: "打招呼",
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermStorage},
	}
}

func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	// 命令触发：/hello <名字>
	reg.OnCommand("hello", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		return r.Text("你好！").Send(ctx)
	}, bot.WithPriority(100))

	// 正则触发（捕获组通过 bot.RouteFrom(ctx).RegexMatches 读取）
	reg.OnRegex(`^你好\s+(\S+)$`, func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		name := "世界"
		if rt, ok := bot.RouteFrom(ctx); ok && len(rt.RegexMatches) > 1 {
			name = rt.RegexMatches[1]
		}
		return r.Text("你好，" + name).Send(ctx)
	})

	// 关键词触发
	reg.OnKeyword([]string{"help", "帮助"}, func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		return r.Text("可用命令：/hello").Send(ctx)
	})

	// 事件类型触发
	reg.OnEvent(bot.EventNotice, func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		return nil
	})

	// 兜底：处理所有没被上面条件排除的消息
	reg.OnAll(func(ctx context.Context, e *bot.Event, r bot.Reply) error { return nil },
		bot.WithPriority(-100))

	return nil
}

func (p *Plugin) Start(context.Context) error { return nil }
func (p *Plugin) Stop(context.Context) error  { return nil }

func init() { bot.RegisterPlugin(&Plugin{}) }
```

规则语义：

- 同一事件可以命中**多条**规则，按 `Priority` 降序依次执行（同优先级按注册顺序）；
  多插件协作时用 `WithPriority(-100)` 之类把兜底逻辑放到最后。
- 过滤条件之间是「与」：`WithPlatforms`/`WithKind`/`WithBotIDs`/`WithMatch` 可自由组合。
- 每个插件可以用 `reg.Use(mw)` 追加自己的中间件（只影响其后注册的规则）。
- 插件运行期依赖通过 `bot.PluginContextFrom(ctx)` 获取：

  | 字段 | 说明 |
  | --- | --- |
  | `Config` | 该插件在 `plugins.<name>` 下的配置（`Get/String/Bool/Int/Duration/Strings/Unmarshal`） |
  | `Logger` | 已绑定 `plugin` 字段的 `*slog.Logger` |
  | `Storage` | 键值存储（未声明 `PermStorage` 时返回拒绝访问的实现） |
  | `HTTPClient` | 带超时的 HTTP 客户端（未声明 `PermNetwork` 时为 nil） |
  | `Bot` | 唯一的平台出口（`Send` 需要 `PermSendMessage`，`Reply` 始终可用） |
  | `Catalog` | 已加载插件元信息（`/plugins` 这类管理命令用） |

插件可以只用 `pkg/bot` 做单元测试，无需启动引擎：

```go
reg := bot.NewRecordingRegistrar()
_ = (&Plugin{}).Setup(ctx, reg)
rule, _ := reg.Command("hello")
reply := bot.NewNoopReply()
_ = rule.Handler(ctx, ev, reply)
fmt.Println(reply.PlainText())
```

## 写一个适配器

```go
type Adapter interface {
	Name() string
	Start(ctx context.Context, sink EventSink) error // 阻塞直到 ctx 结束
	Stop(ctx context.Context) error
	Send(ctx context.Context, req *SendRequest) (*SendResult, error)
	Capabilities() Capabilities
}
```

约定：

- `Start` 必须在返回前完成监听/连接，随后阻塞到 `ctx.Done()`；`Stop` 幂等。
- 平台回调要在超时前返回（飞书 3 秒），业务事件经 `sink.Emit` 异步投递。
- 用 `bot.Key*` 常量读写 `Segment.Data`，不要写字面量键。
- 发送前调用 `bot.Degrade(msg, a.Capabilities())`，平台不支持的消息段会自动降级为文本。
- `Event.ID` 用于去重，必须稳定（飞书用 `header.event_id`，OneBot 用 `message_id`）。

已内置的适配器与能力：

| 适配器 | 接入方式 | 能力 |
| --- | --- | --- |
| `mock` | HTTP 控制面注入/观察 | 全能力，用于本地联调与测试 |
| `onebot` | HTTP 上报（`Authorization: Bearer <secret>`）+ HTTP API | 文本/图片/At/表情/引用/文件；不支持 Markdown、卡片 |
| `feishu` | 事件订阅回调（challenge、签名校验、AES 解密、3s 内 ACK）+ 开放平台 API | 文本/Markdown/图片/At/卡片/引用/文件 |

适配器与插件一样由注册表驱动：适配器在自己的包内 `init()` 注册「元信息 + 工厂」，
主程序只做空导入，`bots[].adapter` 按注册名装配。因此第三方适配器可以是独立包或
独立 module（只依赖 `pkg/bot`），**核心仓库无需任何改动**。

```go
package myim

func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        "myim",
		Version:     "v0.1.0",
		Author:      "third-party",
		Description: "MyIM 平台适配器",
		Platforms:   []string{"myim"},                                 // 写入 Event.Platform
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
		Options:     []string{"api_base", "listen_addr"},              // 未知配置键会告警
	}, New)
}

// New 每个 bot 实例调用一次，必须是互相隔离的实例。
func New(ac bot.AdapterContext) (bot.Adapter, error) {
	if ac.HTTPClient == nil {
		return nil, errors.New("myim: 未声明 network 权限")
	}
	return &Adapter{
		botID:   ac.BotID,                                  // 必须写入 Event.BotID / SendRequest.BotID
		apiBase: ac.Config.String("api_base", ""),
		http:    ac.HTTPClient,
		log:     ac.Logger.With("bot", ac.BotID, "adapter", "myim"),
	}, nil
}
```

启用只需空导入 + 配置：

```yaml
bots:
  - name: myim-main
    adapter: myim
    api_base: https://im.example.com
    listen_addr: 127.0.0.1:18091
```

权限由核心在装配期裁剪，声明必须如实：

| 权限 | 拿到什么 | 未声明时 |
| --- | --- | --- |
| `network` | `AdapterContext.HTTPClient` | 为 `nil`，工厂应报错而不是回落到自带客户端 |
| `storage` | `AdapterContext.Storage` | 注入拒绝式存储（`ErrPermissionDenied`） |
| `net_listen` | 允许监听入站端口 | 配置里出现保留键 `listen_addr` 时启动失败 |
| `receive_event` | 外部适配器经 `BotService.EmitEvent` 投递事件 | 该调用被拒绝 |

`/adapters` 会列出已注册适配器与每个 bot 的绑定关系（含进程内/外部 gRPC 标记）。

临时停用某个平台不用删配置，加一行即可（其 `bots[]` 条目一并跳过）：

```yaml
adapters:
  feishu: { enabled: false }   # 缺省为 true；也可用 KEI_ADAPTERS_FEISHU_ENABLED=false
```

独立 module 形式（推荐给第三方）：

```text
kei-adapter-myim/
├── go.mod      # require github.com/RandomLemon/kei
├── myim.go     # 实现 bot.Adapter
└── register.go # init() 中 RegisterAdapter
```

想用其他语言写适配器（独立进程 + gRPC）见下一节。

## 外部适配器（gRPC）

适配器也可以作为独立进程运行（Python/Node/Rust 均可按 `proto/adapter.proto` 实现）：

- 核心提供 `BotService`（`EmitEvent`/`GetConfig`/`Log`），用 `token` 鉴权、可选 mTLS；
- 核心作为客户端连接适配器进程的 `AdapterService`（`Init`/`Start`/`Stop`/`Send`/`Shutdown`），
  `Init` 一次、每个绑定的 bot `Start` 一次，因此一个进程可服务多个 bot；
- 事件上行只经 `BotService.EmitEvent`（需 `receive_event` 权限，归属校验由核心完成），
  适配器要先 ACK 平台再投递；断连后核心按指数退避重连，达上限只停用相关 bot，主进程不受影响。

1）在配置里声明外部适配器（`grpc.addr` 必须是固定端口）：

```yaml
grpc:
  addr: 127.0.0.1:19070            # 核心 BotService 监听地址
adapters:
  example:
    enabled: true                  # 缺省 true；false 则不 dial、并跳过 example-main
    grpc_addr: 127.0.0.1:19071     # 适配器 AdapterService 地址
    token: change-me               # 必填
    platform: example              # 适配器上报多个平台时必填
    timeout: 10s
    permissions: [receive_event, network, net_listen]
    listen_addr: 127.0.0.1:18091   # 其余键作为进程级配置下发
bots:
  - name: example-main
    adapter: example
```

2）启动核心与适配器进程：

```bash
nix develop --command go run ./cmd/bot -config configs/config.yaml

nix develop --command go run ./cmd/example-adapter \
  -listen 127.0.0.1:19071 -platform example -control 127.0.0.1:19081

# 模拟平台推来一条消息（适配器 ACK 后经 EmitEvent 投递，由插件回复）
curl -sS -XPOST 127.0.0.1:19081/inject -H 'content-type: application/json' \
  -d '{"bot_id":"example-main","text":"/echo hi from adapter"}'
curl -sS '127.0.0.1:19081/sent?bot_id=example-main' \
  | jq -r '.[-1].message.segments[0].data_json | fromjson | .text'
# "hi from adapter"
```

适配器进程的 `-listen`/`-platform`/`-control` 见 `cmd/example-adapter`：它实现了
`AdapterService`，把 `/inject` 收到的文本包装成 `bot.Event` 经 `EmitEvent` 投递，
并记录收到的发送请求。真实适配器把它换成平台 SDK 调用即可。

## 外部插件（gRPC）

插件可以作为独立进程运行（Python/Node/Rust 均可按 `proto/plugin.proto` 实现）：

- 核心提供 `BotService`（`SendMessage`/`GetConfig`/`Log`），并用 `token` 鉴权、可选 mTLS；
- 核心作为客户端连接插件进程的 `PluginService`（`Init`/`HandleEvent`/`Shutdown`），
  插件地址来自配置，`HandleEvent` 收到**所有**事件（等价 `OnAll`）；
- 插件通过 `BotService.SendMessage` 回调核心发消息，因此插件崩溃不影响主进程。

1）在配置里打开通道并声明插件：

```yaml
grpc:
  addr: 127.0.0.1:19070          # 核心 BotService 监听地址
  # cert_file/key_file/ca_file 三个都给齐才启用 TLS；给了 ca_file 即启用 mTLS
plugins:
  external-echo:
    enabled: true
    grpc_addr: 127.0.0.1:19071   # 插件进程的 PluginService 地址
    token: change-me             # 必填
    permissions: send_message    # 默认 send_message
    greeting: 你好，我是外部插件
```

2）启动核心与插件进程：

```bash
nix develop --command go run ./cmd/bot -config configs/config.yaml

# 插件进程（先起或与核心同时起都可以：核心会在超时窗口内重试连接插件，
# 插件也会在 10s 内等待核心的 BotService 就绪）
nix develop --command go run ./cmd/example-plugin \
  -listen 127.0.0.1:19071 -core-addr 127.0.0.1:19070 -token change-me

curl -sS -XPOST 127.0.0.1:18080/inject -H 'content-type: application/json' \
  -d '{"text":"/ext hi"}'
curl -sS 127.0.0.1:18080/sent | jq '.[-1].Request.Message.Segments[0].Data.text'
# "你好，外部插件: hi"
```

示例插件的参数：`-listen`（自身监听地址）、`-core-addr`（核心 BotService 地址）、
`-token`（与配置一致）、`-name`、`-greeting`，以及连接核心时使用的
`-tls-ca`/`-tls-cert`/`-tls-key`。它监听 `PluginService`，通过核心的
`BotService.SendMessage` 回复，因此自身不需要任何平台凭证。

`permissions` 决定插件能做什么：`send_message` 才能主动发送，`storage` 才能读写存储，
`network` 才会拿到 HTTP 客户端；`*` 表示全部。

## 可观测性

- `GET /metrics`（`metrics.addr`）暴露 Prometheus 文本指标：

  | 指标 | 标签 |
  | --- | --- |
  | `kei_events_published_total` | `platform` |
  | `kei_events_dropped_total` | `platform`, `reason` |
  | `kei_events_handled_total` | `plugin`, `rule`, `result` |
  | `kei_event_handling_seconds`（直方图） | `plugin`, `rule` |
  | `kei_messages_sent_total` | `platform`, `result` |
  | `kei_message_send_seconds`（直方图） | `platform` |
  | `kei_rules_matched_total` | `plugin`, `rule` |

- 日志用 `log/slog`，字段化输出（`plugin`/`rule`/`event_id`/`duration_ms`/`error`），
  `log.format: json` 可切换为 JSON。

## 测试与验证

```bash
nix develop --command go build ./...
nix develop --command go vet ./...
nix develop --command go test ./...
nix develop --command go test -race ./...
cd proto && nix develop --command buf lint        # proto 规范校验
```

测试覆盖：命令/正则/关键词/事件触发、优先级与中间件顺序、panic 恢复、超时、
事件去重、同会话保序、优雅关闭排空、发送限流与重试、能力降级、
Mock→EventBus→Router→Plugin→Reply 全链路集成、飞书回调（challenge/签名/AES 解密/ACK）、
OneBot 事件与 API、gRPC 外部插件（bufconn + localhost TCP + mTLS）。

## 已知限制

- 个人微信逆向协议不支持（`adapters/wecom` 尚未实现，企业微信可参考飞书适配器结构接入）。
- `BotAPI` 没有 `reply_to` 参数：插件回复当前会话无法携带引用；需要引用时用
  `Engine.SendRequest`（gRPC 通道已把 `reply_to` 透传到该入口）。
- 同一平台配置多个 bot 时，主动发送必须在 `bot.Target.BotID` 指定 bot 名称
  （gRPC 通道对应 `Target.bot_id`）；不指定则只有该平台仅有一个 bot 时才能自动选择。
- `Storage` 为内存实现，重启即丢失；生产可替换为 Redis/SQLite（实现 `bot.Storage` 即可）。
