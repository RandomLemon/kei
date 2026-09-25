# kei gRPC 通道（外部插件与外部适配器）

覆盖 `proto/` 下的唯一进程外契约（`plugin.proto`、`adapter.proto`、buf 配置与生成代码）、核心侧的实现位置（`internal/grpcsrv`、`internal/pluginmgr/external`、`internal/adaptermgr/external`）、协议层的硬性要求与已提供的示例进程。不覆盖进程内插件/适配器的接口与装配（见 `plugin.md`、`adapter.md`），也不覆盖配置键的完整清单（见 `configuration.md`）与安装/快速开始（见 `../README.md`）。

## 12. 外部插件与外部适配器 gRPC 协议

外部插件与外部适配器共用同一套通道与信任模型：核心只做 `BotService` 的 Server，并作为客户端 dial 外部进程；外部进程实现自己的服务并反向调用核心的 `BotService`。

### 12.1 拓扑

```text
外部插件进程：实现 PluginService（核心 dial 调用）→ 反向调用核心 BotService.SendMessage
核心进程：    实现 BotService（Server，监听 grpc.addr）+ 作为客户端 dial 外部进程
外部适配器进程：实现 AdapterService（核心 dial 调用）→ 反向调用核心 BotService.EmitEvent

数据流：平台事件 → 适配器 → EmitEvent / EventSink.Emit → EventBus → Engine → Reply
        → BotService.SendMessage（外部插件）或 AdapterService.Send（适配器）→ 平台
```

双角色说明（`internal/grpcsrv` 与两个 `external` 包互为协议两端）：

- 核心侧服务端：`internal/grpcsrv` 实现 `plugin.BotService`，监听 `grpc.addr`，供外部插件与外部适配器反向调用。四个 RPC 在核心侧统一做令牌认证与入参校验，`SendMessage`/`EmitEvent` 另按令牌类别与权限进一步授权。
- 核心侧客户端：`internal/pluginmgr/external` dial 插件进程的 `PluginService`（投递事件、初始化、关闭）；`internal/adaptermgr/external` dial 适配器进程的 `AdapterService`（初始化、按 bot 启停、发送、关闭）。
- 监听地址来源：插件为 `plugins.<name>.grpc_addr`，适配器为 `adapters.<name>.grpc_addr`；核心地址由核心在 `Init` 中下发给对端（`reply_service_addr` / `core_addr`），配置 `grpc.addr` 是固定端口。
- 时序约束：核心的 `BotService` 在适配器装配之后才开始监听，因此适配器在 `Start` 之后才能反向调用核心（`EmitEvent`/`GetConfig`/`Log`），`Init` 阶段只能保存 `core_addr`。核心侧 `Init` 带 `WaitForReady`，插件侧对核心的连接在核心起来后自动变 Ready，因此启动顺序无关（推荐先起外部进程再起核心）。

统一约定：

1. 事件投递：核心 dial 外部插件并调用 `PluginService.HandleEvent`；适配器方向相反，事件由平台推给适配器，适配器再经 `EmitEvent` 送进核心。
2. 事件上行：外部适配器经 `BotService.EmitEvent` 投递（对应进程内适配器的 `EventSink.Emit`）。
3. 发送：外部插件经 `BotService.SendMessage` 回复；核心经 `AdapterService.Send` 让适配器发送。
4. 认证：`token` 必填且与核心配置一致，可选 mTLS（`grpc` 段的 `cert_file`/`key_file`/`ca_file`）。
5. 崩溃隔离：外部进程崩溃或断连只影响其自身绑定的插件/bot，核心记录日志（外部适配器断连与重连另有指标），绝不退出。
6. 语言无关：`proto/` 是唯一契约，外部插件与适配器可用 Python、Node.js、Rust 实现。

### 12.2 proto

`proto/plugin.proto`（插件协议 + `BotService`）与 `proto/adapter.proto`（适配器协议）同属 `package plugin`、共用 `go_package = github.com/RandomLemon/kei/proto/pluginpb`；`adapter.proto` 通过 `import "plugin.proto"` 复用 `Event`、`Message`、`Target`、`Empty`、`SendResponse` 等定义，避免同一业务结构出现两份契约。字段命名与 `pkg/bot` 的 Go 类型一一对应，JSON 载荷统一用「JSON 字符串」承载（业务结构体可独立演进）。

gRPC 方法全名形如 `/plugin.PluginService/Init`、`/plugin.BotService/SendMessage`、`/plugin.AdapterService/Send`。

#### plugin.proto

```proto
// PluginService 由外部插件实现，核心调用。
service PluginService {
  // Init 初始化插件并下发生效配置与令牌。
  rpc Init(InitRequest) returns (InitResponse);
  // HandleEvent 把事件投递给插件处理。
  rpc HandleEvent(Event) returns (HandleResult);
  // Shutdown 通知插件退出，插件需释放资源并尽快返回。
  rpc Shutdown(Empty) returns (Empty);
}

// BotService 由核心实现，外部插件与外部适配器都作为客户端调用。
service BotService {
  // SendMessage 以插件身份发送消息，需要 send_message 权限。
  rpc SendMessage(SendRequest) returns (SendResponse);
  // GetConfig 读取调用方自身的配置（适配器读取其进程级配置）。
  rpc GetConfig(GetConfigRequest) returns (GetConfigResponse);
  // Log 以调用方身份写入核心结构化日志。
  rpc Log(LogRequest) returns (Empty);
  // EmitEvent 供外部适配器投递平台事件，需要 receive_event 权限；
  // 返回 ok 只表示事件已入队，不表示已被处理。
  rpc EmitEvent(EmitEventRequest) returns (EmitEventResponse);
}
```

`InitRequest`（核心 → 插件，每个字段都是「核心交给插件」的信息）：

| 字段 | 类型 | 语义 |
| --- | --- | --- |
| `name` | string | 核心为该插件配置的名称，插件应以它作为自身标识 |
| `version` | string | 核心记录的插件版本，仅用于日志与展示 |
| `author` | string | 核心记录的插件作者，仅用于展示 |
| `description` | string | 核心记录的插件说明，仅用于展示 |
| `permissions` | repeated string | 核心授予该插件的权限列表 |
| `config_json` | string | 核心下发的该插件配置，JSON object 字符串；无配置时为空串 |
| `token` | string | 核心分配给该插件的认证令牌，后续 `BotService` 调用都需携带 |
| `reply_service_addr` | string | 核心 `BotService` 的监听地址，插件据此反向调用核心 |

`InitResponse`：`ok`（是否初始化成功）、`error`（失败原因，`ok` 为 true 时为空）。插件以 `ok=false` 表达业务拒绝，核心据此区分「插件拒绝初始化」与「传输失败」，两种情况都视为启动失败。

`Event`（核心 → 插件，对应 `bot.Event`）：

| 字段 | 类型 | 语义 |
| --- | --- | --- |
| `id` | string | 事件唯一标识，用于去重 |
| `type` | string | 事件分类：`message`/`notice`/`request`/`meta` |
| `platform` | string | 产生事件的平台名，与适配器名一致 |
| `bot_id` | string | 接收该事件的机器人标识 |
| `time_unix_ms` | int64 | 事件发生时间，Unix 毫秒（UTC） |
| `message` | Message | 消息内容，仅在 `type` 为 `message` 时存在 |
| `sender` | User | 消息发送者，可能缺省 |
| `channel` | Channel | 消息所在会话，私聊时可能缺省 |
| `command` | Command | 解析后的命令，未触发命令时缺省 |
| `raw_json` | string | 平台原始事件，JSON 字符串，仅用于调试 |

`Message`：`id`（消息 ID，发送前可为空）、`kind`（会话类型 `private`/`group`/`channel`）、`segments`（有序消息段）。
`Segment`：`type`（`text`/`image`/`at`/`face`/`reply`/`markdown`/`card`/`file`）、`data_json`（片段数据，JSON object 字符串，键名见 `pkg/bot` 的 `Key*` 常量）。
`User`：`id`、`name`、`is_bot`。
`Channel`：`id`、`name`、`kind`（`private`/`group`/`channel`）。
`Command`：`name`（不含前缀）、`args`（保持原样不拆分）、`raw`（原始文本）。

`HandleResult`：`handled`（插件是否消费了该事件）、`error`（处理失败原因）。`handled=false` 表示插件主动放弃，属于正常结果。

`SendRequest`（插件 → 核心）：`token`（`Init` 阶段获得的令牌，必填）、`target`（`Target`）、`message`（`Message`）、`reply_to`（非空表示引用回复该消息 ID）。

`Target`（对应 `bot.Target`）：`platform`、`channel_id`（群/频道 ID，私聊可空）、`user_id`（群聊可空）、`kind`（`private`/`group`/`channel`）、`bot_id`（核心配置中的 bot 名称；为空时由核心按平台自动选择，同平台多个 bot 时必须显式指定）。核心要求 `channel_id` 与 `user_id` 至少有一个，否则 `InvalidArgument`。

`SendResponse`：`message_id`（平台返回的消息 ID）、`error`（发送失败原因）。核心的失败语义：认证/权限/参数错误以 gRPC status 返回；平台发送失败写入 `error`，因为这是业务失败而非 RPC 失败。

`GetConfigRequest`：`token`（必填）、`key`（配置键，支持 `"a.b.c"` 多级路径；为空表示读取整份配置）。
`GetConfigResponse`：`found`（配置或键是否存在）、`value_json`（值的 JSON 字符串，`found=false` 时为空）。外部适配器的进程级配置同样以适配器名为键从这里读取。

`LogRequest`：`token`（必填）、`level`（`debug`/`info`/`warn`/`error`，未知取值按 `info` 处理）、`message`（日志正文）、`fields_json`（结构化字段，JSON object 字符串）。核心按令牌类别绑定 `plugin` 或 `adapter` 字段；`fields_json` 解析失败时降级为原始字符串字段，不报错。

`EmitEventRequest`（外部适配器 → 核心）：`token`（`Init` 阶段获得的令牌，必填）、`event`（`Event`；为空视为参数非法）。适配器必须先 ACK 平台，再调用本 RPC。

`EmitEventResponse`：`ok` **只表示事件是否已入队，不表示已被处理或已被插件消费**；`error` 是入队失败原因。核心侧入队失败（事件总线入口报错）走 `error` 而非 status error；RPC 自身被取消/超时仍按传输失败上报。

`Empty`：无字段的占位消息。

#### adapter.proto

```proto
// AdapterService 由外部适配器实现，核心 dial 后调用。
service AdapterService {
  // Init 初始化适配器并下发生效配置与令牌，每个进程只调用一次。
  rpc Init(AdapterInitRequest) returns (AdapterInitResponse);
  // Start 启动一个 bot 实例的事件接收，返回该实例的能力声明；
  // 必须已就绪才返回，保证返回后该实例即可产生事件。
  rpc Start(AdapterInstance) returns (AdapterStartResponse);
  // Stop 停止一个 bot 实例，释放该实例独占的资源，必须幂等。
  rpc Stop(AdapterStopRequest) returns (Empty);
  // Send 向平台发送消息，失败原因写在 SendResponse.error 中。
  rpc Send(AdapterSendRequest) returns (SendResponse);
  // Shutdown 通知适配器退出，适配器需停止全部实例、释放资源并尽快返回。
  rpc Shutdown(Empty) returns (Empty);
}
```

`AdapterInfo`（身份、能力声明与配置键名单，双向使用）：

| 字段 | 类型 | 语义 |
| --- | --- | --- |
| `name` | string | 适配器名，也是配置中 `bots[].adapter` 的取值 |
| `version` | string | 适配器版本，仅用于日志与展示 |
| `author` | string | 适配器作者，仅用于展示 |
| `description` | string | 适配器说明，仅用于展示 |
| `platforms` | repeated string | 适配器支持的平台名列表 |
| `permissions` | repeated string | 核心授予该适配器的权限列表，例如 `receive_event` |
| `options` | repeated string | 适配器接受的配置键名单；核心据此对未知配置键告警 |

`AdapterInfo` 的权威性方向：核心下发的 `AdapterInfo` 只填 `name`、`platforms`（仅当配置里显式指定了 `adapters.<name>.platform` 时有 1 个值）与 `permissions`（核心授予值）；`version`/`author`/`description`/`options` 下发时为空。适配器在 `AdapterInitResponse.info` 中回传自身的 `version`/`author`/`description`/`platforms`/`options`，核心以回传值覆盖配置中记录的值，实际生效权限 = 适配器声明 ∩ 核心授予。

`AdapterInitRequest`：`info`（`AdapterInfo`）、`token`（核心分配给该适配器的令牌，后续 `BotService` 调用都需携带）、`core_addr`（核心 `BotService` 监听地址）、`config_json`（进程级配置，JSON object 字符串；无配置时为空串）。一个适配器进程只 `Init` 一次，随后按绑定的 bot 实例逐个 `Start`。

`AdapterInitResponse`：`ok`、`error`、`info`（适配器回传的实际元信息）。校验规则（`internal/adaptermgr/external`）：`info` 缺失、`name` 非空且与配置名不一致、`platforms` 为空、声明 `*` 权限（仅插件可用）都导致启动失败。

`AdapterInstance`：`bot_id`（配置中的 `bots[].name`）、`platform`（写入 `Event.Platform` 的平台名）、`config_json`（该实例的适配器私有配置）。适配器必须按 `bot_id` 隔离实例状态。

`AdapterStartResponse`：`ok`、`error`、`capabilities`（**该实例能力的唯一权威来源**，核心据此执行消息段降级）、`platform`（该实例实际使用的平台名，优先于 `AdapterInstance.platform`）。核心在 `platform` 非空且与本实例平台名不一致时判定启动失败。

`AdapterStopRequest`：`bot_id`（要停止的实例标识；`Stop` 必须幂等）。

`Capabilities`（对应 `bot.Capabilities`）：`text`、`markdown`、`image`、`at`、`card`、`file`、`reply`、`private`、`group`。全零声明无法表达「什么都不支持」：核心按「仅文本」处理（`text=true`）并记 warn，避免把文本段也降级成占位符；降级规则见 `adapter.md` 与 `domain-model.md`。

`AdapterSendRequest`（核心 → 适配器，对应 `bot.SendRequest`）：`bot_id`、`target`（`Target`）、`message`（`Message`）、`reply_to`。

#### 代码生成

- 包约定：两个 `.proto` 同属 `package plugin`；`option go_package = "github.com/RandomLemon/kei/proto/pluginpb"`。`adapter.proto` 以 `import "plugin.proto"` 复用消息，因此不重复定义 `Event`/`Message`/`Target`/`Empty`/`SendResponse`。
- 生成位置：`proto/pluginpb/`，共四个文件 `plugin.pb.go`、`plugin_grpc.pb.go`、`adapter.pb.go`、`adapter_grpc.pb.go`（由 `protoc-gen-go` / `protoc-gen-go-grpc` 生成，提交入库）。
- `proto/buf.gen.yaml`：`version: v2`，两个 `local` 插件（`protoc-gen-go`、`protoc-gen-go-grpc`），`out: pluginpb`，`opt: paths=source_relative`。
- `proto/buf.yaml`：`version: v2`，`modules: [{path: .}]`；lint 用 `STANDARD`，显式豁免 `PACKAGE_DIRECTORY_MATCH`、`PACKAGE_VERSION_SUFFIX`、`RPC_REQUEST_STANDARD_NAME`、`RPC_RESPONSE_STANDARD_NAME`、`RPC_REQUEST_RESPONSE_UNIQUE`（契约要求包名为 `plugin`、文件位于 `proto/` 根、RPC 使用业务化类型名如 `HandleEvent(Event) returns HandleResult`，外部插件按该签名实现，不能为满足 lint 风格规则而改名）；breaking 用 `FILE`。
- 命令：生成 `cd proto && buf generate`；规范校验 `buf lint`。工具链（`protoc`、`protoc-gen-go`、`protoc-gen-go-grpc`、`buf`）由 flake 提供。

### 12.3 要求

1. 核心侧实现：`BotService` 的 gRPC 服务端在 `internal/grpcsrv`，由插件与外部适配器共用；两条客户端通道互为镜像，外部插件在 `internal/pluginmgr/external`，外部适配器在 `internal/adaptermgr/external`。装配与配置解析在 `cmd/bot/external.go`（令牌登记 `bindToken`、外部适配器名单 `externalAdapterNames`、外部插件描述 `externalSpecs`）。
2. 一个外部适配器进程可服务多个 bot：`Init` 一次，`Start` 每个绑定的 bot 一次，`Stop` 幂等；实例状态必须按 `bot_id` 隔离。核心侧由 `adaptermgr/external.Client` 的 `instances map[string]*instance` 与每个 `instance` 自持的状态实现；同一 `bot_id` 重复 `Instance()` 报错，已启动实例再次 `Start` 由核心侧状态短路，`Stop` 按协议必须在对端幂等。
3. 事件上行只经 `BotService.EmitEvent`：适配器必须先 ACK 平台，再调用它；`ok` 只表示已入队。核心侧 `grpcsrv.EmitEvent` 只把事件交给 `Options.EmitEvent` 钩子并按返回值决定 `ok`/`error`；适配器通道的 `instance.Start` 不接触 `EventSink`。
4. 授权：`EmitEvent` 需要 `receive_event` 权限，且令牌类别必须是适配器（插件令牌即使带该权限也被拒绝）；`GetConfig`/`Log` 与插件同一规则（只需有效令牌）。未知或空 token 返回 `Unauthenticated`，权限不足返回 `PermissionDenied`，事件不得落到事件总线。核心未接入事件入口时返回 `FailedPrecondition`。入队阶段还会校验归属：适配器只能投递其绑定的 bot 的事件（`adaptermgr.Bindings.EmitFunc`）。
5. 能力协商：`Start` 返回的 `Capabilities` 是该实例的唯一权威声明；全零按「仅文本」处理并告警，核心据此执行降级（规则见 [docs/adapter.md](adapter.md) 的能力与降级一节）。核心在 `Start` 之前对该实例返回零值能力。
6. 平台名协商：平台名以 `AdapterInitResponse.info.platforms` 为准；上报多个平台时必须在 `adapters.<name>.platform` 指定，否则启动失败；指定的平台不在上报列表中同样失败。实例上报的 `platform` 与之一致才接受（为空时沿用实例已有平台名）。
7. 崩溃隔离与重连：外部适配器断连后该 bot 不再产生事件（事件只能经对端连接上行），核心按指数退避重连；重连期间 `Send` 因 RPC 失败而返回错误，重连成功会重新 `Init` 并重启此前已启动的实例。重连参数：巡检间隔 500ms、初始退避 100ms、退避上限 5s、连续失败上限 6 次；达到上限把该适配器绑定的实例标记为停用（`Send` 立即返回「已停用（重连失败）」）并转入 30s 低速巡检，核心进程与其他 bot 不受影响。整个过程核心同时记录日志与指标：重连成功与失败计入 `kei_adapter_reconnects_total{adapter,result}`（`result` 为 `ok`/`error`），实例被停用计入 `kei_adapter_disabled_total{adapter}`。外部插件断连不影响核心：事件投递失败向上返回错误，由引擎中间件决定记日志或重试。
8. 生命周期对齐：核心关闭按「停止接收 -> 等待处理完成 -> `Stop` 各实例 -> `Shutdown`」顺序收尾，且必须带超时。实现：引擎 shutdown 先 `Adapter.Stop` 各绑定（外部实例即 `AdapterService.Stop`，共用引擎的 `ShutdownTimeout`，默认 10s），再排空事件总线、等适配器 goroutine 退出，最后逆序 `Stop` 插件（外部插件即 `PluginService.Shutdown`，超时 5s）；引擎返回后先 `external.close()`（关闭插件连接并以 3s 超时 `grpcsrv.Server.Stop` 停止 `BotService`），再 `Bindings.Close()` 对适配器通道兜底 `Stop` 各实例（已停止者为 no-op）+ `AdapterService.Shutdown`（单次 RPC 超时，默认 10s）+ 关闭连接。`Shutdown` 通知即使传入 ctx 已取消也会尽力发出（「让对端释放资源」优先于尊重取消）。
9. MVP 先做进程内适配器注册表，外部 gRPC 适配器作为阶段 8（对照外部插件的阶段 6）；外部插件协议的最小实现仍是阶段 6。

#### 核心侧实现要点

- `grpcsrv.Server`：`New(Options)` 校验 `Addr`/`Bot` 与令牌类别（只能是 `plugin`/`adapter`），复制 `Tokens`/`Configs`；`Start` 非阻塞（返回后 `Addr()` 可用），`Stop` 幂等且 ctx 超时后强制停止。`Options.TLS` 非 nil 时启用 TLS，mTLS 由调用方在 `tls.Config` 里配 `ClientAuth`。
- 令牌身份：`grpcsrv.TokenInfo{Name, Kind, Permissions}`；同一 token 重复登记、插件与适配器重名、空 token 都在启动阶段报错（`bindToken`）。插件配置的 `permissions` 缺省为 `send_message`，外部适配器的 `permissions` 缺省为 `receive_event`。
- 外部插件通道：`external.New` 连接并同步 `Init`（`WaitForReady`，超时默认 10s，可区分「未就绪」与「拒绝初始化」），`Setup` 用 `OnAll` 注册兜底处理器（事件过滤由插件进程负责），单次 `HandleEvent` 超时默认 10s，`Stop` 通知 `Shutdown` 后关闭连接，`Close` 只回收连接。核心侧元信息固定为 `version=external`、`author=config`、`description=gRPC 外部插件`。
- 外部适配器通道：`external.Client` 负责 `Init` 校验、平台解析、权限交集、`Metadata()`/`Platform()`/`Instance()`/`Close()`；`instance` 实现 `bot.Adapter`（`Name` 取连接期解析的平台名、`Capabilities` 取 `Start` 应答、`Send` 组装 `AdapterSendRequest`）。实例级保留键 `listen_addr` 需要适配器声明 `net_listen`，否则装配失败（未授予 `network` 不阻止实例创建，因为进程内权限裁剪不适用于外部进程）。指标经依赖注入：`adaptermgr.Deps` 与 `adaptermgr/external.Deps` 都有 `Recorder metrics.Recorder` 字段，由 `cmd/bot` 传入指标注册表；`Recorder` 为 nil 时回退为 `(*metrics.Registry)(nil)` 空实现（所有方法对 nil 接收者安全），因此未启用指标的部署零成本关闭上报。
- TLS：`grpc.cert_file`/`key_file`/`ca_file` 既用于核心服务端（给了 `ca_file` 即 mTLS，要求客户端证书），也作为核心 dial 外部插件与外部适配器的客户端凭据；外部进程自身的监听是否需要 TLS 由其自行决定（示例插件与示例适配器的监听均为明文，插件连接核心时可带 `-tls-*`）。
- JSON 载荷约定：`nil` 映射编码为空串（「没有配置」），非 nil 空映射编码为 `"{}"`（「配置为空对象」）；事件 `raw_json` 与段 `data_json` 同理，因此往返后可区分。

### 12.4 示例进程

仓库提供两个 Go 示例，演示协议的最小完整形态；启动核心、注入事件与查看回复的具体命令行见 `../README.md`。

- `cmd/example-plugin`：实现 `PluginService`（`plugin.go` + `main.go`）。先 `net.Listen` 并 `Serve` 自己的 `PluginService`，再等待核心 `BotService` 就绪（`waitCore`，超时 10s），避免两侧互相等待。`Init` 校验令牌（不匹配返回 `ok=false` 而非 RPC 错误），`HandleEvent` 只处理纯文本消息事件、命中 `/ext` 前缀或 `external` 关键词时经 `BotService.SendMessage` 回复，发送失败写入 `HandleResult.Error` 且保持 `handled=true`。参数：`-listen`、`-core-addr`、`-token`（三者必填），`-name`（缺省 `example-plugin`）、`-greeting`（缺省 `你好，我是外部插件`），连接核心可选 `-tls-ca`/`-tls-cert`/`-tls-key`（三项必须同时提供）。每次反向调用在请求体写令牌，并额外附加元数据 `x-plugin-token`（核心以请求体为准校验，元数据供拦截器/网关审计）。
- `cmd/example-adapter`：实现 `AdapterService`（`main.go`）。`Init` 只保存 `core_addr`/令牌/进程级配置并构造惰性连接（此时核心尚未监听），`Start`/`Stop` 按 `bot_id` 维护实例（重复调用幂等），`Send` 记录发送请求并返回消息 ID（未知实例按业务失败上报），`Shutdown` 停控制面并释放连接。内置 HTTP 控制面用于演示与测试：`POST /inject`（`{"bot_id","text"}`）把文本包装成事件经 `EmitEvent` 投递，`GET /sent` 返回记录到的发送请求。`Start` 上报的能力为 `text/image/at/reply/private/group`，回传的平台名优先取核心下发的平台。参数：`-listen`（缺省 `127.0.0.1:19071`）、`-platform`（缺省 `example`）、`-control`（缺省 `127.0.0.1:19081`）。

## 相关文档

- [docs/architecture.md](architecture.md) — 总体架构、启动/关闭顺序与目录结构
- [docs/adapter.md](adapter.md) — 适配器接口、注册表、能力与降级
- [docs/plugin.md](plugin.md) — 插件接口、注册表与插件上下文
- [../README.md](../README.md) — 启动示例进程与注入事件的命令
