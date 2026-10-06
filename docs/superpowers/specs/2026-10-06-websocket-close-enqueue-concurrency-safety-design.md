# PR #4 — WebSocket Close / Enqueue Concurrency Safety

- 日期：2026-10-06
- 状态：Design spec（Approach B 已批准；implementation 尚未开始）
- 基线：`main` @ `8dbcb974f8999102ed10c47c4995db49f67c9061`
- Feature branch：`codex/ws-close-safety`
- 范围：本文锁定 WebSocket enqueue 与 terminal close 的并发安全设计；不包含 implementation plan、生产代码或测试代码。

## 1. Goal

让 WebSocket session 的 enqueue 与 terminal close 在同一个 session-local synchronization boundary 上线性化，确保任何合法并发顺序都不会向已关闭的 `Send` channel 发送。

保留当前队列容量、nonblocking backpressure、单一普通 writer、topic routing、authorization、WebSocket event/wire behavior 和共享 cleanup 语义。本 PR 只提供可供当前 lifecycle 与后续 PR #5 复用的 safe close primitive。

## 2. Confirmed Race

当前竞态已由源码静态确认；本轮没有运行测试或进行 runtime 复现：

```text
publisher: manager.FindDeliveries() 返回 session pointer，manager lock 已释放
publisher: enqueue 读取 Closed == false
publisher: 暂停
closer:    closeSession 设置 Closed、unregister、close(Send)
publisher: 恢复并向已关闭的 Send 发送
runtime:   panic: send on closed channel
```

`Closed.Load()` 与后续 channel send 不是一个原子操作。`sync.Once` 只保证 cleanup body 执行一次；manager lock 只保护 registry，并且在 publisher 使用 session pointer 前已经释放。因此这些机制都没有把 send 与 channel close 线性化。

主要源码：`internal/services/ws_realtime_types.go` 的 `ClientSession.enqueue`、`internal/services/ws_service.go` 的 `closeSession` 和 `PublishToTopics`、`internal/services/ws_connection_manager.go` 的 `FindDeliveries`。

## 3. Current Concurrency Model

Dashboard employee WS、employee notification WS 与 customer/open WS 共用 `upgradeConnection`、`ClientSession`、`readPump`、`writePump` 和 `closeSession`。`upgradeConnection` 创建容量为 64 的 `Send` channel，注册 session，再启动两个 pump。Publisher 通过 manager 获取 session pointer 快照，之后在 manager lock 外调用 `enqueue`。

当前 lifecycle 相关字段：

| Field | Current owner and access |
|---|---|
| `Send` | `upgradeConnection` 创建；生产者通过 `enqueue` 发送；`writePump` 接收；`closeSession` 是唯一生产 close site。 |
| `Closed` | atomic flag；`enqueue` 读取、`closeSession` 写入。该 flag 本身不保护 channel send。 |
| `closeOnce` | `closeSession` 的多个调用者共享；只运行一个 cleanup body。 |
| `Conn` | upgrade 后保持同一 pointer；`readPump` 是普通 reader；`writePump` 是普通 writer；`closeSession` 调用 `Conn.Close()`。 |
| `ID`、`Principal`、`External`、`CustomerID`、`Role`、`TerminalType` | 注册前初始化；WebSocket lifecycle 注册后只读。 |
| `Topics` | manager 在自身 mutex 下修改；`topicList()` 的读取不在该 mutex 下。这个独立问题记录在第 18 节，不纳入本 PR。 |
| `LastActiveAt` | 通过 `atomic.Int64` 更新。 |

当前没有 session-local mutex、close-request channel 或 context。应用层普通 Gorilla writes (`WriteMessage`、`SetWriteDeadline`) 只在 `writePump`。read side 使用 Gorilla 默认 ping/close handlers 时会触发 control writes；Gorilla 允许 `WriteControl` 与其他 connection methods 并发。当前没有普通 concurrent-writer 违规。

## 4. Approved Architecture Decision

采用已批准的 **Approach B — Linearized Enqueue / Close State**。

`enqueue` 和 terminal transition 必须使用同一个 session-local mutex。`Send` 保持为 closable channel；外部代码不得绕开该 mutex 发送或关闭它。`closeOnce` 继续作为共享 idempotent finalizer。

不增加 `done` / `closeRequest` channel，不重构 writer，也不改 manager snapshot API。复核 teardown 与 inbound subscribe 的并发后，`WsConnectionManager.Subscribe` 需要一个窄的 terminal-session admission guard；原因与顺序见第 8 节。这不是 manager redesign。

## 5. ClientSession Synchronization Model

实现时 `ClientSession` 使用以下确切 lifecycle 字段：

```go
sendMu    sync.Mutex
Send      chan []byte
Closed    atomic.Bool
closeOnce sync.Once
```

`sendMu` 是 enqueue 与 channel close 唯一共享的同步边界。`Closed` 保留，避免改变现有字段语义；其 `Store(true)` 在 `sendMu` 保护下执行。它可用于观察 terminal 状态，但任何 `Closed.Load()` 都不能单独授权 channel send。

不新增第二个 terminal boolean，避免两个状态源不一致。`sendMu` 使用零值初始化，session 只以 pointer 传递，不复制已使用的 mutex。不得将 `Send` 的生产发送或关闭移出约定方法。

## 6. Enqueue Linearization

`ClientSession.enqueue(payload)` 在 `sendMu` 下执行：

1. 获取 `sendMu`。
2. 检查 `Closed.Load()`；若为 true，返回 false，不写入 channel。
3. 使用当前 nonblocking `select` 尝试 `Send <- payload`。
4. send 成功返回 true；`default` 返回 false。
5. 释放 `sendMu`。

成功的 channel send 是 enqueue 的线性化点。若队列已满，enqueue 在线性化边界内返回 false，不等待容量，也不改变 terminal 状态。

## 7. Terminal Close Linearization

`closeSession(session)` 保留 `session.closeOnce.Do(...)` 作为唯一 cleanup body。body 执行以下状态转换：

1. 获取 `session.sendMu`。
2. 执行 `session.Closed.Store(true)`，作为 terminal close 的线性化点。
3. 执行 `close(session.Send)`。
4. 释放 `session.sendMu`。
5. 调用 `manager.Unregister(session)`。
6. 调用 `session.Conn.Close()`（若 connection 非 nil）。
7. 执行现有断开日志。

channel close 与 `Closed.Store(true)` 位于同一临界区。没有 enqueue 能在 close 后通过该边界写入 `Send`。manager 与网络操作均不得放在该临界区内。

## 8. Teardown Ordering

```text
closeOnce.Do enters
→ acquire session.sendMu
→ Closed.Store(true)                 [terminal close linearization point]
→ close(Send)
→ release session.sendMu
→ manager.Unregister(session)
→ Conn.Close()
→ writePump exits
→ blocked readPump returns from ReadMessage and exits
```

该顺序满足以下要求：

1. **不会在 terminal close 后 enqueue：** send 与状态检查在锁内；close 也必须取得该锁。先取得锁的操作先线性化。
2. **stale manager snapshot 安全：** unregister 前已复制 session pointer 的 publisher 仍可调用 `enqueue`，但会在 mutex 下观察 `Closed == true` 并返回 false。
3. **队列满不拖延 close：** enqueue 使用 `select/default`，在持锁期间不会等待队列容量。
4. **manager lock 顺序安全：** close 释放 `sendMu` 后才获取 manager mutex；publisher 的 `FindDeliveries` 释放 manager `RLock` 后才调用 enqueue。两把锁不嵌套，不形成反向 lock order。
5. **writer 退出：** `Send` 关闭后，`writePump` 最终观察到关闭 channel 并返回；connection close 也会令正在进行的写操作失败退出。
6. **reader 退出：** `Conn.Close()` 关闭底层 network connection，令阻塞的 `ReadMessage` 返回 error，随后 read pump 的 defer 调用同一 finalizer。
7. **重复 close harmless：** 所有 read pump、write pump、publisher 或未来 server-side caller 都进入同一个 `closeOnce`；channel close、unregister 和 connection close body 只运行一次。

不得持有 `sendMu` 调用 manager、socket I/O、日志或其他可能阻塞的 teardown 操作。

### Manager subscribe 与 close 的顺序

源码还确认 read pump 可在另一 goroutine 已开始 close 时处理一条已读入的 subscribe frame。若 finalizer 先 `Unregister`，随后 `manager.Subscribe` 才取得 manager mutex，当前实现可能把 terminal session 再放回 topic map。因此 `WsConnectionManager.Subscribe` 必须在持有 manager mutex 时先检查 `session.Closed.Load()`；若为 true，不修改任何 registry/topic map 并返回空结果。

该 guard 与 `sendMu` 不嵌套：close 先在 `sendMu` 下 Store closed 并释放它，再调用 `Unregister`。若 Subscribe 在 terminal Store 前已于 manager mutex 下观察 open 并开始注册，Unregister 随后取得该 mutex 并移除它；若 Subscribe 在 Store 后才进入，则 guard 拒绝注册。这样保留“manager operation 在 lifecycle mutex 外”的要求，并保证 close 完成后不会由迟到的 subscribe 留下 registry entry。`Register` 在 pumps 启动前执行，不需要此 guard。该窄变化不改变仍存活 session 的 topic subscription behavior。

## 9. Stale Snapshot Safety

manager 继续允许 publisher 在 lock 下复制 `(session pointer, delivery topic)` 并在 lock 外完成 publication。unregister 不需要也不能撤销已返回的 pointer。

有效顺序包括：

```text
publisher obtains session pointer
→ closeSession linearizes terminal state and closes Send
→ publisher calls enqueue(stale pointer)
→ enqueue returns false without channel send
```

不允许通过延长 manager lock 覆盖 marshal、authorization、enqueue 或 socket work 来规避 stale pointer。唯一的 manager-side addition 是第 8 节的 closed-session `Subscribe` admission guard；它用于避免迟到的 inbound subscribe 在 unregister 后复活 terminal session。

## 10. Queue / Backpressure Semantics

- `Send` capacity 保持 64。
- enqueue 继续使用 nonblocking send。
- 队列满时返回 false。
- `PublishToTopics` 保留当前 slow-client log 与 `go closeSession(session)` 行为。
- 新增的 `sendMu` 只保护 state check 与非阻塞 select；不得持锁执行 blocking send。
- 不加入 timeout send、retry queue、扩大队列或 backpressure policy redesign。

## 11. Already-Queued Message Semantics

enqueue 在 terminal close 之前成功写入 `Send` 的 payload 属于 **pre-close queued message**。terminal close 不会回滚或从 channel 中撤回该 payload。关闭后 writer 可能已经发送它，也可能因紧随其后的 `Conn.Close()` 而未能发送；客户端可见性仍是 best-effort，不承诺 flush。

terminal close 之后开始的 enqueue 必须返回 false，且不得写入 channel。PR #4 不引入对已排队 payload 的 authorization revoke/retraction 语义；若未来 lifecycle security 要求更强保证，由 PR #5 单独设计。

## 12. Pump Termination

`writePump` 仍是唯一执行普通 `WriteMessage` 和 `SetWriteDeadline` 的 goroutine。它继续从 `Send` 接收，收到 channel closed 且 buffer 已耗尽时尝试现有 CloseMessage 并返回；写 text/ping 失败也会返回。`closeSession` 随后关闭底层连接，确保进行中的网络 I/O 结束。

`readPump` 继续由 `ReadMessage` error 退出并 defer `closeSession`。底层 `Conn.Close()` 唤醒阻塞读取。无需更改 Gorilla reader/writer 数量，也不增加新 close code。

生产代码没有 pump completion handle。测试用例通过测试拥有的 `done` channel 包装对实际 `readPump` / `writePump` 方法的直接调用，并在有界 deadline 内等待返回；不新增 public API、production WaitGroup 或新的 session lifecycle 功能。

## 13. Deterministic Regression Test Seam

增加一个 package-private、无全局状态的窄 helper：

```go
func (s *ClientSession) enqueueWithBeforeSend(payload []byte, beforeSend func()) bool
```

普通 `enqueue(payload)` 委托给 `enqueueWithBeforeSend(payload, nil)`，因此 production send 和测试 send 共用同一实现。测试可传入局部 barrier；helper 在第一次观察 session open 后、获取 `sendMu` 前调用该 callback。无 callback 时不等待、不改变队列语义。

先以保持旧行为的 helper extraction 写 RED：测试 goroutine 穿过 open observation 后阻塞在 barrier；测试调用 `closeSession` 并等待 channel close 完成；再释放 enqueue。测试在 enqueue goroutine 内 recover 并断言旧实现出现 `send on closed channel` panic，避免 panic 逃逸为非确定性测试进程崩溃。然后实现 mutex 后，同一个 helper 在锁内重新检查 `Closed`，该测试必须返回 false 且不 panic。

该 helper 在实现后保留，避免为 RED 测试引入一次性生产结构；测试仍执行真实 `Closed` 检查、`sendMu`、nonblocking channel send 和 `closeSession` 行为。它不只是验证 callback，也不包含 package-global mutable hook。并发测试对 callback 的配置只发生在 goroutine 启动前。

## 14. Test Strategy

所有测试在实现后按 TDD 顺序运行；本设计阶段不创建或运行测试。

1. **Deterministic legacy RED — `TestClientSessionEnqueueCloseLinearization`:** 用第 13 节 barrier 固定 old interleaving。旧实现捕获到 `send on closed channel` panic；新实现返回 false 且无 channel write。
2. **Concurrent enqueue + close — `TestWsSessionConcurrentEnqueueClose`:** 多个 producer 与 terminal close 并发；所有 goroutine 在有界 deadline 内结束，无 panic、无死锁。
3. **Concurrent close requests — `TestWsSessionConcurrentClose`:** 多 caller 同时调用 `closeSession`；无 double-close panic，最终 session 不在 manager 中。`closeOnce` 负责 cleanup body 单次执行。
4. **Queue full + close — `TestWsSessionQueueFullClose`:** 填满 64-slot queue 后请求 close；关闭不等待容量，全部操作有界完成。
5. **Enqueue after terminal close — `TestWsSessionEnqueueAfterClose`:** 返回 false；队列长度不变；无 panic。
6. **Normal delivery — `TestWsSessionNormalDelivery`:** open session 的 enqueue 仍成功；writer 仍通过原有路径发送数据。
7. **Writer termination — `TestWsWriterPumpTerminatesAfterSessionClose`:** 对真实 writer pump 使用测试拥有的 done channel 包装，terminal close 后有界等待返回。
8. **Reader termination — `TestWsReaderPumpTerminatesAfterSessionClose`:** 对真实 reader pump 和 real Gorilla connection 使用测试拥有的 done channel 包装；server-side close 后有界等待 `ReadMessage` 返回。
9. **Manager cleanup and stale snapshot — `TestWsSessionCloseRejectsStaleOperations`:** 在 close 前取出 delivery pointer；close 后用该 pointer enqueue，要求 false/no panic；并控制 `Subscribe` 与 close 两种锁序，证明 closed-session guard 阻止 unregister 后重新注册；manager 最终无该 session。
10. **Shared primitive regression — `TestWsSessionCloseAcrossHandlerRoles`:** 以 dashboard employee、dashboard notification、customer/open 三种 handler/role 做 table-driven 或复用 fixture 覆盖，确认正常 delivery 与 close 均保持一致，不复制相同逻辑。
11. **Race detector supplemental:** `go test -race ./internal/services -run 'Test(ClientSessionEnqueueCloseLinearization|WsSessionConcurrentEnqueueClose|WsSessionConcurrentClose|WsSessionQueueFullClose|WsSessionEnqueueAfterClose|WsSessionNormalDelivery|WsWriterPumpTerminatesAfterSessionClose|WsReaderPumpTerminatesAfterSessionClose|WsSessionCloseRejectsStaleOperations|WsSessionCloseAcrossHandlerRoles|WsSessionCloseSyntheticRuntime)$' -count=1`。race detector 不能替代 deterministic RED。

预计生产文件：`internal/services/ws_realtime_types.go`（`sendMu`、enqueue helper）、`internal/services/ws_service.go`（terminal close ordering）、`internal/services/ws_connection_manager.go`（仅 `Subscribe` 对 terminal session 的 admission guard）。本次增加 manager 文件是为了阻止“Unregister 完成后，迟到 Subscribe 重新注册 closed session”这一源码确认的 interleaving；不修改 `FindDeliveries`、manager snapshot 或其他 routing behavior。测试集中在 `internal/services/ws_session_concurrency_test.go`。

## 15. Synthetic Runtime Verification

实现后由 `TestWsSessionCloseSyntheticRuntime` 使用仓库现有 integration test 方式，在 `httptest` 中注册真实 AgentDesk WebSocket handler，通过 Gorilla client 建立连接并启动真实 pumps。使用 synthetic session 和 conversation，不使用真实用户数据。

并发发布一条 marker event，同时触发 server-side `closeSession`。验证：

- client 在有界 deadline 内观察到 disconnect；
- test process 无 send-on-closed-channel panic；
- manager 最终不再包含该 session；
- stale delivery pointer enqueue 返回 false；
- pump completion tests 在有界 deadline 内结束；
- 正常未关闭 socket 仍收到事件。

该证据只能称为 **in-process synthetic WebSocket runtime/integration acceptance**，不是 production/browser E2E。

## 16. Compatibility

| Surface | Intended change |
|---|---|
| DB/schema | NO |
| Dependency | NO |
| REST | NO |
| WS endpoint/handshake | NO |
| WS event schema | NO |
| Frontend production | NO |
| Customer auth/routing | NO |
| Employee auth/RBAC | NO |
| Queue capacity | NO CHANGE |
| Backpressure semantics | NO CHANGE |
| Wire-visible close behavior | NO INTENTIONAL CHANGE |

本 PR 不调整 customer、employee 或 notification path 的 authorization、routing、payload 或 close reason。安全修复只改变内部 session-local synchronization。

## 17. Rollback

若实现尚未共享，撤销对应 implementation commit；若后续提交已经基于该实现，使用新的 revert commit，不重写共享历史。该设计文档不创建运行时状态、schema 或数据变更；若设计被取代，可单独 revert 本设计 commit。

## 18. Known Separate Finding

调查发现 `ClientSession.Topics` 由 manager 在 manager mutex 下修改，而 `topicList()` 可在该 mutex 外遍历该 map。pump 启动后，connected event 构造仍会调用 `topicList()`；早期 subscribe 或 disconnect 可能与该遍历并发。

这是独立 map concurrency finding，不是 enqueue/close channel race。PR #4 **不修复** `Topics` 访问，不修改 topic routing，也不扩大 manager 职责。该 finding 应作为后续 WebSocket concurrency hardening 工作单独跟踪。

## 19. Non-goals

PR #4 不包含：

- session identity propagation、LoginSession ID、session revoke/revoke-all、employee disable invalidation、expiry timer、role/permission invalidation、PR #5 lifecycle indexes；
- Guest Recovery、customer identity、RBAC、topic authorization、payload shaping；
- `ClientSession.Topics` race fix、graceful server shutdown subsystem；
- frontend reconnect、auth close codes、Redis/pubsub、多实例 lifecycle；
- Gorilla writer redesign、close reason taxonomy、schema、依赖或 REST 变更。

## 20. Open Questions

NONE

Approach B、`Send` channel close policy、finalizer ownership、lock ordering、pre-close queued messages 和 deterministic test seam 均已在本文锁定。

## 21. Done When

PR #4 implementation 后只有在下列条件全部满足时才算完成：

- 所有生产 `Send` enqueue 与唯一 `Send` close site 使用同一个 `sendMu`；
- `Closed` 的检查与 channel send 位于锁内；terminal `Closed.Store(true)` 与 `close(Send)` 位于同一锁内；
- 锁内没有 blocking send、manager operation 或 network I/O；
- stale manager snapshot 在 close 后 enqueue 安全返回 false；
- 迟到的 Subscribe 不能在 terminal close/unregister 后重新注册 session；
- deterministic test 在旧行为下证明 panic、修复后通过；
- 已排队与 close 后 enqueue 的语义符合第 11 节；
- 测试矩阵、synthetic runtime acceptance 和 supplemental race detector 均通过；
- dashboard、notification、customer/open 均无回归；
- Topics map finding 仍被记录且未混入实现；
- 无 schema、dependency、REST、frontend、authorization 或 lifecycle scope 扩张。

本 spec 不授权实现。Implementation plan 与代码工作必须经过后续批准。
