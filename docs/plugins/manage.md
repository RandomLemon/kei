# 内置管理插件 manage

`plugins/manage` 是仓库内的内置管理插件：注册 `/ping`、`/version`、`/plugins`、`/adapters`、`/admin` 五条命令，同时是「插件目录（`PluginContext.Catalog`/`Adapters`）+ 管理员规则（`WithAdmin` + Auth 中间件）」的参考实现。

本文是 `plugins/manage` 的**命令契约与行为说明**（逐条对照 `plugins/manage/manage.go`、`internal/router`、`internal/engine`、`internal/middleware` 的实现，并以实际运行输出为证）。第 9 章插件契约与第 11 章 BotAPI/PluginContext 见 [`../plugin.md`](../plugin.md)，`/plugins`、`/adapters` 的对比与 Auth 中间件见 [`../engine.md`](../engine.md)，配置键全表见 [`../configuration.md`](../configuration.md)。`plugins/echo` 的最简示例见 `plugin.md` 9.4。

---

## 1. 定位与启用

- 包路径 `github.com/RandomLemon/kei/plugins/manage`，源码 `plugins/manage/manage.go`，在 `init()` 中调用 `bot.RegisterPlugin(&Plugin{})`（`manage.go:169`）；命令实现与包同构、无第三方依赖。
- 空导入：`cmd/bot/main.go:27` 的 `_ "github.com/RandomLemon/kei/plugins/manage"`；第三方可仿此在 import 块加一行。
- 启用：配置中必须显式启用（`plugins.<name>` 未列出即不启用，见 [`../configuration.md`](../configuration.md) 12.3）：

```yaml
plugins:
  manage:
    enabled: true
    plugins_admin_only: true   # 可选，见第 4 节
```

- 元信息（`manage.go:27`）：

| 字段 | 值 |
| --- | --- |
| `Name` | `manage` |
| `Version` | `v0.1.0` |
| `Author` | `core` |
| `Description` | `管理命令：/ping、/version、/plugins、/adapters、/admin` |
| `Permissions` | 未声明（空） |

**权限声明为空的原因与后果**：五条命令全部只调用 `bot.Reply`（对任何插件始终可用），因此不需要 `PermSendMessage`；也不读 `Storage`、不发外部请求，因此不需要 `PermStorage`/`PermNetwork`。代价是插件不能主动发送（`BotAPI.Send` 返回 `engine: plugin manage lacks permission send_message`）、`PluginContext.Storage` 是拒绝式实现（`../plugin.md` 11.4）。`/plugins`、`/adapters` 读取 `PluginContext.Catalog`/`Adapters` **不需要**声明权限——目录是装配期注入的只读视图，与 `PermAdmin` 无关。

`Plugin` 只有两个实例字段（`manage.go:18`）：`cfg *bot.Config`（插件私有配置）与 `start time.Time`（`Setup` 时刻），无包级可变状态。

## 2. 命令契约

`Setup` 注册的五条规则（`manage.go:45`–`69`）：

| 命令 | 规则 ID | 优先级 | 管理员门槛 | 回复内容 |
| --- | --- | --- | --- | --- |
| `/ping` | `manage:ping` | 100 | 无 | `pong · 已运行 <Duration>` |
| `/version` | `manage:version` | 100 | 无 | `kei v0.1.0 · manage v0.1.0` |
| `/plugins` | `manage:plugins` | 100 | 由 `plugins_admin_only` 决定（默认无） | 插件目录多行文本 |
| `/adapters` | `manage:adapters` | 100 | 无 | 适配器注册表 + 绑定关系多行文本 |
| `/admin` | `manage:admin` | 100 | **始终**（`bot.WithAdmin()`） | `管理员校验通过` |

要点：

- 全部使用 `bot.WithPriority(100)` 与 `bot.WithID("manage:<cmd>")`：优先级 100 让管理命令先于兜底规则（例如 `WithPriority(-100)` 的 fallback）执行，规则 ID 用于日志与指标定位（`../plugin.md` 9.2）。
- 注册名不含前缀；默认前缀为 `/`（`internal/engine/engine.go:73`、`:150`），可用引擎选项 `CommandPrefixes` 覆盖。命令比较忽略大小写（`Rule.Command`），因此 `/PING` 同样命中，但前缀必须匹配。
- `WithAdmin()` 只设置 `Rule.AdminOnly`；真正的拦截者是全局中间件 `middleware.Auth`（第 5 节）。因此不存在「插件内部自己判管理员」的逻辑。
- 五条命令都是同步的纯文本回复，无 goroutine、无后台任务：`Start`/`Stop` 是空实现（`manage.go:75`、`:78`）。

## 3. 输出格式

以下格式串与分支顺序均取自 `describePlugins`（`manage.go:81`）、`describeAdapters`（`manage.go:116`）与 `joinPermissions`（`manage.go:161`）；示例为在 `mock` 适配器上实际注入命令后 `GET /sent` 得到的真实输出（`/ping` 的运行时长随进程不同）。

### 3.1 `/ping`

```
pong · 已运行 6.602s
```

`p.start` 在 `Setup` 时记录（`manage.go:43`），`uptime = time.Since(p.start).Truncate(time.Millisecond)`（`manage.go:46`），因此显示的是**插件 Setup 以来**的时长，不是进程启动时间，精度毫秒（`Duration.String()` 的自然格式，例如 `1m23.456s`）。

### 3.2 `/version`

```
kei v0.1.0 · manage v0.1.0
```

格式为 `"kei v" + bot.Version + " · " + meta.Name + " " + meta.Version`（`manage.go:52`）。`bot.Version` 是框架版本常量 `"0.1.0"`（`pkg/bot/version.go:4`），`manage v0.1.0` 来自本插件的 `Metadata()`。

### 3.3 `/plugins`

三个分支：

| 条件 | 输出 |
| --- | --- |
| `PluginContext.Catalog == nil` | `插件目录不可用` |
| 目录存在但没有已加载插件 | `没有已加载的插件` |
| 其它 | `已加载 N 个插件：` + 每插件一行 |

每个插件按名字**升序**排序（`sort.Slice`），行格式为 `- <Name>[ <Version>][ by <Author>][ [<perm1,perm2>]][ — <Description>]`，可选段在值非空时才出现，权限用英文逗号连接。

真实输出（`echo` + `manage` 均启用时，注意 `manage` 自己也在列表里）：

```
已加载 2 个插件：
- echo v0.1.0 by core — 回显命令参数：/echo <文本>
- manage v0.1.0 by core — 管理命令：/ping、/version、/plugins、/adapters、/admin
```

未声明权限的插件（如 `echo`、`manage`）不会出现 `[...]` 段；声明了权限的插件显示权限常量原文（例如 `[network,net_listen]`）。

### 3.4 `/adapters`

输出固定分两段：**编译期注册表**（`bot.RegisteredAdapters()`）与**实际绑定实例**（`PluginContext.Adapters`，即引擎 `Engine.Adapters()`）。

第一段：

| 条件 | 输出 |
| --- | --- |
| 注册表为空 | `没有已注册的适配器` |
| 否则 | `已注册 N 个适配器：` + 每个适配器一行（按名字升序） |

适配器行格式为 `- <Name>[ <Version>][ (<platforms，逗号连接>)][ [<permissions>]][ — <Description>]`。

第二段：

| 条件 | 输出 |
| --- | --- |
| `PluginContext.Adapters == nil` | 追加 `绑定信息不可用` |
| 没有绑定实例 | 追加 `没有已绑定的适配器实例` |
| 否则 | 追加 `已绑定 N 个实例：` + 每实例一行 `- <BotID> → <适配器注册名>`（按 `BotID` 升序） |

真实输出（`cmd/bot` 空导入了 `mock`/`feishu`/`onebot`，但配置里只装配了 `mock-main`）：

```
已注册 3 个适配器：
- feishu v0.1.0 (feishu) [network,net_listen] — 飞书（Lark）适配器：事件经开放平台事件订阅回调进入，发送经开放平台 IM 接口发出。
- mock v0.1.0 (mock) [net_listen] — Mock 适配器：不连接真实平台，事件经 Inject 或 HTTP 控制面手动注入，发送结果记录在内存中，用于本地联调与测试。
- onebot v0.3.0 (onebot) [network,net_listen] — OneBot v11 适配器：按 mode 选择 HTTP 双向 / 正向 WebSocket / 反向 WebSocket，事件入站与发送通道随接入方式切换。
已绑定 1 个实例：
- mock-main → mock
```

两段天然可以不一致，这是设计意图而非缺陷：

- 注册表来自 `pkg/bot` 的编译期注册表（`pkg/bot/adapter_registry.go:152`），列出二进制内**所有**已注册适配器，包括未在 `bots[]` 中引用、或经 `adapters.<name>.enabled: false`/`bots[].enabled: false` 停用的适配器。
- 绑定列表来自引擎装配结果，只含**实际装配成功**的实例；被禁用的 bot/适配器不出现（`../configuration.md` 12.2、12.4）。

### 3.5 `/admin`

```
管理员校验通过
```

固定文本，只用于演示 Auth 中间件链路；规则带 `bot.WithAdmin()`，命中但身份不符时不回复（第 5 节）。

## 4. 配置键

| 键 | 类型 | 默认 | 语义 |
| --- | --- | --- | --- |
| `plugins.manage.enabled` | bool | `false`（未列出即不启用） | 是否装配该插件；可用布尔标量简写 `manage: true` |
| `plugins.manage.plugins_admin_only` | bool | `false` | 为真时给 `/plugins` 规则追加 `bot.WithAdmin()`（`manage.go:55`–`57` 读取 `p.cfg.Bool("plugins_admin_only", false)`） |

- `enabled` 的解码与「未列出即不启用」语义由配置层统一处理（见 [`../configuration.md`](../configuration.md) 12.3），插件私有配置经 `PluginContext.Config` 读取。本节只记录 manage 自己的键。
- 环境变量覆盖与 `plugins.<name>` 段的通用规则一致（只命中配置中**已存在**的插件名，见 `../configuration.md` 12.5）：`KEI_PLUGINS_MANAGE_ENABLED=false` 停用插件；`KEI_PLUGINS_MANAGE_PLUGINS_ADMIN_ONLY=true` 等效于 YAML 里写 `plugins_admin_only: true`。
- 示例配置 `configs/config.yaml` 中 manage 是启用的，并把 `plugins_admin_only` 设为 `true`（见第 8 节第 3 条注意事项）。

## 5. 管理员校验链路

`bot.WithAdmin()` 只是声明，实际拦截在全局中间件链：

1. 注册期：`WithAdmin()` 置 `Rule.AdminOnly = true`（`pkg/bot/registrar.go:139`）。
2. 匹配期：路由器为命中的规则构造 `bot.Route`，把 `Rule.AdminOnly` 抄进 `Route.AdminOnly` 并写入 ctx（`internal/router/router.go:212`）。
3. 执行期：全局链中的 `middleware.Auth(e.isAdmin)`（`internal/engine/engine.go:263`，位于 Timeout 之后的第二个 Recover 与 RateLimit 之间）只在 `Route.AdminOnly` 为真时检查（`internal/middleware/middleware.go:246`）。
4. 判定：`Engine.isAdmin` 用 `Event.Sender.ID` 与 `auth.admin_users` 逐一**精确比较**；`ev` 或 `ev.Sender` 为 nil 返回 false（`internal/engine/engine.go:270`）。
5. 拒绝：返回 `middleware: unauthorized: plugin=manage rule=manage:admin`（包 `ErrUnauthorized`，`internal/middleware/middleware.go:43`），**不调用 Handler**，因此不会发送任何消息；日志侧中间件记 warn `中间件: 事件被拒绝`（字段 `plugin`/`rule`/`platform`/`kind`/`user`/`event_id`/`duration_ms`/`error`/`decision=denied`），路由器再记 warn `event dispatch failed`（`router: 规则 manage:admin 执行失败: ...`）。

实际运行的拒绝日志（配置 `plugins_admin_only: true`，非管理员访问 `/plugins`）：

```
level=WARN msg="中间件: 事件被拒绝" plugin=manage rule=manage:plugins platform=mock kind=group user=someone-else channel=mock-group event_id=b1 duration_ms=0.166 error="middleware: unauthorized: plugin=manage rule=manage:plugins" decision=denied
level=WARN msg="event dispatch failed" event_id=b1 platform=mock bot=mock-main error="router: 规则 manage:plugins 执行失败: middleware: unauthorized: plugin=manage rule=manage:plugins"
```

前置条件：Auth 是引擎全局链的固定成员，插件无需安装；只有绕过引擎、直接用 `internal/router` 且不挂 Auth 的嵌入/测试组合才不会被拦截。`auth.admin_users` 为空时，所有带管理员门槛的规则对**任何**事件都拒绝（第 8 节第 3 条）。

## 6. 与 `PluginContext` 的关系

`Setup` 开头取上下文，取不到直接报错（`manage.go:37`–`41`）：

```go
pc, ok := bot.PluginContextFrom(ctx)
if !ok {
    return fmt.Errorf("manage: missing plugin context")
}
p.cfg = pc.Config
p.start = time.Now()
```

- 失败即 `Setup` 失败，`internal/pluginmgr` 会终止本次启动（插件 panic/阶段失败只终止启动，不拖垮进程）。
- `PluginContext` 只注入生命周期方法（`Setup`/`Start`/`Stop`）的 ctx，Handler 执行期的 ctx 只带 `Route`（`../plugin.md` 11.3）。因此 `pc.Config` 被保存到实例字段，而 `pc.Catalog`/`pc.Adapters` 由 Handler 闭包直接引用——命令执行时读的是**调用时刻**的目录快照，不是 Setup 时刻的快照。
- `Catalog`/`Adapters` 为 nil 只出现在手动装配的场景：测试手工构造 `PluginContext`，或其他未接线 `pluginmgr.Deps.Catalog`/`AdapterCatalog` 的装配路径。引擎装配一定接线（`internal/engine/engine.go:230`–`240`，`Catalog` 来自插件管理器、`AdapterCatalog` 来自 `Engine.Adapters()`），所以运行期见到的通常是「目录不可用」以外的分支。

## 7. 测试与手工验证

单元测试 `plugins/manage/manage_test.go` 共 5 个，逐包统计与覆盖描述见 [`../testing.md`](../testing.md) 的「实际测试分布」表（`plugins/manage` 行），此处不再重复。测试手法即 `../plugin.md` 11.5 的插件测试辅助：`bot.NewRecordingRegistrar()` 记录规则、手工构造 `PluginContext`（含 `Catalog`/`Adapters` 假实现）注入 ctx，再直接调用 `rule.Handler`。

手工验证（`mock` 适配器，无需真实平台）：用只含一个 mock bot 的配置启动 `./cmd/bot`，注入后读 `GET /sent`：

```bash
curl -sS -XPOST 127.0.0.1:18080/inject -H 'content-type: application/json' -d '{"text":"/plugins","id":"ev-1"}'
curl -sS 127.0.0.1:18080/sent | jq -r '.[].Request.Message.Segments[0].Data.text'
```

本文第 3 节的示例输出即由此得到；管理员门槛可用 `user_id` 字段区分身份（`{"text":"/admin","user_id":"admin-user"}` 在 `auth.admin_users: ["admin-user"]` 下得到回复，其它 user_id 只得到第 5 节的两条 warn 日志）。

## 8. 已知边界与注意事项

1. **`/ping` 的时长口径**是插件 `Setup` 以来的时长（`manage.go:43`），不是进程启动时间；引擎重启或进程重启才重置。
2. **`/plugins` 会列出 `manage` 自身**（引擎的插件目录包含所有已加载插件），且因未声明 `Permissions` 而不显示 `[...]` 段。
3. **示例配置当前会挡住 `/plugins`**：`configs/config.yaml` 把 `plugins.manage.plugins_admin_only` 设为 `true`（`:92`）而 `auth.admin_users` 为空（`:27`），此时 `/plugins`、`/admin` 对任何事件都返回未授权（只打 warn 日志、不回复）。要使用管理命令需先在 `auth.admin_users` 中配置管理员 ID（或把 `plugins_admin_only` 置 false）。
4. **两段适配器列表可以不一致**（第 3.4 节）：注册表含编译进二进制但未启用/未引用的适配器，绑定列表只含实际装配的实例。
5. **输出是单个 `SegText` 段**（`Reply.Text`），不含 Markdown/卡片；能否送达取决于平台能力，降级链见 [`../adapter.md`](../adapter.md)。
6. **`/adapters` 不显示适配器实例的私有配置**，只显示注册元信息与 `bot 名 → 适配器名`；实例私有键与保留键语义见 `../configuration.md` 12.2、12.4。
7. `/plugins`、`/adapters` 的输出长度随插件/适配器数量线性增长，没有分页或截断，平台消息长度上限由适配器负责（见 `../adapter.md` 的能力与降级）。

---

## 相关文档

- [`../plugin.md`](../plugin.md)：插件接口、`Registrar`/`Option`、`Reply`、`PluginContext` 与插件测试辅助（第 9、11 章）；其中 9.4 节以 `plugins/echo` 为代码示例，本插件不再在彼处重复。
- [`../engine.md`](../engine.md)：路由匹配、优先级、全局中间件链与 `middleware.Auth` 的执行位置（第 7、8、10 章）。
- [`../adapter.md`](../adapter.md)：适配器注册表、`AdapterInfo`/`AdapterCatalog` 的语义与能力降级。
- [`../configuration.md`](../configuration.md)：`plugins.<name>` 段、环境变量覆盖与保留键校验（第 12 章）。
- [`../testing.md`](../testing.md)：`plugins/manage` 的测试行与质量门命令。
