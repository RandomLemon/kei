# Engine、EventBus 与路由中间件

覆盖 `internal/engine`（核心引擎、启动与关闭顺序、发送链路与限流重试）、`internal/eventbus`（分片事件总线）、`internal/router` + `internal/middleware`（规则路由与中间件链）。
不覆盖：`pkg/bot` 中 Event/Message/Rule/Target 等公开类型的逐字段说明（见 [domain-model.md](domain-model.md)）、适配器装配与权限裁剪的完整规则（见 [adapter.md](adapter.md)）、插件生命周期与插件上下文（见 [plugin.md](plugin.md)）、测试与构建要求（见 [testing.md](testing.md)）。
本文以仓库实现为准：以下接口、字段、默认值与错误信息均逐字对照 `internal/` 下源码，与早期设计稿不一致处以实现为准，已知缺口见文末附注。

---

## 7. Engine 核心引擎

在 `internal/engine` 中实现。

### 7.1 结构与构造

```go
type Engine struct {
	opts Options
	cfg  *config.Config
	log  *slog.Logger
	met  *metrics.Registry

	store      bot.Storage
	ownStorage bool
	httpClient *http.Client

	router  *router.Router
	bus     *eventbus.Bus
	plugins *pluginmgr.Manager
	sink    *eventSink

	mu           sync.RWMutex
	adapters     map[string]bot.Adapter
	platforms    map[string][]string
	adapterInfos []bot.AdapterInfo

	sendLimits  map[string]*ratelimit.Limiter
	handleLimit *ratelimit.Limiter
	ruleDedup   *dedup.Set

	runMu   sync.Mutex
	running bool
}
```

引擎通过 `AdapterBinding` 接收已装配的适配器，不感知平台名：

```go
type AdapterBinding struct {
	// BotID 是配置中的 bot 名称。
	BotID string
	// Adapter 是该 bot 的适配器实例。
	Adapter bot.Adapter
	// Metadata 是适配器的注册元信息，用于管理命令展示；可为零值。
	Metadata bot.AdapterMetadata
}
```

`engine.New(opts Options)` 只做静态校验与内部构造，不做任何 IO；适配器与插件的启动都发生在 `Run` 中。`New` 阶段校验：`Config` 非 nil、每个绑定的 `Adapter` 非 nil、`BotID` 非空、`BotID` 不重复（`engine: duplicate bot %q`）。`Run` 可重复调用性由 `runMu/running` 保护，重复调用返回 `engine: already running`。

`Options` 的可配置项与默认值（`engine.orInt`/`engine.orDuration` 把非法零值回落到默认）：

| 字段 | 含义 | 默认 |
| --- | --- | --- |
| `EventTimeout` | 单条事件整体处理超时 | 30s |
| `RuleTimeout` | 单条规则中插件中间件与 Handler 的执行超时（Timeout 中间件，框架自身判定在其之外，见 10.5） | 10s |
| `ShutdownTimeout` | 优雅关闭总超时 | 10s |
| `BusWorkers` | 事件总线分片 worker 数 | 4 |
| `BusQueueSize` | 每个分片队列容量 | 256 |
| `DedupTTL` | 事件去重时间窗 | 5m |
| `DedupCapacity` | 去重集合容量上限（总线按分片均分，规则去重集合独占） | 4096 |
| `SendRetries` | 发送失败重试次数（不含首次），负数按 0 | 0（不重试） |
| `SendBackoff` | 发送重试初始退避 | 100ms |
| `HTTPClient` | 插件 HTTP 客户端，nil 时使用 `Timeout: 15s` 的默认客户端 | 15s |
| `CommandPrefixes` | 命令前缀 | `["/"]` |

这些可调参数在唯一装配路径（`pkg/kei.Run`）上由配置 `engine` 段透传，配置键与 `Options` 字段一一对应（键名与取值规则见 [configuration.md](configuration.md) 12.6）：`command_prefixes`→`CommandPrefixes`、`event_timeout`→`EventTimeout`、`rule_timeout`→`RuleTimeout`、`shutdown_timeout`→`ShutdownTimeout`、`bus_workers`→`BusWorkers`、`bus_queue_size`→`BusQueueSize`、`dedup_ttl`→`DedupTTL`、`dedup_capacity`→`DedupCapacity`、`send_retries`→`SendRetries`、`send_backoff`→`SendBackoff`。配置未写（或时长/容量类写 0）时字段保持零值，由上表的 `orInt`/`orDuration` 回落默认；`SendRetries` 例外——`0` 是有效取值（不重试），不是「用默认」。配置层不替引擎填默认值。

插件生命周期各阶段超时由 `internal/pluginmgr` 决定，默认 Setup/Start/Stop 各 15s。

`Engine` 实现 `bot.BotAPI`、`bot.PluginCatalog`、`bot.AdapterCatalog` 三个接口，并提供 `Rules()`、`RegistrationErrors()`、`Bus()`、`Logger()`、`Storage()`、`Adapter(botID)`、`Adapters()`。`Adapters()` 返回深拷贝快照（`Platforms`/`Permissions`/`Options` 切片全部复制），调用方修改不影响引擎内部状态；`Rules()` 与 `RegistrationErrors()` 同样是隔离副本，引擎不暴露可写的路由器（注册与启动期调整详见第 10 章）。

插件拿到的不是引擎本体：`engine.pluginAPI` 按插件名与元信息包装出专属 `bot.BotAPI`，`Send` 需要 `PermSendMessage`（否则返回 `engine: plugin %s lacks permission %s`），`Reply` 所有插件可用，`Storage()` 在未声明 `PermStorage` 时返回 `storage.Denied()`，`Logger()` 带 `plugin` 字段。详见 [plugin.md](plugin.md)。

### 7.2 启动顺序

实际顺序（`pkg/kei.Run`：`buildConfig` → `adaptermgr.Build` → `engine.New` → `engine.Run`）：

1. `config.Load`/`config.LoadBytes` 加载配置；构造 `slog` 日志器并设为默认；`storage.NewMemory()`；`metrics.New()`；`cfg.Metrics.Addr` 非空时启动 `/metrics` HTTP 服务（`pkg/kei/assemble.go` 的 `serveMetrics`，同时注册 `/healthz`）。该服务由 `Run` 以 `defer` 同步关闭：无论装配成功还是中途失败，`Run` 返回时监听端口都已释放，重复 `Run` 可复用同一地址（`pkg/kei` 的 `TestRunReleasesMetricsPortOnEarlyFailure`、`TestServeMetricsStopReleasesPort`）。
2. `adaptermgr.Build(ctx, cfg, deps)`：校验适配器注册表与配置一致性，并按配置装配全部 bot 的适配器（适配器经 `bot.LookupAdapter` + `bot.AdapterContext`）。平台名分支只存在于适配器实现内部，`cmd/` 与 `internal/` 没有平台名分支。
3. 收集需要加载的插件（`selectPlugins`：注入实例在前，其后是配置启用且已注册的插件）。
4. `engine.New`：构造 Router（含全局中间件链）、构造并**启动事件总线 worker**、构造插件管理器，登记注册表中的插件。
5. `engine.Run(ctx)`：
   - `plugins.Setup(runCtx, e.router.Registrar)`：按注册顺序调用每个插件的 `Setup`，注入 `PluginContext`（含路由注册器、目录、适配器目录、按权限裁剪的依赖）。任一步失败或 panic 立即返回错误。
   - `e.applyBotPluginFilter()`：按每个 bot 的插件白名单裁剪规则适用范围（见 7.6）。
   - `plugins.Start(runCtx)`：按注册顺序启动插件；中途失败会逆序停止已启动的插件。
   - 为每个 `AdapterBinding` 启动一个独立 goroutine 调用 `Adapter.Start(runCtx, e.sink)`；适配器返回非 `context.Canceled` 错误时把 `engine: adapter <botID>: <err>` 写入带缓冲的 `adapterErr` 并 `cancel()`。
   - 之后阻塞在 `select`：`runCtx.Done()`（ctx 取消，例如 SIGINT/SIGTERM）或 `adapterErr`（适配器不可恢复错误）。适配器先写 `adapterErr` 再 `cancel()`，两个分支可能同时就绪而被 `select` 随机挑选；因此 `shutdown` 之后统一用非阻塞读收敛 `adapterErr` 中的全部残余错误（多个适配器失败时用 `errors.Join` 合并），保证适配器崩溃不会被静默吞成 `nil`。

### 7.3 关闭顺序与优雅退出

`Run` 返回前必定调用 `engine.shutdown`，其顺序固定：

1. 逐个调用 `Adapter.Stop(ctx)` 停止接收新事件，错误只记日志（`adapter stop failed`）。
2. `bus.Close(ctx)`：停止接收新事件并**消费完所有已入队事件**，等待全部分片 worker 退出。
3. 等待 7.2 中为每个适配器启动的 goroutine 全部退出（超时只记日志）。
4. `plugins.Stop(ctx)`：**逆序**停止已启动的插件。
5. 引擎自有的内存存储（`ownStorage`，即 `Options.Storage` 为 nil 时内部创建）关闭。
6. 记录 `engine stopped` 日志，携带总线的 `published`/`handled` 计数。

关闭阶段的所有错误都不阻止退出流程，只写 `Warn`/`Error` 日志；总超时由 `ShutdownTimeout`（默认 10s）约束。`Run` 的返回值是收敛后的适配器错误（多个失败用 `errors.Join` 合并），并同时以 `engine aborted by adapter failure` 记为 `Error` 日志；仅 ctx 取消且无任何适配器错误时才返回 nil。

优雅退出的触发点是 `cmd/bot/main.go` 的 `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`（门面本身只消费传入的 ctx）；取消后 `Run` 走上述关闭路径，因此已入队事件不会丢失（`internal/engine` 的 `TestGracefulShutdownDrainsQueuedEvents` 覆盖该行为）。

### 7.4 事件接收与处理

每个 Adapter 一个 goroutine：

```go
	for _, b := range e.opts.Adapters {
		wg.Add(1)
		go func(b AdapterBinding) {
			defer wg.Done()
			if err := b.Adapter.Start(runCtx, e.sink); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case adapterErr <- fmt.Errorf("engine: adapter %s: %w", b.BotID, err):
				default:
				}
				cancel()
			}
		}(b)
	}
```

适配器通过 `EventSink` 投递事件；`eventSink.Emit` 先把上报平台归一化为已装配适配器名，再调 `bus.Publish`，按返回值分别埋点——只有真正入队才记 `EventPublished`，去重命中的事件记 `EventDropped(platform, "duplicate")` 且仍返回 nil（重复不是错误），错误路径按 `queue_full`/`closed`/`error` 记 `EventDropped`：

```go
func (s *eventSink) Emit(ctx context.Context, ev *bot.Event) error {
	if ev == nil {
		return errors.New("engine: nil event")
	}
	platform := s.eng.metricPlatform(ev.Platform)
	enqueued, err := s.eng.bus.Publish(ctx, ev)
	if err != nil {
		reason := "error"
		switch {
		case errors.Is(err, eventbus.ErrQueueFull):
			reason = "queue_full"
		case errors.Is(err, eventbus.ErrClosed):
			reason = "closed"
		}
		s.eng.met.EventDropped(platform, reason)
		return err
	}
	if !enqueued {
		// 去重命中：事件未入队，但对外契约仍成功（重复不是错误）。
		s.eng.met.EventDropped(platform, "duplicate")
		return nil
	}
	s.eng.met.EventPublished(platform)
	return nil
}
```

`bus.Publish` 返回 `(enqueued, err)`：入队为 `(true, nil)`，ID 重复被忽略为 `(false, nil)`，队列满/已关闭/空事件为 `(false, 对应错误)`；`Bus.Emit` 为满足 `bot.EventSink` 只返回错误，需要区分「入队」与「去重忽略」的调用方直接用 `Publish`。

业务处理在事件总线的 worker 中进行。`Engine` 在 `New` 时以 `e.handle` 作为唯一订阅者注册到总线，`handle` 施加事件级超时并分发：

```go
func (e *Engine) handle(ctx context.Context, ev *bot.Event) {
	timeout := orDuration(e.opts.EventTimeout, defaultEventTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e.logIncomingMessage(ctx, ev)

	if err := e.router.Dispatch(ctx, ev); err != nil {
		e.log.Warn("event dispatch failed",
			"event_id", ev.ID,
			"platform", ev.Platform,
			"bot", ev.BotID,
			"error", err,
		)
	}
}
```

单条规则不向适配器错误通道上报：路由只把命中规则的全部错误用 `errors.Join` 聚合返回给 `handle` 记录日志，事件处理失败不会终止引擎。

### 7.4.1 消息收发的 INFO 日志

`internal/engine/logging.go` 在收发消息时向根日志器（`Engine.log`）写 INFO 日志，字段化输出消息内容与收发双方；可见性由 `log.level` 控制（默认 `info` 可见，设为 `warn`/`error` 即关闭），没有单独开关。

- 收到消息：`handle` 在分发前调用 `logIncomingMessage`，仅当 `ev.Message != nil` 时记录（非消息事件如心跳不产生日志），日志文案 `收到消息`，字段：`platform`/`bot`/`event_id`/`message_id`/`kind`/`user`/`user_name`/`channel`/`channel_name`/`content`。
- 发送消息：`SendRequest` 在适配器 `Send` 成功后调用 `logOutgoingMessage`，日志文案 `发送消息`，字段：`platform`/`bot`/`kind`/`channel`/`user`/`content`，另有 `message_id`（平台返回时）。记录的是降级后实际发送的 `req.Message`。此前的重试失败仍按原样记 Debug，最终失败通过返回的错误暴露。
- `content` 由 `describeMessage` 渲染：每个段为 `type:值`，段间以空格连接（文本/Markdown 输出原文，`at` 输出 `userID(显示名)`，`image`/`file` 输出 URL 或文件标识，`face` 输出表情 ID，`reply` 输出消息 ID，`card` 只输出类型名不展开载荷，未知类型输出 `<type>`）。该函数平台无关，适配器只需按 `Segment.Data` 键约定填充数据。

`internal/engine/logging_test.go` 覆盖渲染规则、收/发 INFO 日志与「非消息事件不记日志」。

### 7.5 会话保序

总线按 `bot.Event.SessionKey()`（格式 `platform:botID:channelID:userID`；`Channel` 为空时 `channelID` 回落为 `userID`，即私聊按用户保序）的 FNV-1a 32 位哈希取模分片数，把同一会话的事件固定投递到同一分片、由同一 goroutine 串行处理，因此同一会话严格保序，不同会话可并行（并行度上限 = `BusWorkers`）。哈希实现复用 `sync.Pool` 中的 `hash.Hash32` 与字节缓冲，缓冲区超过 512 字节不再放回池中，避免超长会话键造成内存滞留。`internal/engine` 的 `TestSessionOrderingIsPreserved` 验证同一会话 50 条事件按序处理。

### 7.6 规则适用范围与权限裁剪

`applyBotPluginFilter` 在插件 `Setup` 完成、适配器启动之前运行，仅当**启用实例**中存在非空 `bots[].plugins` 白名单时才生效：对每条规则计算允许它的 bot 名列表（该 bot 白名单为空视为允许全部）并排序写入 `Rule.BotIDs`；任何 bot 都不允许的规则得到空切片，从而匹配不到任何事件。`Rule.BotIDs` 为 nil 表示不做 bot 过滤。

被 `bots[].enabled: false` 停用的实例（`config.BotConfig.IsEnabled()` 为 false，见 [configuration.md](configuration.md)）被完全排除在收窄之外：既不参与「是否存在非空白名单」的判断，也不写入 `Rule.BotIDs`。理由是停用实例根本收不到事件（`internal/adaptermgr` 的 `Build` 也不会为它装配适配器），若把它计入白名单，停用唯一配置了白名单的实例会把 `Rule.BotIDs` 收窄成空列表、连带误伤其他实例。相关日志字段 `bots` 记录的是启用实例数，而非 `cfg.Bots` 长度。

启动校验与装配裁剪不在引擎内，而在 `internal/adaptermgr`（唯一执行点，见 [adapter.md](adapter.md)）：

- `bots[].adapter` 既未注册、也未在 `adapters:` 段声明（且该 bot 与适配器均未被 `enabled: false` 禁用）时报错，错误信息必须列出所有已注册适配器名与 `adapters` 段已声明名（空列表显示「无」）：

  ```text
  config: bots[3].adapter: 未知适配器 "myim"（已注册: mock, onebot；adapters 段已声明: 无）
  ```

- 注册表校验（`ValidateRegistry`）报错条件：注册名重复（`adaptermgr: 适配器注册名重复: %s`）、元信息缺 `Platforms`（`adaptermgr: 适配器 %s 未声明 Platforms`）、声明了仅插件可用的 `PermAll`；另有被拒绝的注册（见附注）。
- `AdapterMetadata.Permissions` 是装配裁剪的唯一依据：未声明 `PermStorage` 时注入 `storage.Denied()`，未声明 `PermNetwork` 时 `HTTPClient` 保持 nil；配置了保留键 `listen_addr`（`bot.OptListenAddr`）但未声明 `PermNetListen` 时装配失败：`config: bot %s 配置了保留键 %s，但适配器 %s 未声明 %s 权限`。
- 适配器不认识（未列入元信息 `Options`）的私有配置键只告警不报错。

### 7.7 发送链路：能力降级、令牌桶限流与退避重试

```go
// Send 实现 bot.BotAPI：把消息发送到目标会话。
func (e *Engine) Send(ctx context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error)
// SendRequest 发送一条完整的发送请求。
func (e *Engine) SendRequest(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error)
// Reply 实现 bot.BotAPI：回复事件所在会话。
func (e *Engine) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error)
```

`SendRequest` 的步骤：

1. 解析目标适配器（`resolve`）：`target.BotID` 非空时必须已注册且其平台名与 `target.Platform` 一致（`engine: bot %q is on platform %q, not %q`）；为空时仅当该平台只有一个 bot 才自动选择，否则报错要求显式指定（`engine: platform %q has %d bots, target.BotID is required`）；平台无 bot 时报 `engine: no adapter for platform %q`。
2. `bot.Degrade(req.Message, ad.Capabilities())` 按平台能力降级消息段；降级后没有可发送的段则直接报错，绝不静默发送空消息。
3. 发送前限流：配置了 `limits.send_rate > 0` 时为每个 bot 建一个令牌桶 `ratelimit.New(SendRate, SendBurst, 0, 0)`，发送前 `lim.Wait(ctx, botID)` 阻塞到拿到令牌或 ctx 结束（发送链路不允许丢消息）。
4. 重试：`attempts = SendRetries + 1`（未配置 `send_retries` 时为 1 次尝试，即不重试；配置段见 [configuration.md](configuration.md) 12.6）；第 n 次重试（n ≥ 1）前先等待 `SendBackoff << (attempt-1)`（默认 100ms、200ms），等待期间响应 ctx 取消。每次尝试都记 `MessageSent(platform, 耗时, err)` 指标。ctx 已取消或某次尝试返回错误且 `ctx.Err() != nil` 时停止重试；错误以 `engine: send via %s: %w` 包装返回。重试是 at-least-once 语义：对端已收到但响应失败时会重复投递。

令牌桶实现（`internal/ratelimit`）：`rate` 为每秒补充令牌数，`burst` 为桶容量（`<= 0` 时取 `max(1, rate)`），`rate <= 0` 表示不限流（`Allow` 恒为 true）。`Allow(key)`/`AllowN(key, n)` 放行时才扣令牌；`Buckets()`/`GC()` 暴露活跃桶数与空闲回收，空闲阈值默认 10 分钟。桶表容量有上限 `maxKeys`（`New` 的第 4 个参数，`<= 0` 时默认 10000）：活跃桶数达到上限后，插入新 key 时按「最久未使用」淘汰一个桶（`order` 为 `container/list` 维护的 LRU，淘汰 O(1)；任何访问——包括被拒绝的调用——都会刷新位置），被淘汰的 key 下次出现时重建为满桶。因此 key 空间不可枚举（例如伪造的用户 ID）时内存仍有界。`Wait` 在无法放行时按 `retryAfter` 计算需要等待的时长后重试，`retryAfter` 为 0 时睡 1ms 避免忙等；`retryAfter` 是只读估算——先按当前时间把令牌补到 `now` 再算 `max(0, (1-tokens)/rate)`，不修改桶状态，也不依赖调用方刚刚调用过 `Allow`（陈旧快照会把等待算长，例如桶已空了半个周期仍按整整一个令牌计）。

### 7.8 Handler 超时与 panic 隔离

三层保护：

1. 引擎层：`handle` 用 `EventTimeout`（默认 30s）约束整条事件的处理，规则、中间件与全部命中规则都在这条 ctx 下运行。
2. 链层：中间件 `Timeout(RuleTimeout)` 对单条规则的**插件中间件与 Handler** 施加独立超时；框架自身的策略判定（`Auth`/`Dedup`/`RateLimit`）在 `Timeout` 之外同步执行，见 10.5。`Recover` 在链中出现两次（见 10.5）。
3. 总线层：`eventbus` 的 worker 对每个处理器单独 `recover`，某个处理器 panic 不影响其它处理器，也不终止 worker；该事件计入 `Failed`，否则计入 `Handled`。

适配器与插件实现因此允许 panic 与阻塞，但必须响应 ctx 取消（超时通过 ctx 传递，Handler 需要感知取消才能真正释放）。`Timeout` 只放弃等待，不会终止内层 goroutine；不响应 ctx 的插件中间件或 Handler 会留下悬空 goroutine，直到自行返回。这类调用按 10.5 的清单被归因：规则超时那一刻仍未返回的调用计入 `kei_rule_overruns_total{plugin,rule}` 并记 Warn 日志（plugin/rule/event_id/timeout_ms）；它随后返回时另记一条含实际耗时的 Warn，超时后经过同样长的时间仍未返回时记一次含全部 goroutine 堆栈的 Error（每个 `Timeout` 实例至多转储一次），`EventTimeout`/引擎关闭导致的取消不计入。匹配阶段（`Rule.Matches`/插件断言）的 panic 不在上述保护范围内，由路由层按规则单独隔离，见 10.3。

### 7.9 管理命令 `/manage adapters`

`/manage adapters` 由内置 `plugins/manage` 插件注册，是「插件经 `PluginContext.Adapters` 读取装配结果」的最小示例：注册表部分来自 `bot.RegisteredAdapters()`（编译期注册表），绑定部分来自 `Engine.Adapters()`（引擎实现 `bot.AdapterCatalog`），因此两段可以不一致（注册表含未启用/未引用的适配器）。

输出格式、分支条件与实测样例见 [`plugins/manage.md`](plugins/manage.md) 3.4 节；`plugins/manage` 的七条子命令及其优先级、管理员门槛见同文档第 2 节。

---

## 8. EventBus 事件总线

在 `internal/eventbus` 中实现。

```go
func New(opts Options) *Bus
func (b *Bus) Publish(_ context.Context, ev *bot.Event) (enqueued bool, err error)
func (b *Bus) Emit(ctx context.Context, ev *bot.Event) error
func (b *Bus) Subscribe(handler func(context.Context, *bot.Event)) error
func (b *Bus) Close(ctx context.Context) error
func (b *Bus) Stats() Stats
```

仓库中不存在名为 `eventbus.EventBus` 的接口类型：`*eventbus.Bus` 是具体实现，对外暴露 `Publish`/`Subscribe`，并实现 `bot.EventSink`（`Emit`，供适配器投递事件）。`Publish` 以 `enqueued` 区分「入队」与「去重忽略」，`Emit` 受 `bot.EventSink` 契约限制只返回错误（入队与去重命中都返回 nil，需要区分时用 `Publish`）。`Engine` 持有具体类型 `*eventbus.Bus`。

### 8.1 配置与默认值

```go
type Options struct {
	Workers       int           // 分片 worker 数；<=0 → 4
	QueueSize     int           // 每分片队列容量；<=0 → 256
	DedupTTL      time.Duration // 去重 TTL；<=0 → 5m
	DedupCapacity int           // 去重集合总容量；<=0 → 4096，总线按 Workers 均分
	Logger        *slog.Logger  // nil → slog.Default()
}
```

零值 `Options` 合法，所有字段都会回落到默认值。`New` 一次启动 `Workers` 个 goroutine，每个分片一个带缓冲队列。

```go
type Stats struct {
	Published  uint64 // 成功入队的事件数
	Duplicated uint64 // 因重复 ID 被忽略的事件数
	Dropped    uint64 // 因队列已满被丢弃的事件数
	Handled    uint64 // 被处理器正常处理完毕的事件数
	Failed     uint64 // 处理过程中至少有一次处理器 panic 的事件数
}
```

### 8.2 Publish 语义

`Publish` 永不阻塞，也不响应 ctx 取消（`ctx` 仅为满足 `bot.EventSink` 契约）。顺序固定为：空事件报错 → 定位分片并在分片锁内检查关闭状态 → 去重命中则忽略 → 队列已满则丢弃 → 写入去重集 → 复制事件并入队。返回 `(enqueued, err)`：`enqueued` 表示事件是否真正入队，取值与错误组合如下。

**事件对象的所有权**：入队时总线复制 `*bot.Event` 并独占副本，队列、worker、处理器与路由器的命令解析都只接触副本，因此 `Publish` 返回后调用方可以立即复用 `ev`（放回 `sync.Pool`、覆写字段等）——旧实现把调用方的指针直接入队，`Router.Dispatch` 稍后写回的 `ev.Command` 会与这种复用方式竞态。复制是**浅拷贝**：`ev` 引用的 `Message`/`Sender`/`Channel` 等对象仍与副本共享，投递后不得再修改它们。复制只发生在确定入队的那条路径上，去重命中与队列满的调用不产生额外分配。

- 空事件返回 `(false, errNilEvent)`（`eventbus: nil event`）。
- 已关闭返回 `(false, ErrClosed)`（`eventbus: closed`）。
- 事件 ID 已出现过：`Duplicated` 加一，返回 `(false, nil)`（去重是「成功」而非错误，因此错误为 nil、但事件未入队）。
- 队列已满：`Dropped` 加一，写 `Warn` 日志，返回 `(false, 包装了 ErrQueueFull 的错误)`（`eventbus: publish %q: queue full`）；被丢弃的事件**不写入去重集**，因此调用方重试仍可成功投递。该日志在分片锁之外写、且**按分片限流**：连续打满时每个分片每个窗口（`dropLogInterval = 1s`）至多一条，字段含 `event_id`/`session_key`/`queue_size`/`dropped_total`（写日志当刻的累计丢弃数）；丢弃总数以 `Stats().Dropped` 为准。因此阻塞式日志处理器只会拖慢本次投递，不会占着分片锁。
- 成功入队：`Published` 加一，返回 `(true, nil)`。`enqueued` 为 true 只表示事件已入队，不表示已被处理；`enqueued=false` 且 `err=nil` 只可能是去重忽略。

容量检查与入队都在分片互斥锁内完成，因此不存在「检查通过后被并发写满、发送阻塞」的窗口，也不会向已关闭的 channel 发送。锁内只做内存操作（去重集与队列），日志与计数器都在锁外：去重集合按事件 ID 哈希拆成 `Workers` 份（见 8.5），因此不同 ID 的投递不会在同一把去重锁上串行，投递方之间只有分片锁这一处串行点。

### 8.3 Subscribe 语义

可以多次调用，同一事件会按注册顺序**串行**调用全部处理器。处理器列表以写时复制方式发布（读取路径无锁、无分配）。注册空处理器返回 `errNilHandler`（`eventbus: nil handler`）；已关闭时返回 `ErrClosed`。建议在首次 `Publish` 之前完成注册。

### 8.4 分片保序与 worker

每个分片是一个带缓冲队列 + 一个 worker goroutine：worker 按 FIFO 消费队列，队列关闭且取空后退出。分片下标由 `bot.Event.SessionKey()` 的 FNV-1a 32 位哈希取模得到（见 7.5），因此同一会话的事件严格串行、不同会话并行。

处理器调用使用总线内部固定的 `baseCtx`（`context.Background()`），**不随 `Close` 取消**：这让关闭时的排空语义成立。处理器 panic 由 `callHandler` 逐个捕获并记 `Error` 日志（含 `event_id`、`session_key`、`panic`），不影响同事件其它处理器或 worker。

### 8.5 去重

去重使用 `internal/dedup.Set`（事件总线与 Dedup 中间件共用的同一实现）：

- `New(capacity, ttl)`：`capacity <= 0` 时容量为 4096；`ttl <= 0` 时条目永不过期（此时容量淘汰是唯一的内存约束）。
- `Add(id)` 返回 true 表示首次出现、false 表示重复；空 ID 一律视为首次出现（无法去重）。
- `Seen(id)` 只查询不改变状态。
- 过期：`Add` 时若距上次 GC 已超过一个 TTL 则顺带清理过期条目；`GC()` 可显式清理。
- 容量：条目数超过容量时按插入顺序淘汰最旧的**活条目**，并压缩插入顺序切片防止无限增长。每个条目在 `order` 中只有一个有效槽位：ID 过期后被重加会分配新槽位，旧槽位在淘汰与压缩时被丢弃，因此陈旧槽位既不会误删重加后的条目，也不会累积。

总线的去重键是事件 ID（`ev.ID`），TTL 与容量来自 `Options.DedupTTL`/`DedupCapacity`（引擎默认 5m / 4096）。它拦截的是「同一条事件被重复投递」（适配器重发、平台重试），与中间件层按规则去重是两层独立机制（见 10.6）。

总线把去重集合按 `Options.Workers` 个分片拆开：**分片下标由 `ev.ID` 的 FNV-1a 哈希决定**，因此同一 ID 的投递（无论会话键是否相同）必然落在同一个集合上，去重语义与单集合一致，而不同 ID 的投递分散到不同集合、不再共用一把锁。每个集合的容量是 `max(1, DedupCapacity/Workers)`，总条目数仍受 `DedupCapacity` 约束；代价是单个集合的淘汰窗口比「全局 4096 条」的语义更短（TTL 仍是主要窗口）。

### 8.6 关闭

`Close(ctx)` 幂等（`sync.Once`）：

1. 置位关闭标志，在分片锁内关闭每个分片队列（此后 `Publish` 返回 `ErrClosed`）。
2. 后台等待全部 worker 退出，然后关闭 `done`。
3. 已排空则直接返回 nil（即使传入的 ctx 已过期——关闭已是既成事实）；否则等待 `done` 或 ctx 结束，ctx 超时返回 `ctx.Err()`。超时后后台仍会继续处理已入队事件，后续 `Close` 可以继续等待它们完成。

`Stats()` 在读路径上无锁，返回各计数器的快照（自总线创建以来的累计值）。

---

## 10. 路由与中间件

在 `internal/router` 和 `internal/middleware` 中实现。

### 10.1 Rule 结构

```go
type Rule struct {
	// ID 是规则唯一标识，未显式指定时由注册器生成。
	ID string
	// Priority 是优先级，数值越大越先执行。
	Priority int
	// Platforms 限定生效平台。
	//
	// nil 表示不做平台过滤；非 nil（含空切片）表示白名单，此时只有对应
	// Platform 的事件才能命中，空切片即任何平台都命中不了。
	Platforms []string
	// BotIDs 限定生效的机器人（bot 名称）。
	//
	// nil 表示不做机器人过滤；非 nil（含空切片）表示白名单，此时只有对应
	// BotID 的事件才能命中，空切片即任何机器人都命中不了。引擎用它实现
	// 「每个 bot 只启用部分插件」。
	BotIDs []string
	// EventType 限定事件类型，空表示全部类型。
	EventType EventType
	// Kind 限定会话类型，空表示全部类型。
	Kind MessageKind
	// Command 限定命令名（不含前缀，比较时忽略大小写）。
	Command string
	// Regex 限定正则，对消息纯文本做匹配，取值时使用 FindStringSubmatch。
	Regex *regexp.Regexp
	// Keywords 限定关键词，命中任意一个（忽略大小写的子串匹配）即触发。
	Keywords []string
	// Match 是自定义断言，可用于权限等复杂判断。
	Match func(*Event) bool
	// AdminOnly 表示该规则只允许管理员触发，需要 Auth 中间件配合。
	AdminOnly bool
	// Handler 是命中后的处理函数。
	Handler Handler
	// Plugin 是规则所属插件名，由注册器填充。
	Plugin string
	// Middlewares 是该插件通过 Registrar.Use 声明的中间件。
	Middlewares []Middleware
}
```

`Rule.Matches(e)` 的过滤条件是「与」关系，未设置的过滤条件不参与判断，判定顺序：`EventType` → `Kind`（要求 `e.Message` 非 nil 且 `Message.Kind` 相等）→ `Platforms`（**非 nil** 时白名单，`nil` 表示不过滤）→ `BotIDs`（**非 nil** 时白名单，`nil` 表示不过滤）→ `Command`（要求 `e.Command` 非 nil 且命令名忽略大小写相等）→ `Regex`（对 `e.Text()` 做匹配）→ `Keywords`（任意关键词忽略大小写子串命中）→ `Match`（自定义断言）。`AdminOnly` 不参与匹配，由 Auth 中间件在链上校验。

### 10.2 Option 全集

`pkg/bot` 暴露 8 个 `Option` 构造器，均为 `func(*Rule)`，按 `opts` 顺序在注册器默认值之后应用；清单、签名与逐项语义见 [plugin.md](plugin.md) 9.2，本节只讨论路由器如何使用这些字段。

### 10.3 匹配与优先级语义

- 注册器（`Registrar`）提供 5 个注册入口 + 1 个中间件入口：`OnCommand`、`OnRegex`、`OnKeyword`、`OnEvent`、`OnAll`、`Use`；各入口预置的规则字段与失败记录通道见 [plugin.md](plugin.md) 9.2 的入口表。`OnAll` 不设置任何过滤条件，因此所有事件都会命中。
- 规则 ID 未显式指定时按 `plugin:kind:index` 生成（`kind` ∈ `command`/`regex`/`keyword`/`event`/`all`，由规则字段推断），序号按「插件 + 类别」独立递增；正则编译失败不占用序号也不注册规则。
- 规则表按 `Priority` **降序**执行，同优先级按注册顺序（`slices.SortStableFunc`）。同一次事件命中的所有规则都会被完整执行，各自的错误用 `errors.Join` 聚合并包装为 `router: 规则 %s 执行失败: %w`；没有任何规则命中时 `Dispatch` 返回 nil。
- `Dispatch` 在 `ev.Command == nil && ev.Message != nil` 时用配置的前缀（默认 `["/"]`）解析命令并写回 `ev`（`bot.ParseCommand`）。
- 每条命中规则注入独立的 `bot.Route`（`Plugin`、`RuleID`、`Trigger`、`Priority`、`AdminOnly`、`RegexMatches`），并创建独立的 `bot.Reply`（`internal/reply.Builder`，每次规则调用一次，`Send`/`SendResult` 时才真正发送，并实现可选扩展 `bot.ExtendedReply`；事件消息带 ID 时自动追加 `SegReply` 引用原消息（平台不支持引用时该段被降级丢弃，见 [plugin.md](plugin.md) 9.3）；工厂缺失时退化为 `bot.NoopReply`），避免多条规则的回复互相污染。
- 规则表以「不可变快照 + 原子替换」发布：`Dispatch` 可被多个总线 worker 并发调用，注册与 `UpdateRules` 由写锁串行化。`UpdateRules(fn)` 在写锁内把 `fn` 依次应用到每条规则的**副本**上，全部校验通过后按优先级重建快照并整体替换（引擎用它写入 `BotIDs`）；中途失败（`fn` 为 nil 或把某条规则的 `Handler` 置空）时已应用的改动一并丢弃，既不发布新快照也不改到已发布的规则，因此不存在半更新状态。`Rules()` 返回深层副本，`RegistrationErrors()` 返回错误副本（无法通过返回值上报的注册错误，例如正则编译失败、重复 ID）。引擎只经 `Engine.Rules()`/`Engine.RegistrationErrors()` 暴露只读视图，不暴露可写的 `Router`（两者都是仓库内视图：`engine.Engine` 为 `internal` 类型，仓库外不可达；注册失败对外的信号是 `recordErr` 的 Warn 日志，见 [plugin.md](plugin.md) 9.2）。
- 匹配阶段的 panic 按规则隔离：`Rule.Matches`（插件的 `WithMatch` 断言、事件文本解析）在 `Dispatch` 内部逐条 recover，panic 只跳过该条规则——其余规则照常匹配与执行，panic 记 `Error` 日志（`plugin`/`rule`/`platform`/`type`/`event_id`/`panic`/`stack`）并计入 `RuleMatchPanic`（`kei_rule_match_panics_total{plugin,rule}`），错误以 `router: 规则匹配 panic: 规则 <id>: <panic>` 形式（包装 `ErrMatchPanic`，可用 `errors.Is` 判定）进入 `Dispatch` 的聚合返回值。修复前该类 panic 会冒泡出 `Dispatch`，被事件总线的「处理器 panic」兜底捕获并中止该事件的所有剩余规则，且日志里没有 rule 标识。

### 10.4 中间件洋葱模型

`Handler` 与 `Middleware` 的类型定义及 `Registrar` 语义见 [plugin.md](plugin.md) 9.2。

`internal/router` 在规则注册时把「全局中间件 → 插件 `Use` 中间件 → Handler」折叠成单个 Handler 并固化进快照（`fold`/`wrap`）：每组内**先注册的在更外层**（即先注册的先执行）。因此一条事件的实际执行顺序由外到内是：全局中间件（见 10.5）→ 插件中间件（`Registrar.Use`，只影响调用 `Use` 之后为该插件注册的规则）→ 插件 Handler。

`internal/middleware/chain.go` 提供组合器：

```go
// Chain 把多个中间件按洋葱模型组合为单个中间件。
// mws[0] 位于最外层：它最先执行、最后返回；mws 为空时返回透传中间件。
func Chain(mws ...bot.Middleware) bot.Middleware
// Apply 把中间件链包裹在 handler 外层，返回可直接注册的处理函数。
func Apply(h bot.Handler, mws ...bot.Middleware) bot.Handler
```

`Chain` 复制入参切片（调用方后续修改不影响已构造的链），nil 中间件按透传处理。引擎自身的链由 `router.fold` 折叠，`Chain`/`Apply` 供需要自定义链的调用方与测试使用。

### 10.5 全局中间件清单与顺序

`internal/middleware/middleware.go` 实现 7 个中间件，全部为 `bot.Middleware`，可用 `errors.Is` 判定拦截原因（`ErrPanic`、`ErrTimeout`、`ErrDuplicate`、`ErrRateLimited`、`ErrUnauthorized`）：

| 中间件 | 签名 | 作用 |
| --- | --- | --- |
| `Recover` | `Recover(log *slog.Logger) bot.Middleware` | 捕获 handler panic（含非 error 值），记 `Error` 日志（plugin/rule/platform/event_id/panic/stack）并返回包装 `ErrPanic` 的错误 |
| `Logger` | `Logger(log *slog.Logger) bot.Middleware` | 记录 plugin、rule、platform、kind、user、channel、event_id、duration_ms；成功记 Debug，`ErrDuplicate`/`ErrRateLimited`/`ErrUnauthorized` 记 Warn 并带 `decision=denied`，其余（含 `ErrTimeout`/`ErrPanic`）记 Error |
| `Metrics` | `Metrics(rec metrics.Recorder) bot.Middleware` | 进入链时（规则已命中）调 `rec.RuleMatched(plugin, rule)`，`next` 返回后再调 `rec.EventHandled(plugin, rule, 耗时, err)`；`rec` 为 nil 时透传，错误原样透传 |
| `Timeout` | `Timeout(d time.Duration, log *slog.Logger, rec metrics.Recorder) bot.Middleware` | 用 `d <= 0` 透传；在独立 goroutine 中执行 next（结果写入容量 1 的 channel），超时或 ctx 取消返回包装 `ErrTimeout` 的错误；超时时刻仍未返回的调用上报 `rec.RuleOverrun` 并记 Warn，超时后经过 `d` 仍未返回时另记一次含全部 goroutine 堆栈的 Error（见 7.8） |
| `Auth` | `Auth(isAdmin func(*bot.Event) bool) bot.Middleware` | 仅当 `Route.AdminOnly` 为 true 时生效：`isAdmin` 为 nil 或返回 false 时返回 `ErrUnauthorized` 且不调用 next |
| `RateLimit` | `RateLimit(l *ratelimit.Limiter, scope string) bot.Middleware` | 按作用域取限流键并 `l.Allow(key)`，超配额返回 `ErrRateLimited`（错误含 `scope` 与 `key`）；`l` 为 nil 时透传 |
| `Dedup` | `Dedup(set *dedup.Set) bot.Middleware` | 见 10.6；`set` 为 nil 时透传 |

限流作用域常量：`ScopeUser = "user"`（按发送者 ID）、`ScopeChannel = "channel"`（会话 ID）、`ScopePlugin = "plugin"`（插件名，缺失时为 `DefaultLabel = "unknown"`）；`RateLimit` 的 `default` 分支（含 `ScopeUser`）按发送者 ID 取键。取到的键为空时统一回退 `SessionKey()`（`platform:botID:channelID:userID`）：无 `Sender` 的事件（如 OneBot 的 notice/meta）因此按会话/机器人分桶，而不是全部挤进同一个空键桶；连 `SessionKey()` 也为空（事件为 nil）时透传——没有身份可限流，不把这类事件塞进同一个桶。

引擎装配的全局链（`Engine.globalMiddlewares`，数组顺序即由外到内的执行顺序）：

```go
	return []bot.Middleware{
		middleware.Recover(e.log),
		middleware.Logger(e.log),
		middleware.Metrics(e.met),
		middleware.Auth(e.isAdmin),
		middleware.Dedup(e.ruleDedup),
		middleware.RateLimit(e.handleLimit, middleware.ScopeUser),
		middleware.Timeout(orDuration(e.opts.RuleTimeout, defaultRuleTimeout), e.log, e.met),
		middleware.Recover(e.log),
	}
```

链的顺序有三条契约：

1. **策略判定在超时之外**：`Auth`/`Dedup`/`RateLimit` 都在 `Timeout` 之外，在调用者 goroutine 中同步完成；`Timeout` 只包裹第三方代码（插件 `Use` 中间件与 Handler）。因此框架自身的判定不会被超时窗口放弃，`RuleTimeout` 的语义是「插件代码的执行上限」。
2. **去重先于限流**：`Dedup` 在 `RateLimit` 之外，重投的事件在 `l.Allow` 之前就被丢弃，重复投递不消耗 `limits.handler_rate` 配额。
3. **去重标记先于超时**：`Dedup` 的写入发生在启动超时 goroutine 之前（见 10.6 的「标记开始」语义），不存在「超时后内层 goroutine 尚未记录」的窗口。

`Recover` 出现两次是有意为之：`Timeout` 会把内层链放进新的 goroutine 执行，外层 `Recover` 只能保护本 goroutine 上的中间件（Logger/Metrics/Auth/Dedup/RateLimit/Timeout），插件中间件与 Handler 的 panic 必须由 `Timeout` 之内的那个 `Recover` 兜住，否则会带崩整个进程。

`Metrics` 的两个计数语义不同，需分别解读：

- `RuleMatched`（`kei_rules_matched_total{plugin,rule}`）记录**规则命中**：`Metrics` 位于该规则专属的处理链上，进入它就意味着路由器已判定该规则匹配当前事件。它在 `next` 之前上报，因此只统计命中次数，与规则后续是否被拒绝无关。
- `EventHandled`（`kei_events_handled_total{plugin,rule,result}` 与耗时直方图 `kei_event_handling_seconds`）记录**处理结果**：`next` 返回后按 `err` 取 `result` 标签（非 nil 为 `error`，否则 `ok`）。

两者不相等：`Metrics` 在全局链中位于 `Timeout`/`Auth`/`RateLimit`/`Dedup` 之外（见上表顺序），所以被去重或限流拦下的调用同样计入 `RuleMatched`，而只在 `EventHandled` 上体现为 `result="error"`。据此可从两者差值读出「命中但被拒绝」的比例。

`internal/metrics` 以自研的 Prometheus 文本格式实现（`Registry` 实现 `metrics.Recorder`，`Handler()` 暴露为 `/metrics` 可用的 `http.Handler`），全部计数器族为 `kei_events_published_total{platform}`、`kei_events_dropped_total{platform,reason}`、`kei_events_handled_total{plugin,rule,result}`、`kei_event_handling_seconds{plugin,rule}`（直方图）、`kei_messages_sent_total{platform,result}`、`kei_message_send_seconds{platform}`（直方图）、`kei_rules_matched_total{plugin,rule}`、`kei_rule_overruns_total{plugin,rule}`、`kei_rule_match_panics_total{plugin,rule}`。全部 `Recorder` 方法对 nil 接收者安全，因此未启用指标的部署可传 `(*metrics.Registry)(nil)` 作为空实现。

事件投递的计数口径：`kei_events_published_total` 只统计**实际成功入队**的事件，`kei_events_dropped_total` 统计未入队的投递尝试（`reason="duplicate"` 为去重命中，另有 `queue_full`/`closed`/`error`），因此 `published + dropped` 等于投递尝试总数；去重命中不是错误，`EventSink.Emit` 仍返回 nil。`kei_events_dropped_total{platform,reason}` 的 `reason="duplicate"` 与 `(*eventbus.Bus).Stats().Duplicated` 计数一致。

`platform` 标签会归一化为**已装配适配器的平台名**（`Adapter.Name()`）：事件上报的平台名不在已装配集合内时统一记为 `unknown`，避免未经校验的上报值（平台回调甚至可指定平台名）为每个新取值新增一条时间序列、导致指标注册表内存无界增长。`kei_messages_sent_total`/`kei_message_send_seconds` 的 `platform` 取自解析后的适配器名，本身就是已知平台，无需归一化。

`kei_rule_overruns_total{plugin,rule}` 记录**规则超时那一刻仍未返回**的调用数：父 ctx 结束（引擎关闭、`EventTimeout` 到期）导致的取消不计入——那不是规则超时；超时后很快退出的良性情况也计入（对应日志里的实际耗时可用于分辨），持续不退出者另有 Error 级堆栈告警（见 7.8）。

`kei_rule_match_panics_total{plugin,rule}` 记录**规则匹配阶段 panic** 的次数：每次 panic 只跳过该条规则、不影响同一事件的其余规则（见 10.3），该计数用于发现反复抛 panic 的插件断言（`WithMatch`）。

引擎的处理侧限流：`limits.handler_rate > 0` 时建 `ratelimit.New(HandlerRate, HandlerBurst, 0, 0)` 并以 `ScopeUser` 作用于全部规则（即按发送者限流，发送者为空的事件回退 `SessionKey`，见上表）；`limits.handler_rate <= 0` 时该中间件透传（不限流）。该中间件位于 `Dedup` 之内，重投的重复事件不消耗配额（见 10.5）。中间件支持的其余作用域需调用方自行用 `Chain` 组合。

`isAdmin` 判定：`ev` 或 `ev.Sender` 为 nil 返回 false，否则与 `auth.admin_users` 逐一比对 `ev.Sender.ID`。

### 10.6 Dedup 中间件与 EventBus 去重的关系

两层去重互不替代：

- **总线层（`eventbus`）**：在事件入队前按 `ev.ID` 去重（TTL 默认 5m、容量默认 4096），拦截适配器/平台重复投递的同一事件；被拦截的事件连分片队列都不进。
- **中间件层（`middleware.Dedup` + 引擎的 `ruleDedup` 集合）**：在规则执行前按「规则 × 事件」去重。因为全局链是**按规则**包裹执行的，同一条事件可能命中多条规则、每条规则都会走一遍链，所以去重键必须带规则标识，否则第二条命中规则会被误判为重复。键的构造：有路由信息时前缀 `route.RuleID + "|"`；基础键优先取 `e.ID`，为空时退化为 `SessionKey + "|" + 消息 ID`（无消息时只用 `SessionKey`）；基础键为空（拿不到任何标识）时放行不做去重。命中重复返回 `ErrDuplicate`。

**去重是「标记开始」而非「标记成功」**：`Dedup` 命中新键即写入 `ruleDedup`，随后无论该次执行成功、报错、超时还是 panic 都不回滚，因此 TTL 内同一「规则 × 事件」的重投一律丢弃。理由是去重要拦住的正是「适配器重发、平台重试」：若等成功后才记录，第一次执行尚未结束时到达的重复投递会被并发处理，去重随之失去意义；这也与总线层在 `Publish` 时立即写入的语义一致。代价是超时或失败的尝试同样占用槽位，其重投在 TTL 内被丢弃——失败本身由 `ErrTimeout`/`ErrPanic` 的 `Error` 日志与 `kei_events_handled_total{result="error"}` 上报，不是静默丢失。为保证标记在超时窗口开始之前同步写入（不依赖可能被 `Timeout` 放弃的内层 goroutine），`Dedup` 位于 `Timeout` 之外，见 10.5。

因此：同一条事件命中多条规则不会互相拦截（规则前缀隔离），而总线 TTL 窗口之外再次出现的事件 ID 也不会被总线拦截、但仍受 `ruleDedup` 的 TTL 约束（同一集合，默认 TTL 5m、容量 4096）。

## 附：已知缺口与实现边界

以下为当前实现的真实边界与缺口，均为正文已描述事实之外的补充；其余实现细节（Rule 字段全集、Option 全集、分片 worker 模型、`RateLimit` 作用域、指标已实现）见正文第 7 至第 10 章，此处不重复。

- **被拒绝的适配器注册是启动期致命错误**：`bot.RegisterAdapter`（`pkg/bot/adapter_registry.go`）在 `meta.Name == ""` 或 `factory == nil` 时不入表，而是把该次注册记入 `bot.AdapterRegistrationErrors()`（保留注册顺序与拒绝原因）；`adaptermgr.ValidateRegistry` 在校验进程内注册表后调用 `validateRegistrationRejects`，非空时报错终止启动：`adaptermgr: 适配器注册被拒绝: %s；RegisterAdapter 要求 Name 非空且 factory 非 nil`，其中 `%s` 为逐条 `名称（原因）`，空名显示为 `<空名>`。因此空 `Name` 既不会让适配器静默可用，也不会表现为运行时「未注册」，而是在进程启动阶段失败。同名重复注册不走这条路径，由 `validateRegistryOf` 单独报错，见 7.6。
- **无 `eventbus.EventBus` 接口**：`internal/eventbus` 只导出具体类型 `*eventbus.Bus`（`Engine` 亦持有具体类型）；其完整方法集为 `Publish(ctx, ev) (bool, error)`、`Emit(ctx, ev) error`（实现 `bot.EventSink`）、`Subscribe(handler) error`、`Close(ctx) error`、`Stats() Stats`。`Publish` 不阻塞：分片队列满时丢弃事件并返回 `(false, eventbus: publish %q: queue full)`（`errors.Is(err, ErrQueueFull)`），已关闭时返回 `(false, ErrClosed)`，去重命中返回 `(false, nil)`。
- **`Timeout` 无法终止不响应 ctx 的调用**：Go 不能强杀 goroutine，规则超时只是放弃等待——不响应取消的插件中间件或 Handler 会继续运行、继续占用该 goroutine，直到自行返回。框架的兜底是契约（`pkg/bot` 的 `Handler` 注释要求响应 ctx 取消）与归因（`kei_rule_overruns_total` 计数、Warn 日志、卡死时的 goroutine 堆栈 Error，见 7.8、10.5），而不是强制退出。

## 相关文档

- [domain-model.md](domain-model.md)：`pkg/bot` 的公开领域模型（Event/SessionKey、Message、Rule、Handler/Reply）。
- [adapter.md](adapter.md)：适配器接口与注册表、`AdapterContext`、元信息与权限裁剪、能力降级。
- [plugin.md](plugin.md)：插件生命周期、`PluginContext`、`BotAPI` 门面与插件中间件。
- [testing.md](testing.md)：单元/集成测试与 `-race` 要求、构建与运行方式。
