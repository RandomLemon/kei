# TODO — kei 待修清单

本文件是 2026-10-06 框架结构分析的落地清单，供后续 agent **逐条**处理。

## 使用说明

- 每条独立交付：完成一条把 `- [ ]` 改为 `- [x]`，并在条目末尾追加 `— 完成 <commit>/<说明>`。
- 修改前先读对应领域文档（[`docs/README.md`](docs/README.md) 索引）与 [`AGENTS.md`](AGENTS.md)。
- 行号基于 `f89e8b6`；改动后以**符号名**为准。
- 证据强度：`[已复现]` = 本次用临时用例实测；`[读码]` = 逐行读码确认；`[静态]` = 子 agent 读码 + 调用链核对，落地前请在本地再确认一次。
- 每条验收至少覆盖：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...`（见 `AGENTS.md` 2.4）。
- 公共 API / 配置键 / 行为变更必须同步更新对应 `docs/` 与 `README.md`（`AGENTS.md` 2.4、`docs/README.md` 文档约定）。
- 禁止兼容垫片：迁移全部调用方后删除旧路径（`AGENTS.md` 7）。

---

## P0 — 正确性 / 静默故障

- [x] **1. dedup：过期后重加的 ID 会被自己的陈旧槽位提前淘汰** `[已复现]` — 完成 018081d：`Set` 的 `order` 改为 `slot{id, seq}` 槽位，`Add` 每次写入分配唯一 `seq`，`evictLocked` 只删除 `items[id].seq == slot.seq` 的槽位（陈旧槽位跳过并继续按插入序淘汰下一条活条目），`compactOrderLocked` 同步丢弃失效槽位。回归测试 `internal/dedup/dedup_test.go` 的 `TestStaleOrderSlotDoesNotEvictReAddedID`（假时钟：X 过期→重加，容量淘汰删最旧的 A 而非 X；修复前该测试失败，报 `重加的 X 在 TTL 内应仍被视为已出现`）。取证：`gofmt -l`、`go vet ./internal/dedup/`、`go test -race -count=1 ./internal/dedup/` 全绿。
  - 位置：`internal/dedup/dedup.go:51-61`（`Add` 重加仍 `append`）、`:127-140`（`evictLocked`）、`:144-152`（`compactOrderLocked`）
  - 问题：`Add` 对“已过期再重加”的 ID 再次 append；`evictLocked` 弹出 `order[0]` 后只按 `items[id]` 是否存在决定删除，删掉的是**新写入的活条目**；compaction 保留重复槽位。实测 `New(2, 50ms)`：`Add(X)`→过期→`Add(X)`→`Add(Y)`→`Add(Z)` 后 `Seen(X)==false`。后果：TTL 内重投的同一事件被二次处理（平台重试正是该路径）。
  - 修复：`order` 槽位携带唯一序号/过期时刻，淘汰时只删除与 `items` 当前条目一致的槽位；或改为按插入序的小顶堆。
  - 补注：原文 trace（过期后重加 X，再加 Y、Z）里 X 本来就是最旧的**活**条目，淘汰 X 属 FIFO 正确结果，该 trace 不区分新旧实现；真正暴露缺陷的是“陈旧槽位排在其他活条目之前”：X 首次插入 → X 过期 → 重加 X → 再加 B（容量已满），旧实现弹出 X 的陈旧槽位并删掉重加后的 X，而更旧的 A 存活（见 `TestStaleOrderSlotDoesNotEvictReAddedID`）。
  - 验收：`internal/dedup` 新增回归测试（过期重加后淘汰其它键，`Seen(重加键)==true`）；`go test -race ./internal/dedup ./internal/eventbus ./internal/middleware`。

- [x] **2. dedup：`order` 在重投场景下无限增长** `[已复现]` — 完成 018081d：已随第 1 条的唯一槽位落地（`Add` 分配唯一 `seq`，`compactOrderLocked` 只保留 `items[id].seq` 匹配的当前槽位，故 compaction 不再保留活 ID 的全部重复槽位）。回归测试 `internal/dedup/dedup_test.go` 的 `TestOrderStaysBoundedUnderReAdd`（`New(4, 1s)`、2 万次跨 TTL 重加同一 ID，断言 `len(s.order) <= 4`，实测 1；修复前同一测试实测 20000）；`docs/engine.md` 8.5 的容量条目已改写为“每个条目在 `order` 中只有一个有效槽位”。
  - 位置：`internal/dedup/dedup.go:57-63`、`:120`、`:129`、`:144-152`
  - 问题：compaction 只在 `now.Sub(lastGC) >= ttl` 时触发，且保留活 ID 的**全部重复槽位**。实测 `New(4, 5s)`、2 万次“跨 TTL 重加同一批 ID”后 `order=20001`。注：纯唯一 ID 流量下不增长（实测 50 万唯一键仅 +12KB），所以该缺口恰好只命中去重的用途。
  - 修复：并入第 1 条（唯一槽位）；至少在淘汰路径也触发压缩，且压缩时对活 ID 去重。
  - 验收：测试断言长跑重加后 `order` 有界（同包测试可直接读 `len(s.order)`）。
  - 文档：`docs/engine.md` 8.5 的“压缩插入顺序切片防止无限增长”要么成就、要么改写。

- [x] **3. `Engine.Run` 可能把适配器崩溃吞成 `nil`** `[读码]` — 完成 018081d：`Run` 在 `select` 后无条件 `cancel()`，`shutdown` 之后用非阻塞 `drainErrors` 收敛 `adapterErr` 中的全部残余错误（`errors.Join` 合并，含 select 随机选中的那一路），并统一以 `engine aborted by adapter failure` 记 `Error` 日志后返回；新增 `internal/engine/engine_test.go` 的 `TestRunSurfacesAdapterStartError`（`failingAdapter` 立即返回哨兵错误，重复 20 次断言错误非 nil、`errors.Is` 命中、含 bot 名且进日志）。取证：在 `HEAD` 的临时 worktree 内跑同一测试必失败（首次即报“适配器错误必须进日志”，日志里根本没有该错误）；`go test -race -count=1 ./internal/engine/` 全绿。
  - 位置：`internal/engine/engine.go:329-345`
  - 问题：适配器 goroutine 先 `adapterErr <- err`（带缓冲立即成功）再 `cancel()`；主 `select` 两分支几乎同时就绪，随机选中 `<-runCtx.Done()` 时 `runErr` 仍为 `nil`，且错误**无任何日志** → 适配器异常后进程“安静地”正常退出。
  - 修复：`cancel` 后统一读取 channel 残余，或在 `runCtx.Done()` 分支非阻塞检查 `adapterErr`；无论如何错误必须进日志。
  - 补注：两分支同时就绪的概率不低——起完适配器 goroutine 后还有 `engine started` 日志等开销，失败适配器往往在进入 `select` 之前就完成“写 channel + cancel”。用等价最小复现（同一 `select` 结构 + 1ms 前置延迟）跑 200 次：旧逻辑 107 次返回 `nil`，新逻辑 0 次。
  - 验收：测试模拟 `Adapter.Start` 返回非 `ctx.Canceled` 错误，断言 `Run` 返回非 nil 错误（重复运行以覆盖随机性）。

- [x] **5. MySQL 后端：`key` 是保留字，Get/Delete 语法错误** `[读码]` — 完成 10ec513：新增 `sqlstore.whereKey(key)`（`clause.Eq{Column: clause.Column{Name: "key"}, Value: key}`），`Get`/惰性清理/`Delete` 全部改用它，列名交由 dialector 按方言引用（与 `Set` 的 `OnConflict` 同一约定）。回归测试 `internal/storage/sqlstore/sqlstore_test.go` 的 `TestKeyColumnQuotedForMySQL`（MySQL 方言 dry-run 断言 `WHERE \`key\` = 'k1'`，不需要真实服务器）。实机取证：podman 起 MySQL 8.4，修复前 `go test ./internal/storage/mysql/` 两个用例均报 `Error 1064 … near 'key = ? ORDER BY …'`，修复后往返与 TTL 全绿。文档：`docs/testing.md` 的 sqlstore 行 2→3。
  - 位置：`internal/storage/sqlstore/sqlstore.go:77`、`:86`、`:107`（裸 `Where("key = ?", …)`）、`Entry.Key`（`:22-23`）
  - 问题：GORM 对字符串条件原样下发；`KEY` 在 MySQL 8 是保留字（需引用），SQLite 宽容 → 仅 MySQL 后端全挂。`internal/storage/mysql/mysql_test.go:25` 未设 `KEI_TEST_MYSQL_DSN` 即 Skip，CI 覆盖不到。
  - 修复：改用 `clause.Eq{Column: clause.Column{Name:"key"}, Value:key}` 或 `Where(map[string]any{"key": key})`，由 dialector 负责引用。
  - 验收：本地起 MySQL 设 `KEI_TEST_MYSQL_DSN` 跑 `go test ./internal/storage/mysql/`（或至少用 dialector 的 SQL 生成断言）。

- [x] **6. 空环境变量被当成覆盖写回，静默改配置** `[读码]` — 完成 018081d：在 `applyEnvPath` 入口统一 `if strings.TrimSpace(value) == "" { return nil }`，所有键（含 `limits.*`、`bots`/`plugins`/`adapters` 与 `storage.params.*`）的空白值一律等同「未设置」；随之删除 `limits_*` 四处调用点检查与 `envFloat`/`envInt` 内的重复空值分支（唯一规则只留一处）。新增 `internal/config/config_test.go` 的 `TestEmptyEnvValuesDoNotOverride`：一份含非默认值的 YAML + 13 个空白值变量，断言 log/metrics/limits/auth/storage（含 `params.cleanup_interval`）/bots（`Settings` 与 `enabled`）/plugins/adapters 全部保持 YAML 值且 `EnvOverrides` 为空。取证：在 `HEAD` 的临时 worktree 跑该测试必失败（`LoadBytes 失败: config: log.format: 不支持的值 "   "`）；另用临时代码观察旧行为：`KEI_STORAGE_TYPE=` 使 `storage.type` 由 `sqlite` 变 `memory`、`KEI_STORAGE_PARAMS_CLEANUP_INTERVAL=` 清空参数、`KEI_AUTH_ADMIN_USERS=` 清空管理员，且三条都进了 `EnvOverrides`。文档：`docs/configuration.md` 12.6 增加全局空白值规则并修正 `limits.*`/`convertValue` 表述、12 章引言，`README.md`「环境变量覆盖」同步。
  - 位置：`internal/config/env.go:47-55`（`log_level`/`log_format`/`metrics_addr`）、`:88-96`（`auth_admin_users`/`storage_type`/`storage_dsn`）直接赋值；对比 `:56-87` 的 `limits_*` 有空值提前返回
  - 问题：`LoadBytes` 顺序是 `applyEnv → applyDefaults`（`config.go:172-178`）。`KEI_STORAGE_TYPE=`（compose/systemd 中 `KEY=${UNSET}` 的常见写法）把 YAML 的 `sqlite` 打回 `memory`（默认值重新填上）；`KEI_AUTH_ADMIN_USERS=` 清空管理员；两者还会进 `EnvOverrides`，日志看像有意覆盖。`docs/configuration.md` 12.6 只写了 `limits.*` 的空值规则。
  - 修复：所有非 limits 路径同样 `TrimSpace(value)==""` 时跳过且不 `recordEnv`；或定义显式的 `KEI_*_RESET` 约定并写进文档。
  - 验收：`internal/config` 测试覆盖各键空值不覆盖 YAML；同步 `docs/configuration.md` 12.6。

- [x] **7. `serveMetrics` 在 `Run` 早期失败时泄漏监听与 goroutine** `[读码]` — 完成 018081d：`serveMetrics` 改为返回同步清理函数 `stop func()`（`sync.Once` 幂等：`srv.Shutdown` + 等 `Serve` goroutine 退出，确保返回时监听端口已释放），`Run` 就地 `defer stopMetrics()`；随之删掉原来那个只等 `<-ctx.Done()` 的关闭 goroutine（不再需要 ctx 参数），`Serve` 提前退出时也不会再留下等待 ctx 的 goroutine。新增 `pkg/kei/assemble_test.go` 的 `TestServeMetricsStopReleasesPort`（`stop` 幂等 + `stop` 返回后立即能重新 `net.Listen` 同一地址）与 `TestRunReleasesMetricsPortOnEarlyFailure`（装配失败后第二次 `Run` 绑定同一 `metrics.addr` 并正常退出）。取证：同一 `Run` 用例在 `HEAD` 的临时 worktree 内失败 —— `第二次 Run: metrics listen 127.0.0.1:42467: bind: address already in use`。
  - 位置：`pkg/kei/assemble.go:177-206`（仅 `<-ctx.Done()` 才关闭）、调用点 `pkg/kei/kei.go:84-88`（早于 `adaptermgr.Build`/`selectPlugins`/`engine.New`）
  - 问题：任一后续步骤返回错误时 `Run` 直接返回而调用方 ctx 仍存活 → 端口不复用（重试报 `address already in use`）、每次失败泄漏一个 goroutine。
  - 修复：`Run` 内建子 ctx 并 `defer cancel()`；或把 metrics 启动挪到装配成功之后。
  - 补注：只做「子 ctx + defer cancel()」不够——`srv.Shutdown` 在另一个 goroutine 里异步执行，`Run` 返回瞬间端口可能仍被占用，紧接着的第二次 `Run` 依旧可能撞上 `address already in use`。因此改为返回同步 `stop func()`（调用方 `defer`），保证返回即释放。
  - 验收：测试连续两次 `Run`（第二次在首次失败后）可正常绑定。

- [x] **8. SQL 存储：AutoMigrate/ApplyPool 失败时泄漏连接池** `[静态]` — 完成 1d132df：`sqlstore.New`/`NewWithClock` 明确取得 db 所有权——AutoMigrate 失败时经新增的 `sqlstore.CloseDB` 关闭底层 `*sql.DB`（`errors.Join` 并入返回错误），`internal/storage/mysql` 的 `ApplyPool` 失败路径同样 `CloseDB`；`Store.Close` 改为复用 `CloseDB`。回归测试 `internal/storage/sqlstore/sqlstore_test.go` 的 `TestNewClosesPoolOnMigrateFailure`（只读库触发 AutoMigrate 失败，断言 Ping 报错且 `OpenConnections==0`）与 `TestCloseDBClosesPool`（真实关闭 + 重复调用幂等）。取证：修复前同一只读 DSN 复现 20 次构造失败后 fd 10→30，修复后 10→10（delta 0）；`go test -race -count=1 ./internal/storage/...` 全绿。文档：`New`/`Close`/`CloseDB` 注释写明所有权语义，`docs/testing.md` 的 sqlstore 行由「无独立测试文件」改为 2 个用例。
  - 位置：`internal/storage/sqlstore/sqlstore.go:62-64`、`internal/storage/mysql/mysql.go:37-39`、`internal/storage/sqlite/sqlite.go:31-37`
  - 问题：`gorm.Open` 已拿到 `*sql.DB`，失败路径直接 `return nil, err`，未 `Close`（SQLite 还占文件锁）；`pkg/kei` 侧也拿不到 closer。
  - 修复：失败分支 `if sqlDB, err := db.DB(); err == nil { _ = sqlDB.Close() }`；明确 `Store.Close` 对调用方传入 DB 的所有权语义。
  - 验收：构造失败迁移（如只读文件/非法 schema），断言无 fd/连接泄漏（可用 `db.Stats()` 或重复开关）。

- [x] **9. MySQL DSN（含密码）明文进 DEBUG 日志** `[静态]` — 完成 7bc7699：`redactEnvValue` 改为两道判定——键名标记（新增 `PASS`（覆盖 `PASSWORD`/`PASSWD`）与 `DSN`，掩码常量 `redactMask`）和取值形态 `looksLikeCredentialValue`（`scheme://user:pass@host`、`user:pass@tcp(...)/db`、`... password=xxx` 三类；保守判定，无凭据 `userinfo` URL、`file:` 路径、普通地址不误伤）。未采用「`storage.dsn` 只记 `(set)`」的最简方案：那会丢掉 `EnvOverrides` 的排查信息，泄露面改为在日志层收敛。回归测试 `pkg/kei/assemble_test.go` 的 `TestRedactEnvValue`（12 案例表驱动）与扩展后的 `TestLogEnvOverrides`（含 `storage.dsn` 明文 DSN、插件键形态、三个密码断言不出现在输出中）。端到端取证：`go run ./cmd/bot` + `KEI_STORAGE_DSN=user:pa55w0rd@tcp(...)` / `KEI_PLUGINS_ECHO_OPTIONS_ENDPOINT=admin:hunter2@tcp(...)` 的 DEBUG 行均为 `value=******`，日志中密码出现 0 次；`https://example.com/hook` 保持原样。文档：`docs/configuration.md` 12.6、`README.md` 可观测性同步，`docs/testing.md` 的 `pkg/kei` 行 13→14。
  - 位置：`pkg/kei/assemble.go:145-148`（敏感标记列表）、`:164-175`（`redactEnvValue` 只按变量名）、`internal/config/env.go:94-96`（`storage.dsn` 记入 `EnvOverrides`）
  - 问题：`KEI_STORAGE_DSN=user:pass@tcp(...)/db?parseTime=true` 不命中任何标记 → 密码写进 stdout；`docs/configuration.md` 12.6 承诺“避免密钥写入日志”。
  - 修复：按值形态兜底掩码（`@tcp(`、`://`、`user:...@`），并补 `DSN`/`PASS` 标记；最简：`storage.dsn` 只记 `(set)`。
  - 验收：测试 DEBUG 日志输出不含 DSN 密码。

---

## P1 — 行为/契约偏差

- [x] **10. 全局中间件顺序：重复事件先消耗限流额度、Dedup 落在 Timeout 的 detached goroutine 内** `[已复现]` — 完成 723ce16：全局链改为 `Recover → Logger → Metrics → Auth → Dedup → RateLimit → Timeout → Recover`——框架自身的策略判定（`Auth`/`Dedup`/`RateLimit`）全部移到 `Timeout` 之外、在调用者 goroutine 中同步执行，`Timeout` 只包裹第三方代码（插件 `Use` 中间件与 Handler）；`Dedup` 提到 `RateLimit` 之前，重投先判重、不消耗 `limits.handler_rate` 配额；去重标记在启动超时 goroutine 之前写入。语义决策：**去重是「标记开始」**（首次出现即写入，随后成功/报错/超时/panic 都不回滚），与总线层在 `Publish` 时写入一致；代价（超时的尝试仍占用槽位、TTL 内重投被丢弃）与理由写进 `middleware.Dedup` 注释与 `docs/engine.md` 10.5/10.6。TODO 原修复建议（`Timeout → Recover → Auth → Dedup → RateLimit`）只覆盖前半：它仍把 `Dedup` 留在 `Timeout` 的 goroutine 内，故采用当前顺序。回归测试 `internal/engine/pluginapi_test.go` 的 `TestGlobalMiddlewareChainSemantics`（「重投不消耗限流额度」：突发 1 的限流器下同一事件重投须得 `ErrDuplicate`、同用户新事件须得 `ErrRateLimited`；「超时的尝试仍占用去重槽位」：阻塞 Handler 超时后重投须得 `ErrDuplicate`）。取证：临时 worktree 里把链改回旧顺序，同一测试必失败（`重投应先被去重拦下（ErrDuplicate），got middleware: rate limited: scope=user key=u1`）；`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 7.8/10.5/10.6 与 `docs/testing.md`（engine 行 28→29、中间件顺序条目）、`docs/roadmap.md` 的 middleware 行号引用同步。
  - 位置：`internal/engine/engine.go` 的 `globalMiddlewares`（原 257-266）；实现 `internal/middleware/middleware.go` 的 `Dedup`（`set.Add`，现 `:207`/`:218`）与 `RateLimit`（`l.Allow`，现 `:231`/`:249`）
  - 问题：`RateLimit`（`ScopeUser`）在 `Dedup` 外层 → 重投先扣配额再判重；超时后 handler 仍在跑但已“记录已处理”。
  - 修复：调整为 `Recover → Logger → Metrics → Timeout → Recover → Auth → Dedup → RateLimit`；决定并文档化“去重是标记开始还是标记成功”。
  - 验收：新增顺序相关测试；同步 `docs/engine.md` 10.5/10.6。

- [x] **11. `Timeout` 中间件泄漏内层 goroutine** `[已复现]` — 完成 62cd704：Go 无法强杀 goroutine，超时仍是「放弃等待」，但每次未退出的调用现在都被归因：规则超时当刻仍在运行的调用上报新增的 `rec.RuleOverrun(plugin, rule)`（指标族 `kei_rule_overruns_total{plugin,rule}`）并记 Warn（plugin/rule/platform/kind/user/channel/event_id/timeout_ms + 「Handler 必须响应 ctx 取消」提示）；父 ctx 已结束（引擎关闭、`EventTimeout` 到期）不算规则超时、不计入。调用随后返回时另记一条含实际耗时的 Warn；超时后再过 `d` 仍未返回时记一次含全部 goroutine 堆栈的 Error（`sync.Once` 每个 `Timeout` 实例至多一次，避免卡死实现刷爆日志）。观察者 goroutine 生命周期 ≤ `d`（未传日志器时不启动），不会随卡死调用常驻。`middleware.Timeout` 签名改为 `Timeout(d, log *slog.Logger, rec metrics.Recorder)`，`metrics.Recorder` 增加 `RuleOverrun`；`Handler` 契约在 `pkg/bot` 注释与 `docs/plugin.md` 9.2 写明「规则超时后框架不再等待，不响应取消的调用会一直占用 goroutine，框架只能计数与告警」。回归测试 `internal/middleware/middleware_test.go` 的 `TestTimeoutReportsOverrun`（三个子测试：超时未返回 → `overruns==1` + Warn 字段，释放后 → 「超时后返回」Warn 含 duration_ms/error，并断言 goroutine 回落到基线；父 ctx 取消 → `overruns==0` 且错误包装 `context.Canceled`；卡死 → ERROR 含堆栈且堆栈里出现被放弃调用的帧），`TestTimeoutFastHandler` 断言未超时不计数；签名迁移 `TestTimeoutBlocks`/`TestTimeoutDisabled` 与 Logger 的「超时记 ERROR」用例。取证：临时用例（Handler 无视 ctx 阻塞在 `RuleTimeout=30ms` 之后）修复前返回 `ErrTimeout` 后 goroutine base=2→3 且无任何计数/日志入口（旧签名不收日志器与指标），修复后 `overruns=1 labels={plugin:slow-plugin rule:slow-rule}`、Warn + 含被放弃帧的 ERROR 堆栈、释放后回到 base=2；`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿，`TestTimeout*` 与全局链用例 `-count=5` 稳定。文档：`docs/engine.md` 7.8/10.5 + 附注（新增「`Timeout` 无法终止不响应 ctx 的调用」条目）、`README.md` 指标表、`docs/testing.md`（middleware 行 22→23、超时条目）、`docs/roadmap.md` 行号引用同步。
  - 位置：`internal/middleware/middleware.go` 的 `Timeout`（原 164-181，现 `:185` 起）与新增的 `observeOverrun`
  - 问题：handler 不响应 ctx 时每事件泄漏一个 goroutine，并会拖住 `eventbus.Close` 排空；`AGENTS.md` 2.3 要求"goroutine 必须有退出机制"。
  - 修复：至少在超时后统计/告警未退出调用（含插件/规则标识与堆栈）；`Handler` 契约明确"必须响应 ctx"。
  - 验收：测试断言超时后能观测到泄漏计数/告警。
  - 补注：「拖住排空」对引擎装配的链并不成立：`Timeout` 在 `RuleTimeout` 后即返回 `ErrTimeout`，被放弃的调用与观察者都不再占用 eventbus worker，排空只受 `RuleTimeout` 约束；残余足迹就是那个不响应 ctx 的 goroutine 本身（Go 无法回收），只有调用方自行把 `Timeout(0)` 组成链时 Handler 才会同步执行并真正阻塞 worker。

- [x] **12. `Router.UpdateRules` 会改到已发布快照里的活对象** `[已复现]` — 完成 929a912：`UpdateRules` 改为「先在副本上应用并校验、全部通过后再整体发布」——每条规则经 `cloneRule` 复制后交给 `fn`，新 entry 由副本构建，只有全部规则都通过校验才替换 `r.ordered` 与快照；中途失败（`fn` 为 nil 或某条规则 `Handler` 被置空）时已应用的改动一并丢弃，已发布快照里的规则对象不再被触碰。公开面收窄：删除可写的 `Engine.Router()`，改为两个只读视图 `Engine.Rules()`（`Rules()` 深层副本）与 `Engine.RegistrationErrors()`（错误副本，即 `docs/plugin.md` 原先经 `Router()` 审计正则注册失败的路径），引擎不再对外暴露任何注册/改规则的入口；调用方（`internal/engine/engine_test.go` 的白名单断言）已迁移到 `eng.Rules()`。回归测试 `internal/router/router_test.go` 的 `TestUpdateRulesFailureLeavesPublishedRulesUntouched`：`fn` 先给第一条规则写入 `BotIDs`/`Priority` 再把第二条规则 `Handler` 置空 → 断言报错后 `Rules()` 两条规则均为原值、`Dispatch` 仍命中两条规则（行为未变）、随后修正过的 `fn` 仍能正常发布。取证：临时 worktree（HEAD 旧实现）内同一测试必失败——`失败的 UpdateRules 改到了已发布规则: &{ID:p:all:0 Priority:99 … BotIDs:[other-bot]}`；`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 10.3（副本语义 + 不产生半更新）与访问器清单、`docs/plugin.md` 9.2 的审计路径、`docs/testing.md`（router 行 27→28）、`docs/roadmap.md` 测试计数同步。
  - 位置：`internal/router/router.go` 的 `UpdateRules`（原 155-176，现 `:152` 起，循环内 `cloneRule`）；原 `Engine.Router()` 位于 `internal/engine/engine.go`，现由 `Engine.Rules()`（`:488`）与 `Engine.RegistrationErrors()`（`:494`）取代
  - 问题：`fn` 作用于快照内 `*bot.Rule`；中途因 `Handler==nil` 提前返回时，前面的字段已被改且不重建快照 → 与 `docs/engine.md` 10.3"原子替换、不发布半更新状态"不符。
  - 修复：先在 `cloneRule` 副本上应用并校验，再整体换快照；同时评估 `Engine.Router()` 是否应收窄公开面。
  - 验收：测试 `fn` 中途报错后规则字段未变。
  - 补注：`fn` 的入参语义随之改变——不再是「规则表里的原始 `*bot.Rule`」，而是副本；把改动写回路由表只能靠「`UpdateRules` 返回 nil」这一条路径，`Register(rule)` 的入参对象在 `UpdateRules` 之后也不再是活跃规则（活跃对象是副本）。

- [x] **13. `eventbus.Publish` 会阻塞：分片锁内取全局 dedup 锁 + slog I/O** `[已复现]` — 完成 31f5961：三处改动——(1) 去重集合由单个全局 `dedup.Set` 拆成 `Workers` 份，**分片下标由 `ev.ID` 的 FNV-1a 哈希决定**（不是会话键），因此同一 ID 无论会话键是否相同都落在同一集合上、去重语义与单集合完全一致，不同 ID 的投递则分散到不同锁；每片容量 `max(1, DedupCapacity/Workers)`，总量仍受 `DedupCapacity` 约束。(2) 丢弃日志移到 `sh.mu` 之外（计数本来是原子操作），阻塞式 slog handler 只拖慢本次投递。(3) 丢弃日志按分片限流（`dropLogInterval = 1s`，`dropLogAt` 原子时间戳 + CAS，并用 `dropped_total` 字段带出写日志当刻的累计丢弃数），`Stats().Dropped` 仍逐条精确。语义取舍：`Seen` + `Add` 双查保留（`Add` 的返回值继续作为「同一 ID 不同会话并发投递」的兜底），因为「队列满不写去重集、重试可成功」的既有契约要求先探视、成功入队后再落标记。回归测试 `internal/eventbus/bus_test.go`：`TestQueueFullDropLogging`（子测试一：一个限流窗口内 10 条丢弃只记 1 条日志、`Dropped=10`、日志带 `dropped_total`；跨窗口再记 1 条且累计值更新。子测试二：`Handle` 阻塞的 slog handler 不会卡住同一分片的后续投递）与 `TestDedupAcrossShardsByEventID`（同一 ID、两条落在不同分片的事件只处理一次，`Published=1 Duplicated=1`，钉住「按 ID 而非按会话分片去重」的设计）。取证：临时 worktree（HEAD 旧实现）+ 新测试必失败——`限流窗口内只应有 1 条丢弃日志, 实际 10 条`、`阻塞式日志处理器卡住了同一分片的后续投递`；另用临时并行投递基准（30 万次、24 线程、TTL=1ns 放大 `Add` 的清理临界区）取 mutex profile：`dedup.(*Set).Add` 的锁延迟 28.7ms → 4.4ms、`Seen` 0.52ms → 0.06ms（残余的分片锁延迟两个版本相同，容量检查 + 入队本就必须在锁内）。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿，新用例 `-count=3` 稳定。文档：`docs/engine.md` 8.1（`DedupCapacity` 语义）、8.2（锁外日志 + 限流 + 字段）、8.5（按 ID 拆分与每片容量）、`internal/engine` 的 `Options.DedupCapacity` 注释、`docs/testing.md`（eventbus 行 23→25）、`docs/roadmap.md` 行号与计数同步。
  - 位置：`internal/eventbus/bus.go` 的 `Publish`（原 168-194，现 `:171` 起）、新增的 `logDrop`/`dropLogInterval`（`:211` 起）、`shard.dropLogAt`、`Bus.dedups`
  - 问题：所有分片在 `dedup.Set.mu` 上串行（`BusWorkers` 并行度失效）；阻塞式 slog handler 会卡住平台回调线程。与 `bus.go` 的“永不阻塞”与 `docs/engine.md` 8.2 矛盾。
  - 修复：丢弃日志移到锁外并限流；dedup 按分片拆分，或直接用一次 `Add` 判定并接受跨分片去重语义（同时去掉 `Seen`+`Add` 双查）。
  - 验收：`go test -race ./internal/eventbus`；文档同步。
  - 补注：未采用「一次 `Add` 判定」的备选，因为那会把「队列满的事件」也写进去重集，破坏 8.2 明确承诺的「被丢弃的事件不写去重集，调用方重试仍可成功投递」；按 ID 拆分的多个集合已消除全局串行，`Seen`+`Add` 的第二查只在该 ID 跨会话并发投递时生效。

- [x] **14. `Publish` 的“ev 可复用”契约是错的** `[已复现]` — 完成 271a142：采用「复制事件」方案（而非「入队前解析命令 + 文档化 Publish 会写 ev」）：`Publish` 在确定入队那一刻把 `*bot.Event` 浅拷贝为 `owned` 并入队 `&owned`，此后队列、去重集、worker、处理器与 `Router.Dispatch` 的 `ev.Command` 回写都只接触副本，调用方在 `Publish`/`Emit` 返回后即可立即复用事件对象（池化、覆写字段），不再与后台读取竞态。复制只在「确定入队」的路径上发生，去重命中与队列满不产生额外分配；浅拷贝的边界（`Message`/`Sender`/`Channel` 等被引用对象仍与副本共享，投递后不得再修改）写进 `Publish` 注释、`pkg/bot` 的 `EventSink.Emit` 契约、`docs/adapter.md` 6.1 与 `docs/engine.md` 8.2（含「旧实现为何错误」的一句）。未选「入队前解析命令」的原因：那只消除 `Dispatch` 写 `ev.Command` 这一处写，worker 仍会读调用方对象（`Rule.Matches`/`Text`/日志），复用依旧竞态，无法满足「`-race` 下池化事件测试通过」。回归测试 `internal/eventbus/bus_test.go` 的 `TestPublishEventOwnership`：子测试一「投递后立即覆写不影响处理」——处理器先等调用方覆写完（gate），再断言收到的不是调用方对象（指针不等）且 `ID`/文本仍是投递时的值；子测试二「并发复用不产生竞态」——订阅者读取字段的同时调用方投递后立即整体覆写并改写 ID（队列满视为可接受结果，不掩盖竞态判定）。取证：临时 worktree（HEAD 旧实现）+ 同一测试——子测试一必失败 `处理器拿到的仍是调用方的事件对象，Publish 未复制`，子测试二在 `-race` 下必报 `WARNING: DATA RACE`（`callHandler` 读 vs 测试覆写同一地址）；修复后两者均通过，`-count=5 -race` 稳定。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 8.2（所有权段落 + 顺序里的「复制事件并入队」）、`docs/adapter.md` 6.1、`pkg/bot/adapter.go`、`docs/testing.md`（eventbus 行 25→26）、`docs/roadmap.md` 计数同步。
  - 位置：`internal/eventbus/bus.go` 的 `Publish`（注释原 `:158-160`，现 `:171-175`；复制在 `set.Add` 成功之后、`sh.q <- &owned`）
  - 问题：bus 保留同一 `*bot.Event` 指针，worker 稍后写字段；任何池化事件的适配器都会与之竞态。
  - 修复：二选一——在入队前解析命令（并如实文档化 Publish 会写 ev），或 dispatch 前拷贝事件；最低限度改正 `bus.go:160` 与 `docs/engine.md:289`。
  - 验收：`-race` 下池化事件测试通过；文档同步。
  - 补注：复制引入「每入队一条事件一次浅拷贝分配」，与 `hasherPool` 那类优化方向相反，但这是「调用方可复用」能做到零竞态的最低成本；如需彻底免分配，只能把所有权明确交给总线（调用方不得复用），不采纳的原因是适配器的池化解码是很自然的写法。

- [x] **15. `Rule.Matches`/`Match` panic 会垮掉该事件的其余规则** `[已复现]` — 完成 b9f750a：`Dispatch` 改经新增的 `(*Router).match` 逐条匹配，`defer recover` 把匹配阶段的 panic 收敛到单条规则——该规则被跳过，其余规则照常匹配与执行；panic 记 `Error` 日志（`plugin`/`rule`/`platform`/`type`/`event_id`/`panic`/`stack`，与 `middleware.Recover` 同风格），计入新增的 `rec.RuleMatchPanic(plugin, rule)`（指标族 `kei_rule_match_panics_total{plugin,rule}`，`metrics.Recorder` 与 `Registry` 同步扩展、引擎经 `router.Options.Metrics` 注入 `e.met`），错误以 `router: 规则匹配 panic: 规则 <id>: <panic>` 包装新导出的 `router.ErrMatchPanic` 进入 `Dispatch` 的 `errors.Join` 聚合返回值（调用方可用 `errors.Is` 判定）。回归测试 `internal/router/router_test.go` 的 `TestMatchPanicIsIsolated`：三条规则按优先级 `[first(100), boom(50, WithMatch panic), last(0)]`，断言 panic 规则被跳过而 `first`/`last` 都执行（`命中顺序 == [first last]`）、聚合错误 `errors.Is(err, ErrMatchPanic)` 且含 panic 值与规则 ID、`matchPanicRecorder` 恰好上报一次 `RuleMatchPanic{b, b:all:0}`、Error 日志含 `规则匹配 panic`/`plugin=b`/`匹配炸弹`/`stack=`。取证：临时 worktree（HEAD 旧实现）内等价用例——`panic 冒泡出 Dispatch: 匹配炸弹`、`命中顺序 = [first]`（`last` 被中止），且旧路径下该 panic 由事件总线 `callHandler` 按「处理器 panic」兜底（日志只有 `event_id`/`session_key`/`panic`，无 `rule` 标识）并把整个事件计为 `Failed`；修复后同一场景按规则归因并继续执行其余规则。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 7.8（匹配 panic 不在 Handler 保护范围内，指向 10.3）、10.3（按规则隔离语义 + 错误文案 + 与旧行为对比）、10.5（指标族与语义）、`docs/plugin.md` 9.2（`WithMatch` 的 panic 行为）、`README.md` 指标表、`docs/testing.md`（router 行 28→29、panic 必测项）、`docs/roadmap.md` 计数同步。
  - 位置：`internal/router/router.go` 的 `Dispatch`（原 `:197` 的裸 `e.rule.Matches(ev)`，现 `:205` 经 `r.match`）；新增 `ErrMatchPanic`（`:24`）与 `match`（`:244`）；兜底日志 `internal/eventbus/bus.go` 的 `callHandler`
  - 问题：插件提供的 `Match` panic 被 bus 按“处理器 panic”捕获并中止剩余规则，日志无 rule 标识 → 跨插件爆炸半径。
  - 修复：`Dispatch` 内对每条规则匹配单独 recover、跳过并带 `plugin`/`rule` 记录与计数。
  - 验收：测试一个 Match panic 不影响其它规则命中。
  - 补注：匹配 panic 不进入 `Result`/`EventHandled` 指标（handler 根本没被调用），因此单独用 `kei_rule_match_panics_total` 计数；`Errors` 与 `Rules()` 之类的只读视图不变。

- [x] **16. onebot：`flexID` 接受任意 JSON token，`Event.ID` 可空/可撞** `[读码]` — 完成 4e97f7b：`flexID.UnmarshalJSON` 末分支改为按 `json.Number` 校验（对象/数组/布尔一律报错：HTTP 上报回 400，反向 WS 丢弃并记 warn；错误里回显的 token 用 `truncate` 截到 `errorBodyLimit`），不再把 `{}` 之类原样存成 ID。回退 ID 保持既有字段顺序，在其后追加 `target_id`、`message_id`、`flag`；消息事件缺 `message_id` 时再附加 `message` 原文（含引号）的 FNV-1a 64 哈希，重复上报仍得到相同 ID。回归测试 `adapters/onebot/onebot_test.go` 的 `TestNonScalarIDsRejected`（6 种非标量 token → 400，随后合法事件证明被拒事件未进 sink）与 `TestFallbackIDsDistinguishSameSecondEvents`（同秒不同内容的消息/不同 `message_id` 的撤回/不同 `target_id` 的戳一戳/不同 `flag` 的请求四组 ID 不撞，且重复上报稳定）；在 `HEAD` worktree 跑同一测试必失败（204 而非 400、ID 为 `"{}"`、四组回退 ID 全撞）。取证：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/adapter.md` 6.6 第 10 条与 `docs/testing.md` 的 onebot 行（40→42）同步。
  - 位置：`adapters/onebot/event.go:74-90`（末分支原样存 token）、`:178-182`（秒级兜底拼接）
  - 问题：`message_id` 非标量时 ID 变成 `{}` 之类；为空时回落 `self_id-post_type-time-…`，同秒同会话的不同消息撞 ID 被去重丢弃，违反 `AGENTS.md` 2.3“Event.ID 稳定且唯一”。
  - 修复：末分支校验为合法数字否则报错（HTTP 400）；兜底 ID 加入 message/子事件唯一信息（如 `NoticeType`+`TargetID`+`MessageID`），并保证稳定。
  - 验收：单测覆盖非标量 `message_id` 报错与同秒不同消息 ID 不同。

- [x] **17. onebot：`Capabilities.File=true` 但 URL-only 文件段发不出去** `[读码]` — 完成 760403e：采用第二条修复方案（保留 `File: true`，在 `buildArray` 按段数据兜底）。`SegFile` 分支改为：`KeyFile` 非空 → 按 `file` 段发送（`file`/`url`/`name` 沿用原行为）；只有 `KeyURL` → 经新增的 `fileFallbackText` 渲染成与 `bot.Degrade` 同格式的文本链接 `[file] 名称 URL`（名称为空时省略）；两个键都为空 → 跳过并记 warn（对齐 `docs/domain-model.md` 5.6 约定 3「与 `KeyURL` 至少一个非空」、两者皆空即不可发送）。回归测试 `adapters/onebot/onebot_test.go` 的 `TestSendFileSegments`（经 `newFakeAPI` 断言实际 HTTP API 载荷）四条：`message.File` 的 URL-only 产物 → 文本链接、无文件名 → `[file] URL`、带 `KeyFile`（同时带 `KeyURL`/`KeyFileName`）→ 仍发 `file` 段、两键皆空 → 段被跳过；在 `HEAD` worktree 跑同一测试必失败（URL-only 两条发出 `{"type":"file","data":{"file":"http://…"}}`，空数据条目发出 `{"file":""}` 且不跳过）。取证：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/adapter.md` 6.4 第 1 条与 6.7 onebot 能力条目、`README.md` 适配器能力表、`docs/testing.md` onebot 行（42→43）同步。
  - 位置：`adapters/onebot/onebot.go:311`、`adapters/onebot/message.go:147-151`；构造器 `pkg/message/message.go:55-57`
  - 问题：`message.File(url,name)` 只给 `KeyURL`，OneBot `file` 字段要求 `file://`/本地路径 → 发送失败，且能力位为 true 使 `bot.Degrade` 不做文本兜底。
  - 修复：`File:false`（让引擎降级为文本），或仅在有 `KeyFile` 时发文件、否则回退文本链接。
  - 验收：单测 URL-only 文件段按声明行为处理。

- [x] **18. onebot：`Stop` 分阶段共用/重建预算，可能超时** `[静态]` — 完成 1d802b5：`Stop` 顶部以 `context.WithTimeout(ctx, shutdownTimeout)`（5s）定一个总 deadline，阶段顺序改为**先 `http.Server.Shutdown`**（独立子预算 `httpShutdownTimeout` = 2s，不再继承被 WebSocket 阶段耗尽的 ctx）**再收尾反向 WebSocket**（用总预算剩余时间）；两阶段错误经 `errors.Join` 并报，不再用 http 错误掩盖 WebSocket 错误。`Start` 退栈与 `Stop` 经新增的 `Adapter.shutdownWS`（`sync.Once`）共享同一次 WebSocket 收尾，`Start` 退栈在 `Stop` 已（或正在）被调用时经 `stopped()` 直接跳过，一次启停不再叠加两份等待预算；`hub.shutdown` 的硬关宽限提为具名常量 `wsCloseGrace`（1s）。回归测试 `adapters/onebot/reverse_test.go` 的 `TestStopBoundedWithStuckSession`（读循环卡在 `Emit`——既不响应关闭帧也不响应硬关连接时，`Stop(context.Background())` 必须在 `shutdownTimeout + wsCloseGrace` + 1s 内返回，错误须指出「反向 WebSocket 连接退出超时」且不含「关闭事件服务失败」，并断言监听端口已释放）与 `TestStartCleanupWithoutStop`（不调用 `Stop`、直接 cancel ctx：`Start` 退栈后连接数归零且返回 nil，守住新的退栈分支）；在 `HEAD` worktree 跑同一测试必失败（旧实现 `Stop` 永不返回，用例 7s 后报「卡住的 WebSocket 会话让 Stop 超出预算未返回」）。取证：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/adapter.md` 6.6 第 7 条与 `docs/testing.md` onebot 行（43→45）同步。
  - 位置：`adapters/onebot/onebot.go:498-519`、`Start` defer `:395-405`、`reverse.go:207-237`
  - 问题：`hub.shutdown` 先耗尽 ctx 预算（失败再等 1s），之后 `srv.Shutdown` 继承已耗尽 ctx；`Start` defer 又用新的 5s 预算，单次启停可叠加。
  - 修复：`Stop` 顶部定一个 deadline，`srv.Shutdown` 先于 `hub.shutdown`，各阶段显式子预算；`Start` defer 复用有界 ctx。
  - 验收：测试卡住的 WS 会话下 `Stop` 在预算内返回。

- [ ] **25. mock：`started` 置位早于 listen；`kind` 不校验** `[静态]`
  - 位置：`adapters/mock/mock.go:110-127`、`:349-355`、`:305-309`
  - 问题：`net.Listen` 失败后 `started==true`、`sink` 已设 → 实例自称健康且永久不可重试；`/inject` 未知 `kind` 静默变群聊，测试可能验证了错误的代码路径。
  - 修复：成功 listen 后再置 `started`/`sink`；失败路径复位状态；`kind` 只接受闭集，否则 400。
  - 验收：单测 listen 失败后 `Started()==false` 且可重试；非法 kind 返回 400。

- [ ] **26. mock：控制面无鉴权且未限制 loopback** `[静态]`
  - 位置：`adapters/mock/mock.go:327-336`、`:373-385`；权限声明 `adapters/mock/register.go`
  - 问题：`listen_addr: 0.0.0.0:18080` 即把事件注入（可触发命令执行）与 `DELETE /sent` 暴露到网络。
  - 修复：`New` 拒绝非 loopback 地址（除非显式 opt-in），或复用 onebot 的 token 机制。
  - 验收：单测非 loopback 地址被拒绝。

- [x] **27. ratelimit：桶表无上限 + 空 key 共享一个桶** `[读码]` — 完成 d7de6f3：桶表加容量上限 `maxKeys`（`New` 第 4 个参数，`<= 0` 取 `defaultMaxKeys` = 10000），用 `container/list` 维护 O(1) 的 LRU（`order` 头部最久未使用）：插入新 key 且已达上限时 `evictLocked` 淘汰头部桶，任何访问（含被拒绝的调用）都刷新位置，空闲回收同步摘除链表元素；被淘汰的 key 下次出现重建为满桶。`RateLimit` 改为经新增的 `limitKey` 取键：各 scope 的身份为空时统一回退 `SessionKey()`（无 `Sender` 的 notice/meta 因此按会话/机器人分桶，不再共用一个空键桶），`SessionKey()` 也为空（事件为 nil）时透传而不是挤在 `key=""` 桶上。回归测试：`internal/ratelimit` 的 `TestBucketTableBounded`（100 个不同 key、`maxKeys=4` 时桶数恰为 4；默认上限同覆盖）与 `TestEvictionKeepsMostRecentlyUsed`（刚访问过的 key 保留已耗尽的令牌、被淘汰的 key 重建为满桶）、`internal/middleware` 的 `TestRateLimitScope` 新增 `user-fallback-session`（同会话共享配额、不同 bot 互不影响）与 `empty-key-passthrough` 子测试。取证：`HEAD` worktree 探针必失败——100 个不同 key 时 `Buckets = 100, 期望 4（上限）`，无 `Sender` 事件跨 bot 撞车报 `rate limited: scope=user key=`。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 7.7 的令牌桶说明（maxKeys/LRU）与 10.5 的 `RateLimit` 行、作用域常量段、7.x 处理侧限流段、`docs/testing.md` 的 ratelimit 行（7→9）与 middleware 行同步。
  - 位置：`internal/ratelimit/ratelimit.go:59-89`、`:116-124`；`internal/middleware/middleware.go:221-236`、`:319-325`
  - 问题：GC 只按 10min 空闲且仅在 `AllowN` 内触发，无容量上限；无 Sender 的事件（OneBot notice/meta）全部落在 `key=""` 同一桶。
  - 修复：加 `maxKeys` + LRU/SWR 或定期 janitor；`RateLimit` 对空 key 选择“放行”或回退 `SessionKey()`，并写入文档。
  - 验收：单测大量不同 key 后桶数有界；空 key 行为确定。

- [x] **28. ratelimit：`retryAfter` 用过时 token 估算，系统性多睡** `[读码]` — 完成 269a85c：`retryAfter` 改为**只读且自洽**的估算——先按当前时间把令牌补到 `now`（`min(burst, tokens+elapsed*rate)`，不修改桶状态、不刷新 LRU），再算 `max(0, (1-tokens)/rate)`，并补 `nil`/`rate <= 0` 守卫；`Wait` 的时序不变（仍先 `Allow` 再估算）。回归测试 `internal/ratelimit` 的 `TestRetryAfterUsesFreshTokens`（假时钟：速率 1/s 的桶耗尽 500ms 后应剩约 500ms、补满后为 0、未知 key 为 0）与 `TestWaitDoesNotOversleep`（验收项：rate=20/s、burst=1 连续 3 次 `Wait` 的实际耗时须 < 1.5×速率推导值 150ms）。**实测修正原判断**：同一组用例在 `HEAD` worktree 上——陈旧实现报 `retryAfter = 1s, 期望约 500ms`（缺陷坐实：该函数只在「调用方刚刚调用过 `Allow`」时才准），而 `TestWaitDoesNotOversleep` 在旧实现上同样通过；另对 `Wait` 实测（顺序 20 次、并发 8 次，rate=200/100）耗时为理论下限的 1.06–1.10×（定时器与调度开销），即 `Wait` 自身并未系统性多睡，原条目「发送路径多睡」的用户可见结论不成立，真实缺陷是 `retryAfter` 的隐式调用顺序依赖。取证：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/engine.md` 7.7 的 `Wait`/`retryAfter` 语义、`docs/testing.md` 的 ratelimit 行（9→11）同步。
  - 位置：`internal/ratelimit/wait.go:38-47`
  - 问题：`b.tokens` 仅在 `AllowN` 内补充，`Wait` 循环里读到的值已陈旧 → 等待被高估；发送路径（单 key）受影响最明显。
  - 修复：读取前先按现在时间补充，再计算 `max(0,(1-tokens)/rate)`。
  - 验收：单测 `Wait` 的实际等待不显著超过速率上限推导值。

- [ ] **29. metrics：直方图抓取可自相矛盾** `[读码]`
  - 位置：`internal/metrics/metrics.go:98-108`（`observe`）、`:111-121`（`snapshot`）
  - 问题：先逐桶 `Load`，再锁内读 `total`；并发写入下可导出 `bucket{le="+Inf"} < 前一桶`，破坏 `histogram_quantile`/`rate`。
  - 修复：同一临界区内完成快照（或在 `total` 读取后再修正 +Inf）。
  - 验收：并发观测下断言各桶单调不减。

- [ ] **30. metrics：重复事件从不计入 `dropped`** `[读码]`
  - 位置：`internal/engine/engine.go:503-512`（先记 `published`）、`internal/eventbus/bus.go:176-179`（去重返回 nil）
  - 问题：`published - dropped` 高估入队量，`kei_events_dropped_total` 看不到 duplicate，而 `Bus.Stats().Duplicated` 有值。
  - 修复：让 `Publish` 区分“入队/去重”，或在 `Emit` 侧对去重记 `reason="duplicate"`。
  - 验收：单测重复事件后指标计数正确。

- [ ] **31. metrics：`platform` 标签基数无界** `[读码]`
  - 位置：`internal/metrics/registry.go:66-71`、`:107-121`；来源 `ev.Platform`（mock 可从请求体指定，`adapters/mock/mock.go:355-358`）
  - 问题：未校验的标签值可为每请求新增序列，Registry 内存无界。
  - 修复：按已注册适配器名归一化，未知平台并入 `reason="unknown_platform"`。
  - 验收：单测未知平台不新增序列。

- [x] **32. 存储：无命名空间隔离、`key` 长度上限未校验** `[静态]` — 完成 24522a9：采用「前缀化」方案。新增 `internal/storage.PluginScoped`/`AdapterScoped`（`scoped` 包装器，读/写/删统一加 `plugin:<插件名>:` / `adapter:<botID>:` 前缀），在 `internal/engine.pluginAPI`、`internal/pluginmgr` 的 `PluginContext.Storage`、`internal/adaptermgr` 三处分发点包裹；插件/适配器按原样传键，同名键不再互相覆盖，同平台多实例也互不可见。键长规则收进共享层：`internal/storage.MaxKeyBytes` = 191、`ErrKeyTooLong`（可 `errors.Is`）与 `CheckKey`（错误里回显的键截断到 64 字节），`internal/storage.Memory` 与 `internal/storage/sqlstore` 的 Get/Set/Delete 在动状态前统一校验（`Entry.Key` 的 `size:191` 注释同步），三个内置后端因此返回同一个错误；`pkg/bot` 的 `Storage`/`PluginContext.Storage`/`AdapterContext.Storage` 注释补充键长与隔离语义。回归测试：`internal/storage` 的 `TestKeyLengthLimit`（191 字节可写、192 字节的 Get/Set/Delete 都返回 `ErrKeyTooLong`）与 `TestScopedStorageIsolatesKeys`（插件/适配器两种视图：同名键互不覆盖、底层键带前缀、删除只影响自身），`internal/storage/sqlite` 的 `TestSQLiteKeyLengthLimit`（与内存实现一致），`internal/engine` 的 `TestPluginAPIPermissionGates/同名键按插件隔离`，`internal/pluginmgr` 的权限上下文用例改为断言 `plugin:p:k` 布局，`internal/adaptermgr` 新增 `TestBuildNamespacesStorage`（两个 bot 实例）。取证：`HEAD` worktree 探针必失败——内存/SQLite 接受 192 字节键（「与 MySQL 行为不一致」/「只有 MySQL 会拒绝」）、两插件同名键读回 `"b"`（被覆盖）、两实例读回 `"second"`、pluginmgr 报 `存储未生效: key not found`。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/plugin.md` 11.2 与上下文表、`docs/adapter.md`（6.3 权限表、6.6 依赖裁剪、`AdapterContext` 片段）、`docs/configuration.md` 12.5、`README.md`（插件/适配器权限表与已知限制）、`docs/testing.md`（storage 9→11、sqlite 5→6、adaptermgr 18→19、engine/pluginmgr 行）同步。
  - 位置：`internal/engine/pluginapi.go:26-29`、`pkg/kei/kei.go`（同一 store 传 `adaptermgr.Build`）、`internal/storage/sqlstore/sqlstore.go:22-23/77/92/107`
  - 问题：插件/适配器共用扁平键空间，同名 key 静默互覆；`size:191` 只在 MySQL 生效，超长 key 在 memory/SQLite 可用、MySQL 报错。
  - 修复：在 `pluginAPI`/`adaptermgr` 前缀化（`plugin:<name>:`、`adapter:<name>:`）或明确写死共享契约；`sqlstore.Set/Get/Delete` 校验 191 字节并返回清晰错误。
  - 验收：单测两插件同名 key 互不影响；超长 key 报错一致。

- [x] **33. storage：`params` 无校验、`cleanup_interval` 语义跨后端不一致** `[静态]` — 完成 fb054a7：`StorageConfig.validate` 新增已知参数校验——`cleanup_interval`/`conn_max_lifetime` 经新增的 `decodeDurationValue`、`max_open_conns`/`max_idle_conns` 经 `decodeIntValue`（接受形态与 `Config.Duration`/`Config.Int` 一致，浮点必须是整数值，负数拒绝，`nil` 等同未配置），错误路径为 `storage.params.<key>`，在环境变量覆盖**之后**校验因此 `KEI_STORAGE_PARAMS_*` 注入的非法值同样被拦；无调用者的 `decodeDurationNode` 删除，由值版本取代。未知键继续原样透传（第三方后端扩展空间，内置后端忽略；拼错键名仍不报错，已在 `docs/configuration.md` 12.5 写明键表与适用范围）。memory 后端改为读取 `cleanup_interval`：新增 `NewMemoryWithCleanup(interval)`（`interval <= 0` 不启动后台清理，默认 `defaultCleanupInterval` = 1m），`NewMemory()` 保持 1 分钟默认，注册表 opener 读取该配置键，与 SQL 后端同义。回归测试：`internal/config` 的 `TestStorageValidateErrors` 扩为表驱动（非时长/负数时长、非整数/负数/小数计数、环境变量注入的非法值、合法边界 `0` 与秒数、未知键透传），`internal/storage` 新增 `TestMemoryCleanupInterval`（自定义间隔清理过期条目、`0` 关闭后台清理且 `Len` 保留、经 `storage.Open("memory", …)` 读取该键）；在 `HEAD` worktree 跑同一组探针必失败——四类非法参数 `err = <nil>`（未被拦下）、`Open("memory", cleanup_interval=20ms)` 过期条目 2s 内未被清理。实机冒烟：`cmd/bot` 配 `cleanup_interval: -5s` 启动即以 `config: storage.params.cleanup_interval: 不能为负数（0 表示关闭后台清理）` 退出（exit 1），合法配置（sqlite + `cleanup_interval: 0` + MySQL 专属池键）正常启动并优雅退出。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/configuration.md` 12.5（参数表 + 校验语义 + 未知键说明）、`docs/plugin.md` 11.2 内存实现条目、`README.md` 存储条目、`docs/testing.md`（storage 11→12、config 行）同步。
  - 位置：`internal/storage/sqlstore/sqlstore.go:152-159`、`internal/storage/memory.go:36-40`、`:112-114`、`internal/config/config.go:638-645`、`internal/config/config.go:473-497`（无调用者的 `decodeDurationNode`）
  - 问题：`max_open_conns` 拼错/非数字 → `0` = 无限；`cleanup_interval: -5s` 静默关 janitor、`abc` 静默回默认；memory 后端忽略该键且硬编码 1m，与 `docs/configuration.md` 12.5 的“通用参数”表述不符。
  - 修复：在配置层校验已知键与类型（复用/删除 `decodeDurationNode`），memory 后端尊重 `cleanup_interval`，后端专属键要么实现要么明确文档范围。
  - 验收：非法 params 启动报错；文档同步。

- [x] **34. 注册表同构只做了一半：插件缺 `Options` 校验、`PermAll` 无约束、插件名不校验** `[读码]` — 完成 65adc3c：三条规则全部镜像到插件侧，门设在 `internal/pluginmgr.Manager.Add`（装配期报错，与适配器的 `adaptermgr` 注册表校验对应）。(1) **名字**：新增 `pluginNamePattern`（`^[a-z0-9_-]+$`），元信息名越界即报错；同名规则也加到配置层——`internal/config` 的 `adapterNamePattern`/`storageTypePattern` 两个重复正则合并为单一 `namePattern`，`plugins.<name>` 与 `bots[].adapter`/`adapters.<name>`/`storage.type` 共用，报错路径 `config: plugins.<name>: 插件名只允许 [a-z0-9_-]`。(2) **`PermAll`**：采用「拒绝」而非豁免——元信息声明 `*` 时 `Add` 报 `pluginmgr: 插件 %s 不得声明 * 权限（需逐项声明）`（与适配器 `adaptermgr` 的拒绝一致；原注释「仅内置插件可用」从未被强制，且豁免机制在 internal 包里没有真实使用者，属未接线旋钮），`pkg/bot` 的 `PermAll` 注释与文档改为「插件与适配器都不得声明，`HasPermission` 的 `*` 展开仅对绕过管理器的 SDK 用法有效」。(3) **`Options`**：`bot.Metadata` 新增 `Options []string`（镜像 `AdapterMetadata.Options`），`Add` 对 `plugins.<name>` 下未声明的私有键记 warn `插件不认识的配置键`（字段 `plugin`/`key`，`Options` 为空时静默跳过），与 `adaptermgr.warnUnknownOptions` 同义。回归测试：`internal/pluginmgr` 新增 `TestAddValidatesMetadata`（合法名通过；大写/点/空格名报 `[a-z0-9_-]`；`PermAll` 单独或混在逐项权限里都报「不得声明」；被拒插件不入管理器）与 `TestAddWarnsUnknownOptions`（声明 `Options` 后 typo 键告警、已知键不告警、未声明 `Options` 时静默），`TestPermissionScopedContext` 的「全部权限」用例改为显式权限列表以继续覆盖依赖裁剪；`internal/config` 的 `TestValidateErrors` 增补「插件名非法字符」用例。取证：`HEAD` worktree 探针必失败——`Add("my.plugin")` 与 `Add(PermAll)` 都返回 `err = <nil>`，且 `internal/pluginmgr`/`internal/engine` 中没有「不认识的配置键」路径、`pkg/bot/plugin.go` 无 `Options` 字段。启动冒烟（临时用例）：`kei.Run` 遇 `plugins."bad.name"` 与注入声明 `PermAll` 的 `FuncPlugin` 都在启动期报错。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/plugin.md` 9.1（Metadata 块、元信息校验段、`PermAll` 语义、nil 接收者句）、11.4 权限表 `*` 行、`docs/adapter.md` 6.2 字符集说明与 6.3 对比表、6.6 注册表校验条目、`docs/configuration.md` 12.3、`docs/testing.md`（pluginmgr 6→8、config 行）同步。
  - 位置：`pkg/bot/plugin.go:25-44`、`:67-68`、`:76-86`；`internal/pluginmgr/manager.go:112-116`、`217-226`；`internal/config/config.go:613-617`；对照适配器 `internal/adaptermgr/adaptermgr.go:119-120`、`249-261`、`config.go:36`
  - 问题：第三方插件声明 `Permissions: [*]` 即拿到真实 Storage/HTTPClient，而注释称“仅内置插件可用”；插件私有配置键无未知键告警；插件名允许任意字符，`KEI_PLUGINS_*` 规范化与白名单会失配。
  - 修复：镜像适配器规则（名字 `[a-z0-9_-]`、`Options` 未知键告警、拒绝或显式豁免 `PermAll`）。
  - 验收：单测三条规则各自生效；`docs/plugin.md`、`docs/adapter.md` 6.3 同步。

- [x] **35. `bot.Config` 直接别名调用方的活 map** `[读码]` — 完成 0cde095：采用「双向隔离」——`NewConfig` 深拷贝入参、`Raw()` 返回每次新建的深层副本（两个方案都做，因为只做其一仍会留下一个方向的别名：`NewConfig` 别名源 map 时引擎持有的 `config.Config` 仍可被插件经 `Get`/嵌套容器改写）。新增 `cloneConfigMap`/`cloneConfigValue`，覆盖配置取值域内会共享引用的容器类型：`map[string]any`（递归）、`[]any`（递归）、`[]string`、`[]byte`、`map[string]string`，nil 容器保持 nil（不改变各取值方法对 nil 的判定），其它类型按值返回。隔离效果覆盖三个调用点：`internal/adaptermgr` 传 `bots[].Settings`、`internal/engine` 传 `plugins.<name>.Settings`、`pluginmgr.Deps.Configs` —— 现在每个 `Config` 独占自己的 map，插件/适配器改写不会回到 `config.Config`，配置之间也不共享可变值。`Get` 对嵌套路径仍返回配置内部的值（同一 `Config` 独占，只有它自己的视图会变），已在文档写明。回归测试 `pkg/bot/api_test.go` 的 `TestConfigIsolatedFromCaller`：对每种容器双向验证（改源 map 不影响配置、改 `Raw()` 副本不影响配置、二次 `Raw()` 仍是干净副本、同一来源构造的两个配置互不影响）。取证：`HEAD` worktree 探针必失败——`Raw() 是活引用，源 map 被改成 hacked`；另用两 goroutine（一个经 `Raw()` 写、一个经 `String` 读）在 `HEAD` 上跑 `-race` 得到 `WARNING: DATA RACE`，修复后同一探针 `-race` 干净。冒烟（临时用例）：`adaptermgr.Build` 后适配器经 `Raw()` 改写配置不回写 `config.Config`，调用方之后改源 map（含嵌套）也不影响适配器持有的配置。`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿。文档：`docs/plugin.md` 11.3（Config API 块 + 上下文表行 + 深拷贝边界段）、`docs/configuration.md` 12.3、`docs/adapter.md` 6.5、`README.md` 插件上下文表、`docs/testing.md`（pkg/bot 17→18）同步。
  - 位置：`pkg/bot/api.go:83-95`；调用方 `internal/adaptermgr/adaptermgr.go:192-196`、`internal/engine/engine.go:200-203`
  - 问题：`Raw()` 返回活引用，插件改它即改 `config.Config` 的共享对象，跨插件无锁并发可致竞态。
  - 修复：`NewConfig` 深拷贝（或 `Raw()` 返回快照）并文档化。
  - 验收：单测改 `Raw()` 不影响原 map。

- [x] **36. `pkg/bot` 注释与实现不符** `[读码]` — 完成 16ec7c6：四处注释按实现改写，无行为变更。`OnRegex` 改为「编译失败不会 panic、不注册该规则，错误由实现记录（内置实现记入注册错误列表并记 Warn 日志），插件 Setup 不受影响」（实现：`internal/router/registrar.go` 的 `recordErr` → `Router.RegistrationErrors()`，`router.go:340` 记 Warn）；`Use` 改为「只影响调用之后注册的规则，已注册的规则不受影响」（实现：`registrar.add` 注册时复制当前中间件列表）；`Reply.Image` 改为「参数为图片 URL，写入 `Segment.Data[KeyURL]`，不支持平台文件标识」；`Adapter` 头部删除「并把平台调用统一为 Send/SendRequest」，改为「上行事件经 Start 注入的 EventSink 投递，下行发送统一为 Send(*SendRequest)」。未改 `Setup` 行为：`RegistrationErrors` 是既有且已文档化的非致命通道，改 `Setup` 报错会变更装配语义，超出注释一致性范围（`AGENTS.md` 7：文档与代码冲突以代码为准）。`docs/plugin.md` 同步：9.2 / 9.3 的镜像注释，原「与注释不一致处」段改为陈述实际失败通道。取证：`gofmt -l .`（无输出）、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿（23 包）。
  - 位置：`pkg/bot/registrar.go:101`（`OnRegex` 说 Setup 返回错误，实际只进 `RegistrationErrors`）、`:109`（`Use` 说影响所有规则，实际只影响之后注册的）、`pkg/bot/reply.go:15`（`Image` 说可用平台文件标识，实际只写 `KeyURL`）、`pkg/bot/adapter.go:9`（说 Adapter 有 `SendRequest`，实为 Engine）
  - 问题：`AGENTS.md` 2.4“注释与实现不符视为缺陷”，且这些是公开 SDK 契约。
  - 修复：按现实改写注释；`OnRegex` 若要真报错需改 `Setup` 行为并同步 `docs/plugin.md` 9.2。

- [x] **37. `Reply.Send` 写入的 `msg.ID` 是死字段** `[读码]` — 完成 e918d3c：采用「追加 `SegReply`」实现引用意图。`internal/reply.Builder.Send` 在事件消息带 ID 时于段列表首位插入 `SegReply{KeyMessageID: ev.Message.ID}`（按次构建、不写入已累积段列表，重复 `Send` 不累积）；`Message.ID` 的死赋值删除，`pkg/bot` 的 `Message.ID` 注释改为「入站消息由适配器填充；构造待发消息时留空，平台分配的消息 ID 见 `SendResult.MessageID`」。选择段方案而非 `SendRequest.ReplyTo`：`ReplyTo` 目前只有示例适配器消费，内置 onebot 只序列化 `SegReply`，且 `Builder` 只持有 `bot.BotAPI`（无 ReplyTo 入口）；降级路径天然生效——`Capabilities.Reply` 为 false 时 `bot.Degrade` 丢弃引用段、其余段照常发送。回归测试：`internal/reply` 的 `TestBuilderAutoQuote`（无消息 ID 不追加、重复 Send 不累积、引用段不进入 `Segments()`）与更新后的 `TestBuilderSendsAccumulatedSegments`/`TestBuilderErrors`（带 ID 的空消息仍不发送）；`internal/engine` 的 `TestReplyQuotesTriggeringMessage`（端到端段首位引用 `m-77`）与 `TestReplyQuoteDroppedByCapability`（仅文本能力的适配器丢弃引用段仍发文本）。取证：新测试在 `HEAD` worktree 内必失败（`Message.ID = "in-1"`、段数 1≠2），修复后全绿；`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿（23 包）。文档：`docs/plugin.md` 9.3、`docs/engine.md` 10.3、`docs/domain-model.md` 5.3、`pkg/bot/reply.go` 契约注释、`docs/testing.md`（reply 3→4、engine 29→31）。
  - 位置：`internal/reply/reply.go:89-91`、`docs/plugin.md:220`；消费方 `adapters/onebot/message.go`
  - 问题：注释称“用于平台侧引用回复”，但无任何适配器读待发 `Message.ID`（onebot 只序列化 `SegReply`）→ 引用回复静默失效。
  - 修复：实现意图（`Builder.Send` 设 `SendRequest.ReplyTo` 或追加 `SegReply`），或删除赋值并改文档。

- [x] **38. 注册器静默陷阱：空参数退化成兜底、`WithPlatforms`/`WithBotIDs` 空参语义相反、`Keywords` 不拷贝** `[读码]` — 完成 2bfb46b：三个陷阱全部落地，无兼容垫片。(1) **空触发参数拒绝**：`internal/router` 的 `OnCommand("")`/`OnRegex("")`/`OnKeyword`（空列表或含空词）/`OnEvent("")` 在注册期 `recordErr` 后直接返回（不注册、不占 ID 序号），错误文案 `router: 插件 %q 的命令名/正则表达式/关键词列表/第 N 个关键词/事件类型为空`；空正则一并纳入（`regexp` 空表达式是事实上的全匹配，属同类陷阱）；兜底必须显式用 `OnAll`。`pkg/bot` 的 `RecordingRegistrar` 镜像同一校验（新增 `recordErr` 助手，错误进 `Errors()`、空参数不落规则），插件单测与生产语义一致。(2) **空参语义统一**：方向取 `Rule.BotIDs` 既有约定（引擎用非 nil 空切片禁用规则，`applyBotPluginFilter` 依赖该语义）——`Rule.Matches` 的 Platforms 判定由 `len > 0` 改为 `!= nil`，`WithPlatforms` 改用 `append([]string{}, platforms...)`：`nil` 不过滤、非 nil 空切片（即 `WithPlatforms()`）任何平台都命中不了；已核查全部调用点，无生产代码依赖旧的「空 = 不过滤」。(3) **Keywords 复制**：`OnKeyword` 注册时 `slices.Clone(words)`，注册后修改调用方切片（改值/追加）不再改变路由。回归测试（修复前全红）：`internal/router` 的 `TestRegistrarRejectsEmptyTriggers`（表驱动 5 例，含「空参数不得成为兜底」断言）、`TestWithPlatformsEmptyMatchesNothing`、`TestKeywordsCopiedAtRegistration`；`pkg/bot` 的 `TestRecordingRegistrarRejectsEmptyTriggers`、`TestRecordingRegistrarCopiesKeywords` 与 `TestRuleMatches` 新增 nil/空切片两行。取证：同一批新测试在 `HEAD` worktree 内失败（空参数注册出规则、`WithPlatforms()` 命中 `[none all]`、改词后旧词不命中 `Matches()=true, want false`），修复后全绿；`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全绿（23 包）。文档：`docs/plugin.md` 9.2（入口契约、失败通道段、Option 注释与 nil/空切片语义）、`docs/engine.md` 10.1/10.3、`pkg/bot` 契约注释、`docs/testing.md`（router 29→32、pkg/bot 18→20）。
  - 位置：`internal/router/registrar.go:35-80`、`:62`；`pkg/bot/registrar.go:76-81`、`:123-136`、`:155-175`
  - 问题：`OnCommand("")`/`OnKeyword(nil)`/`OnEvent("")` 变成消息/全局兜底，`OnKeyword([]string{""})` 是死规则；`WithPlatforms()` 不过滤而 `WithBotIDs()` 谁都不命中；注册后修改 `words` 会改变路由。
  - 修复：注册期拒绝空触发参数并记入 `RegistrationErrors`；统一空参语义并文档化；`slices.Clone(words)`。
  - 验收：单测空参数报错/语义一致；`pkg/bot/registrar.go` 与 `docs/plugin.md` 9.2 同步。

- [ ] **39. `ErrPermissionDenied` 未在 `pkg/bot` 导出** `[静态]`
  - 位置：`internal/storage/denied.go:12-14`；`pkg/bot/api.go:17-18` 只导出 `ErrNotFound`；`docs/plugin.md` 11.2 自认该洞
  - 修复：在 `pkg/bot` 定义并让 `internal/storage` 包装它，文档给出 `errors.Is` 用法。
  - 验收：仅依赖 `pkg/bot` 的插件可 `errors.Is(err, bot.ErrPermissionDenied)`。

- [ ] **40. `Reply` 能力缺口 + `pkg/message` 缺 `KeyFile` 构造器** `[读码]`
  - 位置：`pkg/bot/reply.go:9-22`（无 `File`/`Face`/引号，`Send` 不返回 `SendResult`）、`pkg/message/message.go:55-57`（无 `FileID`/`KeyFile`）
  - 问题：`Reply` 比 `pkg/message` 与其自身接口都弱；`SegFile` 契约允许 `KeyFile` 或 `KeyURL`，但公开构造器只能表达后者。
  - 修复：以不破坏外部实现的方式扩展（新增能力放可选接口或具体构建器）；补 `FileID`/`ImageID`。

---

## P2 — 结构 / 一致性 / 文档

- [ ] **41. `pkg/kei` 空导入存储方言，嵌入方被迫吃下 GORM + cgo** `[读码]`
  - 位置：`pkg/kei/kei.go:29-31`；对照 `docs/architecture.md` 3.1“不空导入任何适配器或插件”
  - 问题：只要 import `pkg/kei` 就链接 `gorm` + `mattn/go-sqlite3`（需 `CGO_ENABLED=1`），即便用 `memory` 或注入 `Options.Storage`；存储后端不像适配器/插件那样由调用方 opt-in，与“注册表 + 配置装配”原则不一致。
  - 修复：把空导入移到 `cmd/bot`，或提供 `pkg/kei/sql` opt-in 包；文档如实描述。

- [ ] **42. 引擎可调参数在唯一装配路径上不可达** `[读码]`
  - 位置：`pkg/kei/kei.go:119-127`；`internal/engine/engine.go:76-95`；`docs/plugins/manage.md:59`（要求用户用 `CommandPrefixes` 覆盖前缀）
  - 问题：`CommandPrefixes`/`EventTimeout`/`RuleTimeout`/`ShutdownTimeout`/`BusWorkers`/`BusQueueSize`/`DedupTTL`/`DedupCapacity`/`SendRetries`/`SendBackoff` 全部取零值 = 只能吃默认；`cmd/bot` 更只能传 `ConfigFile`。
  - 修复：在 `kei.Options` 和/或配置 `engine:` 段暴露并透传；至少补 `CommandPrefixes`、`DedupTTL`、`SendRetries`。
  - 验收：`!manage ping` 可通过配置跑通；`docs/engine.md` 7.x 与 `docs/configuration.md` 同步。

- [ ] **43. 适配器重复实现抽取到共享层** `[读码]`
  - 位置：`firstNonEmpty` ×4（`adapters/onebot/message.go:213`、`pkg/bot/degrade.go:87`、`internal/engine/logging.go:122`、`examples/kei-adapter-myim/myim.go:511`）；`dataString` ×3 语义不同（`adapters/onebot/message.go:183` 走 `jsonString`，`internal/engine/logging.go:116` 只认 `string`，`examples/kei-adapter-myim/myim.go` 又写一套）；`truncate` ×3
  - 问题：同一 `bot.Segment` 在不同适配器上取值行为不同；违反“禁止并存第二套约定”。
  - 修复：把 `Segment.Data` 取值约定收进 `pkg/bot`（紧邻 `Key*` 常量），适配器只保留平台特有逻辑。
  - 验收：新增测试断言两种取值在 onebot 与示例适配器上一致。

- [ ] **44. 入站 HTTP 事件源的服务器超时未按需配置** `[读码]`
  - 位置：`adapters/onebot/onebot.go:414-420`（完整四项，但与事件源的实际风险面不一定匹配）
  - 问题：入站 HTTP 事件源目前只剩 OneBot 一处，但服务器超时默认值、body 上限与 ACK/丢弃/排空语义没有共享实现；后续新增 HTTP 回调型适配器（如企业内部 IM）时会再次各写一套。
  - 修复：抽 `internal/adapterkit`：body 上限读取、鉴权钩子、ACK 策略、有界队列 + worker、带 deadline 排空、服务器超时默认值；适配器只提供 auth/parse/convert。
  - 验收：OneBot 迁移到共享实现，现有行为测试保持通过；`docs/adapter.md` 同步。

- [ ] **45. `docs/adapter.md` 与 `AdapterContext.Config` 注释过时** `[静态]`
  - 位置：`docs/adapter.md:384` vs `pkg/bot/adapter_registry.go:29`
  - 修复：删除该括注（`enabled` 已在注释里）。

- [ ] **46. 配置要求 `bots>=1`，与“只运行插件”能力矛盾** `[读码]`
  - 位置：`internal/config/config.go:593-595` vs `pkg/kei/kei.go:106-108`
  - 问题：`kei.Run` 明确支持无适配器只跑插件，且 bot 可全部 `enabled: false`，但校验阶段强制至少一个 bot。
  - 修复：允许空 `bots`（或仅要求“存在 enabled 的 bot 时字段合法”），同步 `docs/configuration.md` 12.2/12.8。

- [ ] **47. sqlstore 错误被吞且无日志器** `[静态]`
  - 位置：`internal/storage/sqlstore/sqlstore.go:86`（惰性删除 `_ =`）、`:137-139`（janitor `_ =`）
  - 问题：清理长期失败（权限/方言/连接）时表无限增长且无任何信号，`Get` 仍返回 `ErrNotFound` 无法区分原因。
  - 修复：注入可选 `*slog.Logger`（或从 `Close` 返回最后错误）并 Warn/Debug 记录。
  - 验收：单测失败删除路径有日志。

- [ ] **48. 手写 YAML 段重复键静默后者胜** `[静态]`
  - 位置：`internal/config/config.go:216-236`、`:276-336`、`:341-436`（`bots`/`plugins`/`adapters`/`storage` 手写遍历无重复键检查；struct 解码路径会报 `already set`）
  - 问题：合并冲突/复制粘贴的配置静默取最后值，无诊断。
  - 修复：统一的 pair 遍历助手，重复键报错并给字段路径。

---

## 附：处理顺序建议

1. P0 全部（1-9）：静默丢事件/丢错误/后端不可用，优先。
2. P1 契约类（10-15、34、38）：中间件顺序、Publish 契约、注册器语义。
3. P1 适配器类（16-18、25-26）：按 onebot → mock。
4. P1 基础设施（27-33、35-37、39-40）。
5. P2 结构与文档（41-48）。

> 注：编号 4、19-24 原为飞书适配器缺陷；飞书适配器已从仓库移除，这些条目随之删除，编号保留空位以维持其余条目的稳定性。

> 注：第 1、2 条已在本地用临时用例复现（结论：`Seen(X)==false`；`order` 由 4 涨到 20001），已落地为 `internal/dedup` 的回归测试 `TestStaleOrderSlotDoesNotEvictReAddedID`、`TestOrderStaysBoundedUnderReAdd`（修复前均失败，修复后通过）。
