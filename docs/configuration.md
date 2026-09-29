# kei 配置

本文即第 12 章，覆盖配置的来源与加载顺序、顶层各段的真实键名与默认值、`bots[]`/`plugins`/`adapters` 三段的解码与语义、环境变量覆盖规则、一份键名真实的示例，以及配置要求（逐条核对到 `internal/config`、`internal/adaptermgr`、`pkg/kei` 的具体代码位置）。

不覆盖：面向用户的安装、快速开始与联调流程（见 [../README.md](../README.md) 的「配置」「环境变量覆盖」）；插件运行期 `Get/String/Bool/...` 的完整接口说明（见 [plugin.md](plugin.md)）与适配器装配细节（见 [adapter.md](adapter.md)）。

本文以仓库实现为准。结构体、解码、默认值、校验与告警全部逐项对照 `internal/config/config.go`、`internal/config/env.go`、`internal/adaptermgr/adaptermgr.go`、`pkg/kei/kei.go`、`pkg/kei/assemble.go` 与 `configs/config.yaml`。

---

## 12. 配置

在 `configs/config.yaml` 提供示例。

配置有两层来源：YAML 文件与环境变量。加载入口是 `internal/config`：

```go
func Load(path string) (*Config, error)          // 读文件 + os.Environ() 覆盖
func LoadBytes(data []byte, env []string) (*Config, error)  // 解析内存数据 + 指定 env
```

`LoadBytes` 的固定顺序是：`decode`（YAML → 结构体，不做校验）→ `applyEnv`（环境变量覆盖）→ `applyDefaults`（填默认值）→ `validate`（校验）。因此环境变量覆盖同样受校验约束（例如 `KEI_LOG_LEVEL=trace` 会在 `log.level` 上报错），而默认值不会覆盖环境变量给出的空串以外的值。

### 12.1 结构体与顶层键

配置结构体位于 `internal/config`（`config.go`），顶层结构为：

```go
type Config struct {
	Log      LogConfig                  `yaml:"log"`
	Metrics  MetricsConfig              `yaml:"metrics"`
	Bots     []BotConfig                `yaml:"bots"`
	Plugins  map[string]PluginConfig    `yaml:"plugins"`
	Adapters map[string]AdapterConfig   `yaml:"adapters"`
	Limits   LimitsConfig               `yaml:"limits"`
	Auth     AuthConfig                 `yaml:"auth"`
	Storage  StorageConfig              `yaml:"storage"`
}
```

顶层只允许这 8 个键，其余一律报错：`config: 未知顶层键 %q`（`decode`）。顶层必须是映射，否则 `config: 顶层必须是映射，实际为 <tag>`；空文档或 `null` 文档被当作空配置处理。`log`/`metrics`/`limits`/`auth` 直接由 yaml 解码进结构体，段内未声明的键被 yaml 静默忽略；`bots`/`plugins`/`storage` 由自定义解码函数处理，条目上的未知键不会报错，而是收进各自的 `Settings`/`Params`（见 12.2–12.3、12.5）；`adapters` 只接受 `enabled`，其余键报错（见 12.4）。

| 段 | 键 | 结构体字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `log` | `level` | `LogConfig.Level` | string | `info` | 取值 `debug\|info\|warn\|error`（校验时大小写不敏感，值原样保留） |
| `log` | `format` | `LogConfig.Format` | string | `text` | 取值 `text\|json`（校验时大小写不敏感） |
| `metrics` | `addr` | `MetricsConfig.Addr` | string | `""` | 空表示不暴露；非空时启动独立 HTTP 服务，暴露 `/metrics` 与 `/healthz` |
| `limits` | `handler_rate` | `LimitsConfig.HandlerRate` | float64 | `0` | 每用户每秒允许处理的规则次数；`0` 表示不限流 |
| `limits` | `handler_burst` | `LimitsConfig.HandlerBurst` | int | `0` | 处理侧突发容量；`<=0` 时取 `handler_rate`（`internal/ratelimit.New`，且下限为 1） |
| `limits` | `send_rate` | `LimitsConfig.SendRate` | float64 | `0` | 每个 bot 每秒允许发送的条数；`0` 表示不限流 |
| `limits` | `send_burst` | `LimitsConfig.SendBurst` | int | `0` | 发送侧突发容量；`<=0` 时取 `send_rate`（同上，`Engine.New` 只在 `send_rate > 0` 时建限流器） |
| `auth` | `admin_users` | `AuthConfig.AdminUsers` | []string | 空 | 管理员用户 ID 列表，供 `middleware.Auth`（`Engine.isAdmin`，按 `Event.Sender.ID` 精确比较）判定 |
| `bots` | — | `[]BotConfig` | 序列 | 无默认，至少一个 | 见 12.2 |
| `plugins` | — | `map[string]PluginConfig` | 映射 | `{}`（`applyDefaults` 保证非 nil） | 见 12.3 |
| `adapters` | — | `map[string]AdapterConfig` | 映射 | `{}`（`applyDefaults` 保证非 nil） | 见 12.4 |
| `storage` | `type` | `StorageConfig.Type` | string | `memory` | 存储后端类型；必须匹配 `^[a-z0-9_-]+$`，是否存在由装配期判定（见 12.5） |
| `storage` | `dsn` | `StorageConfig.DSN` | string | `""` | 数据源，语义由后端定义；`memory` 忽略 |
| `storage` | 其余键 | `StorageConfig.Params` | 映射 | `{}` | 原样传给后端，如 `cleanup_interval`、`max_open_conns` |

默认值由 `applyDefaults` 落地：`log.level`/`log.format` 为空时填 `info`/`text`；`storage.type` 为空时填 `memory`；`Plugins`/`Adapters` 为 nil 时填空映射。`limits`、`auth.admin_users`、`metrics.addr` 的默认值就是 Go 零值，`applyDefaults` 不做处理。

校验逐段进行，错误都带字段路径（`Config.validate` 及其方法 `LogConfig.validate`、`StorageConfig.validate`、`LimitsConfig.validate`）：

- `bots` 至少一个；`bots[i].name` 非空且全局唯一；`bots[i].adapter` 非空且匹配 `^[a-z0-9_-]+$`；`bots[i].enabled` 必须是布尔（解码阶段报错，不参与此处校验）。
- `log.level` 必须属于 `{debug, info, warn, error}`，`log.format` 属于 `{text, json}`（比较前转小写；空值留给 `applyDefaults`）。
- `limits.*` 的速率与突发容量都不能为负。
- 插件名不能为空；适配器名不能为空且匹配 `^[a-z0-9_-]+$`。
- `storage.type` 非空且匹配 `^[a-z0-9_-]+$`；类型是否存在不在配置层校验（由装配期负责），`dsn` 是否必填也不在此校验（由各后端负责）。

加载失败的错误都包装了来源：`Load` 返回 `config: 读取配置文件 <path>: ...`（读文件失败）或 `config: 配置文件 <path>: ...`（解析/覆盖/校验失败），`Load` 内部对 `os.ErrNotExist` 做 `%w` 包装；YAML 语法错误为 `config: YAML 解析失败: ...`；段级解码错误会再包一层 `<段名> 段: ...`。

### 12.2 `bots[]`

`BotConfig` 有四个保留键，其余键全部进入 `Settings`：

```go
type BotConfig struct {
	Name     string         // 实例名，全局唯一
	Adapter  string         // 平台适配器名，只允许 [a-z0-9_-]
	Enabled  *bool          // 控制是否装配该实例；nil（未写 enabled）视为启用
	Settings map[string]any // 除 name/adapter/enabled/plugins 外的其余键（适配器实例级私有配置）
	Plugins  []string       // 该实例启用的插件名，为空表示全部启用
}
```

- `name`：实例名，即 `Event.BotID`、`SendRequest.BotID`、`Target.BotID` 使用的值；非空、全局唯一（重复时报 `config: bots[i].name: bot 名 %q 重复`）。
- `adapter`：引用的适配器名。必须能解析为已注册的适配器，否则启动失败并列出已注册名（`internal/adaptermgr.Validate`）；被 `enabled: false` 禁用的实例或适配器都跳过该检查。
- `enabled`：实例级开关，缺省启用（见下）。
- `plugins`：该实例的插件**白名单**。`Engine.applyBotPluginFilter` 只在存在非空白名单时收窄规则适用范围：每条规则只对「白名单为空（允许全部）或白名单包含该规则所属插件」的 bot 生效；任何 bot 都不允许的规则被置为匹配不到任何事件（`Rule.BotIDs` 为空切片）。被 `enabled: false` 停用的实例不参与「是否存在非空白名单」的判断，也不进 `Rule.BotIDs`，因此停用唯一配置了白名单的实例不会误伤其他实例。白名单里的名字不要求是已加载插件。
- 其余键（适配器私有配置）：例如 `app_id`、`listen_addr`、`api_url` 等，逐键存入 `Settings`，取值保证可被 `encoding/json` 编解码（`normalizeValue` 递归把映射键规范化为字符串）。无额外键时 `Settings` 保持 `nil`。适配器通过 `AdapterContext.Config`（即 `bot.NewConfig(Settings)`）读取。

**实例级 `enabled` 开关**：`BotConfig.IsEnabled()` 返回 `Enabled == nil || *Enabled`，即只有显式的 `enabled: false` 才停用该实例（与 `adapters.<name>.enabled` 同向：缺省启用，声明本身不是启用的前提）。

- 解码：`decodeBot` 识别 `name`/`adapter`/`enabled`/`plugins`，`enabled` 不进入 `Settings`；非布尔值报 `bots[i].enabled: ...`（错误信息里的索引即条目位置）。
- 效果：停用实例不装配适配器、不接收事件，也不参与适配器校验（未知适配器名与保留键 `listen_addr` 都跳过），`internal/adaptermgr.Build` 记 info 日志 `bot 已禁用，跳过`（字段 `bot`、`adapter`）。
- 环境变量覆盖：`KEI_BOTS_<BOTNAME>_ENABLED=false|true`（`internal/config/env.go` 的 `applyBotEnv` 新增 `enabled` 分支，用 `envBool` 解析；值无法解析为布尔时静默忽略）。
- 与 `adapters.<name>.enabled` 的区别：`bots[].enabled` 只停用**一个实例**（跳过该实例的装配、监听端口与校验）；`adapters.<name>.enabled: false` 停用**整个平台适配器**，其全部 `bots[]` 条目一并跳过（见 12.4）。示例配置里 `feishu-main` 的 `enabled: false` 现在真的生效：该 bot 不再装配、不监听端口。

**保留键 `listen_addr`**（常量 `bot.OptListenAddr`）：只允许出现在声明了 `net_listen` 权限的适配器实例上。`internal/adaptermgr.checkReservedOptions` 在 `Validate` 阶段检查，命中时报 `config: bot <name> 配置了保留键 listen_addr，但适配器 <adapter> 未声明 net_listen 权限`。判定「已配置」的规则是该键存在且 `fmt.Sprint` 后去空白非空；核心不解释它的取值，由适配器自行解析。被 `bots[].enabled: false` 停用的实例不参与该校验。

**同平台多实例与 `Target.BotID`**：插件发送时构造 `bot.Target{Platform, BotID, ChannelID, ...}`。`Engine.resolve` 的行为是：`BotID` 非空时按 bot 名定位实例，并要求该实例的平台与 `Target.Platform` 一致（不一致报 `engine: bot %q is on platform %q, not %q`）；`BotID` 为空时，只有该平台恰好一个 bot 才自动选择，0 个报 `no adapter for platform %q`，多个报 `platform %q has %d bots, target.BotID is required`。因此同一平台配置多个 bot（例如两个飞书应用、同适配器的多个实例）时发送必须显式指定 `BotID`；回复当前会话用 `TargetFromEvent` 自动带上事件的 `BotID`。每个 `bots[]` 条目都会得到一个独立的适配器实例（由工厂构造），实例状态互不影响。

### 12.3 `plugins.<name>`

```go
type PluginConfig struct {
	Enabled  bool           // 插件是否启用；插件未在配置中出现时为 false
	Settings map[string]any // 除 enabled 外的其余键
}
```

- **未列出即不启用**：`PluginConfig.Enabled` 是普通 `bool`，零值 `false`；`Config.Plugin(name)` 对缺失插件返回零值。想启用必须写 `enabled: true`，或使用布尔标量简写 `echo: true`。
- `enabled`：布尔标量或带引号的布尔文本（`enabled: "false"` 也得到 `false`，见 `decodeBool`）；非布尔（如 `enabled: 也许`）报 `plugins.<name>.enabled: 需要布尔值`。`enabled` 不会进入 `Settings`。
- 私有配置：其余键进入 `Settings`，由引擎在构造插件管理器时转换为 `bot.Config`（`engine.New` → `pluginmgr.Deps.Configs`），插件经 `PluginContext.Config` 读取；插件未配置时是空配置（`bot.NewConfig(nil)`）。
- 读取方式（`pkg/bot/api.go`，`Config` 对 nil 接收者与缺失键都返回默认值）：`Raw() map[string]any`、`Get(key) (any, bool)`（支持 `"a.b.c"` 多级路径）、`String(key, def)`、`Bool(key, def)`、`Int(key, def)`、`Duration(key, def)`（字符串按 `time.ParseDuration`，数字按秒）、`Strings(key)`（缺失返回 nil）、`Unmarshal(v)`（依赖 `encoding/json` 标签）。
- 「`enabled: true` 但没有任何已注册插件对应」只会得到 warn（`pkg/kei/assemble.go` 的 `selectPlugins`：`enabled plugin is not registered`）。

### 12.4 `adapters.<name>`

```go
type AdapterConfig struct {
	Enabled *bool // nil（未写 enabled）视为启用
}
```

声明是**可选**的：进程内适配器只靠 `bots[].adapter` 引用即可工作，`adapters` 段只用于显式启用/禁用。该段只支持 `enabled` 键，其余键一律报错（见下）。

- **`enabled` 缺省 `true`**（与插件相反）：`AdapterConfig.IsEnabled()` 返回 `Enabled == nil || *Enabled`，`Config.AdapterEnabled(name)` 对未声明的适配器也返回 `true`。只有显式 `enabled: false` 才禁用。非布尔报 `adapters.<name>.enabled: 需要布尔值`。
- **只允许 `enabled` 键**：写了任何其他键（如 `platform`/`timeout`/`permissions`）都在解码阶段报 `config: adapters.<name>: 未知键 "<key>"（只支持 enabled）`（`decodeAdapter`），与 `enabled` 取值无关（禁用的条目也不会豁免）。
- **未知私有键告警（不报错）**：实例级未知键在装配每个 bot 时经 `adaptermgr.warnUnknownOptions` 告警，键集来自适配器的 `AdapterMetadata.Options`。适配器未声明 `Options`（空列表）时静默跳过。告警形如 `适配器不认识的配置键`，字段为 `bot`、`adapter`、`key`。
- 装配期的其他告警：`adapters` 段声明了但未注册的适配器（`适配器已在 adapters 段声明但未注册（是否漏了空导入？）`）。
- **禁用语义**：`enabled: false` 的适配器不装配，其 `bots[]` 条目一并跳过（每个跳过的 bot 记 warn：`适配器已禁用，跳过 bot`），不参与未知适配器名与保留键校验；核心仍可只运行插件（`pkg/kei.Run` 会记 warn：`没有启用的适配器，核心将只运行插件`）。

### 12.5 `storage`

```go
type StorageConfig struct {
	Type   string         `yaml:"type"`
	DSN    string         `yaml:"dsn"`
	Params map[string]any // 除 type/dsn 外的其余键
}
```

存储后端由 `storage.type` 选择，`dsn` 与其余键进入 `Params` 后原样交给对应后端。`type` 默认 `memory`，`storage` 段整体可以省略。

- **已注册类型**：`memory`（默认，进程内内存，重启即丢）、`sqlite`（GORM + cgo 版 `github.com/mattn/go-sqlite3`，`dsn` 是文件路径或 `file:...?params`）、`mysql`（GORM + `github.com/go-sql-driver/mysql`，`dsn` 形如 `user:pass@tcp(host:3306)/db?parseTime=true`）。`parseTime=true` 是过期时间列能扫描为 `time.Time` 的前提，核心不校验，缺失时由驱动报错。
- **类型注册表在装配期**：`internal/storage` 提供 `Register`/`Open`/`Kinds`，各后端包在 `init()` 注册；`pkg/kei.buildStorage` 调用 `storage.Open`，未知类型在此报错并列出已注册类型：`kei: 初始化 storage: storage: 未知存储类型 "x"（已注册: memory, mysql, sqlite）`。配置层不维护类型白名单，第三方可注入 `Options.Storage` 使用自定义实现。
- **通用后端参数**：`cleanup_interval`（过期行清理间隔，默认 `1m`，`0` 关闭后台清理；过期键在 `Get` 命中时也会被惰性删除并返回 `bot.ErrNotFound`）。
- **MySQL 连接池参数**：`max_open_conns`、`max_idle_conns`、`conn_max_lifetime`（`time.ParseDuration` 字符串或秒数）；三者都缺失时不改动驱动默认值。SQLite 不使用这些键。
- **注入优先**：`Options.Storage` 非 nil 时优先使用（由调用方拥有，`Run` 不关闭）；为 nil 时按 `storage.type` 构造，并在 `Run` 返回前关闭自有实例。

### 12.6 环境变量覆盖

规则在 `internal/config/env.go`（`applyEnv`、`applyEnvPath`、`canonicalName`、`canonicalSegments`）：

- 只有以 `KEI_` 开头（前缀不区分大小写）的变量参与覆盖；其余一律忽略。`KEY=VALUE` 中缺少 `=` 的元素、以及空的 `KEI_` 被忽略。
- 路径段之间用 `_` 连接；每一段都做规范化：转小写，`-` 与 `.` 视作 `_`，空段丢弃。因此 `KEI_BOTS_FEISHU_MAIN_APP_ID` 与 `KEI_BOTS_feishu-main_APP_ID` 命中同一个 bot 的同名键。
- 顶层段的固定路径（`applyEnvPath` 直接赋值或经 `envFloat`/`envInt` 解析）：

| 环境变量 | 目标字段 |
| --- | --- |
| `KEI_LOG_LEVEL` / `KEI_LOG_FORMAT` | `log.level` / `log.format`（原样字符串） |
| `KEI_METRICS_ADDR` | `metrics.addr` |
| `KEI_LIMITS_HANDLER_RATE` / `KEI_LIMITS_SEND_RATE` | 浮点速率；空值表示不覆盖，非法值报错并点名变量 |
| `KEI_LIMITS_HANDLER_BURST` / `KEI_LIMITS_SEND_BURST` | 整数突发容量；同样空值不覆盖、非法值报错 |
| `KEI_AUTH_ADMIN_USERS` | 逗号分隔列表，忽略空白与空项；无有效项时为 nil |
| `KEI_STORAGE_TYPE` / `KEI_STORAGE_DSN` | `storage.type` / `storage.dsn`（原样字符串） |
| `KEI_STORAGE_PARAMS_<KEY>` | `storage.Params[<key>]`（键名按规范化规则映射，值按 YAML 规则推断类型） |

- `KEI_BOTS_<BOT>_<KEY>`：`<BOT>` 按规范化名匹配**配置中已存在的** bot（快照于覆盖开始前，因此 `KEI_BOTS_X_NAME` 之后的其他变量仍按旧名匹配；同时命中多个 bot 时取规范化名最长者，长度相同则全部应用）。`<KEY>` 为 `name`/`adapter`/`plugins`（逗号分隔列表）时写入对应字段，为 `enabled` 时按布尔覆盖实例启用状态（值不可解析为布尔则**静默忽略**），其余键写入 `Settings`。
- `KEI_PLUGINS_<NAME>_<KEY>`：只命中配置中已存在的插件名，未知插件名忽略。`<KEY>` 为 `enabled` 时按布尔覆盖（值不可解析为布尔则**静默忽略**），其余键写入该插件的 `Settings`。
- `KEI_ADAPTERS_<NAME>_<KEY>`：只命中配置中已存在的适配器名。支持的键只有 `enabled`（指针布尔）；其余键忽略。
- 值的类型按 YAML 规则推断（`convertValue`）：`true`→bool、`3`→int、`1.5`→float64、其余→string、空值→空字符串；无法按 YAML 解析时退化为原始字符串。写入 `Settings` 时，若已有键规范化后同名，则沿用 YAML 中的原键名（多个同名时取字典序最小者），否则使用规范化后的路径作为键名。
- 未匹配任何已知路径的 `KEI_*` 变量被静默忽略（不报错，也不会创建新的 bot/plugin/adapter 条目）。

因为覆盖发生在校验之前，非法的日志级别、负数限流等仍会被 `validate` 拒绝；`KEI_LIMITS_*` 的非数字值在 `applyEnv` 阶段就报错。

示例（键名取自 `configs/config.yaml` 与 [../README.md](../README.md) 的「环境变量覆盖」）：

```bash
KEI_LOG_LEVEL=debug
KEI_METRICS_ADDR=127.0.0.1:9090
KEI_LIMITS_SEND_RATE=5
KEI_AUTH_ADMIN_USERS=ou_xxx,123456
KEI_BOTS_FEISHU_MAIN_APP_SECRET=xxx      # bot 名 feishu-main → Settings["app_secret"]
KEI_PLUGINS_ECHO_ENABLED=false           # 只对已列出的插件生效
KEI_ADAPTERS_MYIM_ENABLED=false          # 只对 adapters 段已声明的适配器生效
KEI_STORAGE_TYPE=sqlite                  # 切换存储后端
KEI_STORAGE_DSN=/var/lib/kei/storage.db
KEI_STORAGE_PARAMS_CLEANUP_INTERVAL=5m   # storage.Params["cleanup_interval"] = "5m"
```

### 12.7 示例

以下片段取自 `configs/config.yaml`（注释与未改动的部分已省略），键名与取值均为该文件中的真实键名；其中 `adapters.feishu` 在示例文件里处于注释状态，这里是展开后的形态：

```yaml
log:
  level: info    # debug | info | warn | error
  format: text   # text | json

metrics:
  addr: 127.0.0.1:19090     # 空表示不暴露

limits:
  handler_rate: 0           # 0 表示不限流
  handler_burst: 0
  send_rate: 0
  send_burst: 0

auth:
  admin_users: []

adapters:
  feishu:                   # 可选声明：只允许 enabled 键
    enabled: false

bots:
  - name: mock-main
    adapter: mock
    platform: mock
    listen_addr: 127.0.0.1:18080
  - name: feishu-main
    enabled: false           # 实例级开关：只跳过这一个 bot（缺省启用）
    adapter: feishu
    app_id: cli_xxx
    app_secret: xxx
    verification_token: xxx
    listen_addr: 127.0.0.1:18081
    path: /feishu/event
  - name: qq-main
    adapter: onebot
    mode: reverse_ws         # forward_http / reverse_http / forward_ws / reverse_ws，缺省 reverse_ws
    api_url: http://127.0.0.1:3000   # HTTP API；forward_http/reverse_http 下必填
    ws_url: ""               # 正向 WebSocket 地址，如 ws://127.0.0.1:6700；forward_ws 下必填
    listen_addr: 127.0.0.1:18082
    path: /onebot/event      # HTTP 上报路径（forward_http 下生效）
    ws_path: /onebot/ws      # 反向 WebSocket 路径（reverse_ws 下生效）
    ping_interval: 30s
    secret: ""
    access_token: ""
    self_id: ""

plugins:
  echo:
    enabled: true
  manage:
    enabled: true
    plugins_admin_only: true

# 可选：切换存储后端。缺省 memory（重启即丢）。
# storage:
#   type: sqlite
#   dsn: /var/lib/kei/storage.db
#   cleanup_interval: 1m
```

### 12.8 配置要求（逐条核对）

1. 配置结构体放在 `internal/config`。
   核对：`internal/config/config.go` 定义 `Config`、`LogConfig`、`MetricsConfig`、`LimitsConfig`、`AuthConfig`、`StorageConfig`、`BotConfig`、`PluginConfig`、`AdapterConfig`；环境变量覆盖在 `internal/config/env.go`；配置测试在 `internal/config/config_test.go`。
2. 支持环境变量覆盖。
   核对：`applyEnv`/`applyEnvPath`（`env.go`），规则见 12.6；`Load` 传入 `os.Environ()`。
3. 支持插件独立配置；适配器实例配置写在 `bots[]` 条目上。
   核对：`PluginConfig.Settings`（`decodePlugin`）、`BotConfig.Settings`（`decodeBot` 的 `default` 分支）；实例级经 `AdapterContext.Config` 读取。
4. 配置加载失败必须返回明确错误。
   核对：`Load`/`LoadBytes` 全程 `%w` 包装并带 `path`；`decode` 报未知顶层键、顶层非映射、YAML 语法错误；`Config.validate` 与 `LogConfig.validate`/`StorageConfig.validate`/`LimitsConfig.validate` 报带字段路径的错误（见 12.1）。
5. `adapters.<name>.enabled` 控制适配器是否启用，缺省为 **true**（与插件相反：适配器不需要声明即可用，声明只用于启用/禁用）。环境变量 `KEI_ADAPTERS_<NAME>_ENABLED` 可覆盖。
   核对：`AdapterConfig.IsEnabled`、`Config.AdapterEnabled`（未声明返回 true）、`decodeAdapter` 的 `enabled` 分支、`applyAdapterEnv` 的 `enabled` 分支。注意环境变量只对 `adapters` 段已声明的名字生效（`applyAdapterEnv` 遍历 `c.Adapters`）。
6. `enabled: false` 的适配器不装配、其 `bots[]` 条目一并跳过（每个跳过的 bot 记 warn 日志），也不做字段校验；核心仍可只运行插件。
   核对：`adaptermgr.Build`（`if !cfg.AdapterEnabled(bc.Adapter)` → `deps.Logger.Warn("适配器已禁用，跳过 bot", ...)`）、`adaptermgr.Validate`（同类判断直接 `continue`，跳过未知适配器名与 `checkReservedOptions`）、`Config.validate`（`!ac.IsEnabled()` 时 `continue`）、`cmd/bot/main.go`（`len(list) == 0` 时 warn `没有启用的适配器，核心将只运行插件`）。实例级的同向开关是 `bots[].enabled`（`BotConfig.IsEnabled`）：`adaptermgr.Build` 先判实例、记 info `bot 已禁用，跳过`，`adaptermgr.Validate` 里 `!bc.IsEnabled()` 与适配器禁用一样 `continue`。
7. `bots[].adapter` 必须能解析为已注册的适配器，否则启动失败，并列出所有已注册适配器名；被禁用的适配器跳过该检查。
   核对：`adaptermgr.Validate` → `config: bots[i].adapter: 未知适配器 %q（已注册: ...）`，名单来自 `registeredNames()`，空名单显示「无」；`!bc.IsEnabled() || !cfg.AdapterEnabled(bc.Adapter)` 的条目在循环开头 `continue`。
8. `adapters.<name>` 声明只允许 `enabled` 键：其余键一律报错，避免静默失效的配置。
   核对：`Config.validate` → `config: adapters.<name>: 只允许 enabled 键（适配器实例配置写在 bots[] 条目上）`。
9. 保留键 `listen_addr` 只允许出现在声明了 `net_listen` 的适配器实例上，否则启动失败。
   核对：`internal/adaptermgr/adaptermgr.go` 的 `checkReservedOptions`（`Validate` 阶段）；键常量 `bot.OptListenAddr`。
10. 适配器不认识的私有键必须告警（不报错），以支持第三方适配器独立演进；键集来自 `AdapterMetadata.Options`。
    核对：`adaptermgr.warnUnknownOptions`（`Build` 中实例级调用用 `meta.Options`）。告警不阻断装配。

---

## 附：已知缺口与实现边界

现状与证据文件：

- `KEI_BOTS_<NAME>_*`、`KEI_PLUGINS_*` 与 `KEI_ADAPTERS_*` 只覆盖配置中已存在的名字；未在 `bots`/`plugins`/`adapters` 段声明的名字被静默忽略，也不会创建新条目（`internal/config/env.go` 的 `applyBotEnv` 遍历 `c.Bots`，`applyPluginEnv`/`applyAdapterEnv` 遍历 `c.Plugins`/`c.Adapters`）。

---

## 相关文档

- [architecture.md](architecture.md)：项目结构概览与 `internal/config` 在整体架构中的位置。
- [adapter.md](adapter.md)：适配器接口、注册表与权限裁剪，`adapters` 段配置如何装配成实例。
- [../README.md](../README.md)：面向用户的配置示例、环境变量示例与快速开始。
