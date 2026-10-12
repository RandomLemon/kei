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

- [ ] **4. 飞书：先 ACK 成功后丢事件** `[读码]`
  - 位置：`adapters/feishu/feishu.go:375-377`（ACK 先于 enqueue）、`:315-321`（满队列 `default` 丢弃）
  - 问题：队列 1024、worker 2；满时飞书收到 `code:0` 不再重试 → 事件永久丢失，仅一条 warn。违反 `AGENTS.md` 2.3 的“快速 ACK + 异步投递”本意（ACK 必须代表已受理）。
  - 修复：`enqueue` 返回 bool；失败时回非 0 code（或 503）让飞书重试，`writeJSON` 移到 enqueue 之后。
  - 验收：单测构造满队列，断言返回非 2xx / 非 0 code。

- [ ] **5. MySQL 后端：`key` 是保留字，Get/Delete 语法错误** `[读码]`
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

- [ ] **10. 全局中间件顺序：重复事件先消耗限流额度、Dedup 落在 Timeout 的 detached goroutine 内** `[读码]`
  - 位置：`internal/engine/engine.go:257-266`；实现 `internal/middleware/middleware.go:234`（`Allow`）与 `:203`（`Add`）
  - 问题：`RateLimit`（`ScopeUser`）在 `Dedup` 外层 → 重投先扣配额再判重；超时后 handler 仍在跑但已“记录已处理”。
  - 修复：调整为 `Recover → Logger → Metrics → Timeout → Recover → Auth → Dedup → RateLimit`；决定并文档化“去重是标记开始还是标记成功”。
  - 验收：新增顺序相关测试；同步 `docs/engine.md` 10.5/10.6。

- [ ] **11. `Timeout` 中间件泄漏内层 goroutine** `[读码]`
  - 位置：`internal/middleware/middleware.go:164-181`
  - 问题：handler 不响应 ctx 时每事件泄漏一个 goroutine，并会拖住 `eventbus.Close` 排空；`AGENTS.md` 2.3 要求“goroutine 必须有退出机制”。
  - 修复：至少在超时后统计/告警未退出调用（含插件/规则标识与堆栈）；`Handler` 契约明确“必须响应 ctx”。
  - 验收：测试断言超时后能观测到泄漏计数/告警。

- [ ] **12. `Router.UpdateRules` 会改到已发布快照里的活对象** `[读码]`
  - 位置：`internal/router/router.go:155-176`；`Engine.Router()` 公开于 `internal/engine/engine.go:428`
  - 问题：`fn` 作用于快照内 `*bot.Rule`；中途因 `Handler==nil` 提前返回时，前面的字段已被改且不重建快照 → 与 `docs/engine.md` 10.3“原子替换、不发布半更新状态”不符。
  - 修复：先在 `cloneRule` 副本上应用并校验，再整体换快照；同时评估 `Engine.Router()` 是否应收窄公开面。
  - 验收：测试 `fn` 中途报错后规则字段未变。

- [ ] **13. `eventbus.Publish` 会阻塞：分片锁内取全局 dedup 锁 + slog I/O** `[已复现]`
  - 位置：`internal/eventbus/bus.go:168-194`（`sh.mu` 内调用 `dedup.Seen/Add` 与 `logger.Warn`）
  - 问题：所有分片在 `dedup.Set.mu` 上串行（`BusWorkers` 并行度失效）；阻塞式 slog handler 会卡住平台回调线程。与 `bus.go:158-159`、`docs/engine.md:289`“永不阻塞”矛盾。
  - 修复：丢弃日志移到锁外并限流；dedup 按分片拆分，或直接用一次 `Add` 判定并接受跨分片去重语义（同时去掉 `Seen`+`Add` 双查）。
  - 验收：`go test -race ./internal/eventbus`；文档同步。

- [ ] **14. `Publish` 的“ev 可复用”契约是错的** `[读码]`
  - 位置：`internal/eventbus/bus.go:158-160`（注释）vs `internal/router/router.go:190-192`（worker 写 `ev.Command`）
  - 问题：bus 保留同一 `*bot.Event` 指针，worker 稍后写字段；任何池化事件的适配器都会与之竞态。
  - 修复：二选一——在入队前解析命令（并如实文档化 Publish 会写 ev），或 dispatch 前拷贝事件；最低限度改正 `bus.go:160` 与 `docs/engine.md:289`。
  - 验收：`-race` 下池化事件测试通过；文档同步。

- [ ] **15. `Rule.Matches`/`Match` panic 会垮掉该事件的其余规则** `[读码]`
  - 位置：`internal/router/router.go:197`（匹配在中间件链之前）；兜底 `internal/eventbus/bus.go:310-321`
  - 问题：插件提供的 `Match` panic 被 bus 按“处理器 panic”捕获并中止剩余规则，日志无 rule 标识 → 跨插件爆炸半径。
  - 修复：`Dispatch` 内对每条规则匹配单独 recover、跳过并带 `plugin`/`rule` 记录与计数。
  - 验收：测试一个 Match panic 不影响其它规则命中。

- [ ] **16. onebot：`flexID` 接受任意 JSON token，`Event.ID` 可空/可撞** `[读码]`
  - 位置：`adapters/onebot/event.go:74-90`（末分支原样存 token）、`:178-182`（秒级兜底拼接）
  - 问题：`message_id` 非标量时 ID 变成 `{}` 之类；为空时回落 `self_id-post_type-time-…`，同秒同会话的不同消息撞 ID 被去重丢弃，违反 `AGENTS.md` 2.3“Event.ID 稳定且唯一”。
  - 修复：末分支校验为合法数字否则报错（HTTP 400）；兜底 ID 加入 message/子事件唯一信息（如 `NoticeType`+`TargetID`+`MessageID`），并保证稳定。
  - 验收：单测覆盖非标量 `message_id` 报错与同秒不同消息 ID 不同。

- [ ] **17. onebot：`Capabilities.File=true` 但 URL-only 文件段发不出去** `[读码]`
  - 位置：`adapters/onebot/onebot.go:311`、`adapters/onebot/message.go:147-151`；构造器 `pkg/message/message.go:55-57`
  - 问题：`message.File(url,name)` 只给 `KeyURL`，OneBot `file` 字段要求 `file://`/本地路径 → 发送失败，且能力位为 true 使 `bot.Degrade` 不做文本兜底。Feishu 反而只在有 `KeyFile` 时发文件。
  - 修复：`File:false`（让引擎降级为文本），或仅在有 `KeyFile` 时发文件、否则回退文本链接。
  - 验收：单测 URL-only 文件段按声明行为处理。

- [ ] **18. onebot：`Stop` 分阶段共用/重建预算，可能超时** `[静态]`
  - 位置：`adapters/onebot/onebot.go:498-519`、`Start` defer `:395-405`、`reverse.go:207-237`
  - 问题：`hub.shutdown` 先耗尽 ctx 预算（失败再等 1s），之后 `srv.Shutdown` 继承已耗尽 ctx；`Start` defer 又用新的 5s 预算，单次启停可叠加。
  - 修复：`Stop` 顶部定一个 deadline，`srv.Shutdown` 先于 `hub.shutdown`，各阶段显式子预算；`Start` defer 复用有界 ctx。
  - 验收：测试卡住的 WS 会话下 `Stop` 在预算内返回。

- [ ] **19. 飞书：`Stop` 不等待 `Serve` goroutine、不清 `addr`** `[静态]`
  - 位置：`adapters/feishu/feishu.go:248-278`、`Addr()` 注释 `:108-116`
  - 问题：`Start`/`Stop` 由引擎不同 goroutine 调用；`Stop` 返回后 `Addr()` 仍可能非空、`started` 仍可能为 true，与注释“已停止时为空串”不符。
  - 修复：用 done channel 与 Serve goroutine 同步，并在 `Stop` 内加锁清理 `addr`/`started`；或改写注释为弱保证。
  - 验收：单测 `Stop` 后 `Addr()==""`。

- [ ] **20. 飞书：排空循环无 deadline 且 `default` 会漏事件** `[读码]`
  - 位置：`adapters/feishu/feishu.go:289-312`
  - 问题：`default` 在 channel 瞬时为空即返回，之后入队的事件无人消费；`drainCtx` 只传给 `Emit`，循环本身无计时 → 既超预算又丢事件。`docs/adapter.md` 声称“5s 排空预算”。
  - 修复：循环内加 `case <-drainCtx.Done()`；排空前先关闭入队（置 draining 标志），或排空至 deadline 并统计残留。
  - 验收：单测慢 sink + 并发 enqueue，断言残留事件被计数/告警。

- [ ] **21. 飞书：多 chunk 发送 + 引擎重试 = 重复投递** `[静态]`
  - 位置：`adapters/feishu/send.go:141-148`（顺序发 N 段，仅返回最后 ID）、重试 `internal/engine/send.go`（默认 2 次）
  - 问题：第 3 段失败 → 引擎重试整条 → 前两段重复；错误也无“已发部分”信息。
  - 修复：为每段带 Lark `uuid` 去重；或返回结构化错误让引擎不重试多段消息。
  - 验收：单测模拟第 N 段失败，断言无重复段。

- [ ] **22. 飞书：token 刷新在锁内做网络调用、失败不清缓存** `[静态]`
  - 位置：`adapters/feishu/send.go:438-458`、`:481-483`
  - 问题：`fetchToken` 在 `tokRefresh` 内 → 慢/挂起的 token 端点串行化整个发送路径；鉴权失败后旧 token 仍留在缓存直到 margin 到期，不会自愈。
  - 修复：网络调用移出锁（发布时二次检查），鉴权类错误清 `tok`/`tokExpire`。
  - 验收：单测并发 Send + 挂起 token 端点，断言不超过一次 fetch 且失败后能重取。

- [ ] **23. 飞书：`parsePost` 随机选 locale** `[读码]`
  - 位置：`adapters/feishu/event.go:366-374`（map 首个键 `break`）
  - 问题：多 locale 的 post 文本随机，关键词规则随投递不同而命中不同。
  - 修复：按固定优先级（如 `zh_cn`→`en_us`→字典序首键）取值。
  - 验收：单测多 locale 输入结果确定。

- [ ] **24. 飞书：`Capabilities.Markdown=true` 但按纯文本发送** `[读码]`
  - 位置：`adapters/feishu/feishu.go:163`、`adapters/feishu/send.go:199-203`；`pkg/bot/adapter.go:80`（注释“支持 Markdown 富文本”）
  - 问题：能力位语义（此处仅表示“不降级为文本”）与字段名/注释承诺不一致，插件 `Reply.Markdown` 得到字面 `**bold**`。
  - 修复：置 `Markdown:false` 让引擎降级为文本（线上结果相同、声明诚实），或真正用 `lark_md` 卡片发送；同时校正 `Capabilities` 注释与 `docs/adapter.md`。
  - 验收：单测按所选语义断言。

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

- [ ] **27. ratelimit：桶表无上限 + 空 key 共享一个桶** `[读码]`
  - 位置：`internal/ratelimit/ratelimit.go:59-89`、`:116-124`；`internal/middleware/middleware.go:221-236`、`:319-325`
  - 问题：GC 只按 10min 空闲且仅在 `AllowN` 内触发，无容量上限；无 Sender 的事件（OneBot notice/meta）全部落在 `key=""` 同一桶。
  - 修复：加 `maxKeys` + LRU/SWR 或定期 janitor；`RateLimit` 对空 key 选择“放行”或回退 `SessionKey()`，并写入文档。
  - 验收：单测大量不同 key 后桶数有界；空 key 行为确定。

- [ ] **28. ratelimit：`retryAfter` 用过时 token 估算，系统性多睡** `[读码]`
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

- [ ] **32. 存储：无命名空间隔离、`key` 长度上限未校验** `[静态]`
  - 位置：`internal/engine/pluginapi.go:26-29`、`pkg/kei/kei.go`（同一 store 传 `adaptermgr.Build`）、`internal/storage/sqlstore/sqlstore.go:22-23/77/92/107`
  - 问题：插件/适配器共用扁平键空间，同名 key 静默互覆；`size:191` 只在 MySQL 生效，超长 key 在 memory/SQLite 可用、MySQL 报错。
  - 修复：在 `pluginAPI`/`adaptermgr` 前缀化（`plugin:<name>:`、`adapter:<name>:`）或明确写死共享契约；`sqlstore.Set/Get/Delete` 校验 191 字节并返回清晰错误。
  - 验收：单测两插件同名 key 互不影响；超长 key 报错一致。

- [ ] **33. storage：`params` 无校验、`cleanup_interval` 语义跨后端不一致** `[静态]`
  - 位置：`internal/storage/sqlstore/sqlstore.go:152-159`、`internal/storage/memory.go:36-40`、`:112-114`、`internal/config/config.go:638-645`、`internal/config/config.go:473-497`（无调用者的 `decodeDurationNode`）
  - 问题：`max_open_conns` 拼错/非数字 → `0` = 无限；`cleanup_interval: -5s` 静默关 janitor、`abc` 静默回默认；memory 后端忽略该键且硬编码 1m，与 `docs/configuration.md` 12.5 的“通用参数”表述不符。
  - 修复：在配置层校验已知键与类型（复用/删除 `decodeDurationNode`），memory 后端尊重 `cleanup_interval`，后端专属键要么实现要么明确文档范围。
  - 验收：非法 params 启动报错；文档同步。

- [ ] **34. 注册表同构只做了一半：插件缺 `Options` 校验、`PermAll` 无约束、插件名不校验** `[读码]`
  - 位置：`pkg/bot/plugin.go:25-44`、`:67-68`、`:76-86`；`internal/pluginmgr/manager.go:112-116`、`217-226`；`internal/config/config.go:613-617`；对照适配器 `internal/adaptermgr/adaptermgr.go:119-120`、`249-261`、`config.go:36`
  - 问题：第三方插件声明 `Permissions: [*]` 即拿到真实 Storage/HTTPClient，而注释称“仅内置插件可用”；插件私有配置键无未知键告警；插件名允许任意字符，`KEI_PLUGINS_*` 规范化与白名单会失配。
  - 修复：镜像适配器规则（名字 `[a-z0-9_-]`、`Options` 未知键告警、拒绝或显式豁免 `PermAll`）。
  - 验收：单测三条规则各自生效；`docs/plugin.md`、`docs/adapter.md` 6.3 同步。

- [ ] **35. `bot.Config` 直接别名调用方的活 map** `[读码]`
  - 位置：`pkg/bot/api.go:83-95`；调用方 `internal/adaptermgr/adaptermgr.go:192-196`、`internal/engine/engine.go:200-203`
  - 问题：`Raw()` 返回活引用，插件改它即改 `config.Config` 的共享对象，跨插件无锁并发可致竞态。
  - 修复：`NewConfig` 深拷贝（或 `Raw()` 返回快照）并文档化。
  - 验收：单测改 `Raw()` 不影响原 map。

- [ ] **36. `pkg/bot` 注释与实现不符** `[读码]`
  - 位置：`pkg/bot/registrar.go:101`（`OnRegex` 说 Setup 返回错误，实际只进 `RegistrationErrors`）、`:109`（`Use` 说影响所有规则，实际只影响之后注册的）、`pkg/bot/reply.go:15`（`Image` 说可用平台文件标识，实际只写 `KeyURL`）、`pkg/bot/adapter.go:9`（说 Adapter 有 `SendRequest`，实为 Engine）
  - 问题：`AGENTS.md` 2.4“注释与实现不符视为缺陷”，且这些是公开 SDK 契约。
  - 修复：按现实改写注释；`OnRegex` 若要真报错需改 `Setup` 行为并同步 `docs/plugin.md` 9.2。

- [ ] **37. `Reply.Send` 写入的 `msg.ID` 是死字段** `[读码]`
  - 位置：`internal/reply/reply.go:89-91`、`docs/plugin.md:220`；消费方 `adapters/feishu/send.go:124-127/151-158`、`adapters/onebot/message.go`
  - 问题：注释称“用于平台侧引用回复”，但无任何适配器读待发 `Message.ID`（feishu 走 `req.ReplyTo`/`SegReply`，onebot 只序列化 `SegReply`）→ 引用回复静默失效。
  - 修复：实现意图（`Builder.Send` 设 `SendRequest.ReplyTo` 或追加 `SegReply`），或删除赋值并改文档。

- [ ] **38. 注册器静默陷阱：空参数退化成兜底、`WithPlatforms`/`WithBotIDs` 空参语义相反、`Keywords` 不拷贝** `[读码]`
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
  - 位置：`firstNonEmpty` ×5（`adapters/feishu/event.go:426`、`adapters/onebot/message.go:213`、`pkg/bot/degrade.go:87`、`internal/engine/logging.go:122`、`examples/kei-adapter-myim/myim.go:511`）；`dataString` ×3 语义不同（`adapters/onebot/message.go:183` 走 `jsonString`，`adapters/feishu/send.go:531`、`internal/engine/logging.go:116` 只认 `string`）；`truncate` ×3
  - 问题：同一 `bot.Segment` 在不同适配器上取值行为不同（数字型 `@` 目标在 feishu 静默丢）；违反“禁止并存第二套约定”。
  - 修复：把 `Segment.Data` 取值约定收进 `pkg/bot`（紧邻 `Key*` 常量），适配器只保留平台特有逻辑。
  - 验收：新增测试断言两种取值在 feishu/onebot 上一致。

- [ ] **44. 入站 HTTP 事件源模式两份且已分叉** `[读码]`
  - 位置：`adapters/feishu/feishu.go:197-217`（仅 `ReadHeaderTimeout`）、`adapters/onebot/onebot.go:414-420`（完整四项）；ACK/排空策略亦不同
  - 问题：慢速 POST 可长期占用飞书连接；ACK/丢弃/排空语义各写一套。
  - 修复：抽 `internal/adapterkit`：body 上限读取、鉴权钩子、ACK 策略、有界队列 + worker、带 deadline 排空、服务器超时默认值；适配器只提供 auth/parse/convert。
  - 验收：两适配器共用同一实现；`docs/adapter.md` 同步。

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
3. P1 适配器类（16-26）：按 feishu → onebot → mock。
4. P1 基础设施（27-33、35-37、39-40）。
5. P2 结构与文档（41-48）。

> 注：第 1、2 条已在本地用临时用例复现（结论：`Seen(X)==false`；`order` 由 4 涨到 20001），已落地为 `internal/dedup` 的回归测试 `TestStaleOrderSlotDoesNotEvictReAddedID`、`TestOrderStaysBoundedUnderReAdd`（修复前均失败，修复后通过）。
