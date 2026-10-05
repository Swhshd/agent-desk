# PR #3 — Customer WebSocket Identity Isolation

- 日期：2026-10-06
- 状态：Design spec（架构方向已批准；implementation 尚未开始）
- 基线：`main` @ `40685ee9519221f8374b9b6202682b30a6ca2d8f`
- Feature branch：`codex/customer-ws-identity-isolation`
- 范围：本文锁定 customer realtime identity isolation 设计，不包含 implementation plan、生产代码或 migration。

## 1. Goal

保证 customer WebSocket 的默认订阅、显式 conversation 订阅、customer-targeted publication、connection registry destination 和 online/presence 都以已验证 session 中的服务端 `CustomerID` 为权威身份。不同 customer records 即使拥有相同 `ExternalID`，也不得共享 realtime destination 或彼此接收受保护数据。

保留 `ExternalSource + ExternalID` 作为外部身份查找、客户创建/匹配、identity mapping 和 session issuance/verification 的业务身份。session 成功解析到 `CustomerID` 后，不得再用 external identity 决定 realtime authorization 或 routing。

## 2. Approved Architecture Decision

采用 **server-authoritative internal CustomerID**。

- customer session 验证必须把已验证的正 `CustomerID` 带入 customer WebSocket authenticated session/context。
- realtime destination 使用 `customer:<decimal CustomerID>`。
- 同一个 `CustomerID` 的多个 socket 共享 customer destination；不同 `CustomerID` 永远使用不同 destination。
- default subscription、显式 conversation authorization、publication routing 和 presence 都使用同一个 CustomerID authority。
- 客户端的 `externalId`、`externalSource`、`identityKey` 或 envelope `topic` 不得覆盖或重新推断该 authority。
- 对缺失或无效的 trusted `CustomerID` 不得退回 `guest:<externalId>`。

这是服务端路由键调整，不改变 customer 外部身份模型或 guest recovery。

## 3. Current Identity Model

| Field | Authority / source | Persisted representation | Current use |
|---|---|---|---|
| `Customer.ID` / `CustomerID` | Server assigned, database primary key | `Customer.ID`、`CustomerIdentity.CustomerID`、`Conversation.CustomerID` | Customer record identity、conversation ownership、session claim；当前未进入 customer WS session/topic |
| `ExternalSource` | Guest path 由服务端根据认证路径标记为 `guest`；user path 来自验证后的 signed user token | `CustomerIdentity.ExternalSource` | 与 `ExternalID` 一起查询 mapping；当前 customer topic construction 丢弃该 source |
| `ExternalID` | Guest 可通过 `X-External-Id` 或 `externalId` 提供；user token path 来自签名 token 的 `userId` claim | `CustomerIdentity.ExternalID` | External identity lookup、session `identityKey`；当前又被用作 WS topic/presence key |
| Channel ID / code | Request 选择已启用 channel，服务端加载 channel record | `CustomerSession` claims、`Conversation.ChannelID` | session 验证时要求 channel ID/code 匹配；不属于 `CustomerIdentity` lookup key 或当前 guest topic |
| Channel type | Server-side channel record | `Channel` | `GetConversationExternalIdentity` 会按 channel type 优先挑选 external mapping；不进入当前 WS topic |
| Customer session identity | Server-signed JWT：`CustomerID`、channel ID/code、`IdentityKey` | Token claims；不新增持久 session table | `VerifyRequest` 检查 customer 存在、channel 匹配、identity mapping 指向 token CustomerID |
| Guest / anonymous state | 没有 user token 时走 guest path；必须提供非空 external ID | `ExternalSourceGuest` customer identity mapping | 建立 session 前使用；PR #3 不改变首次身份取得、所有权或恢复语义 |

证据：`Customer` 与 `CustomerIdentity` 类型位于 `internal/models/models.go`；`GetBy` 在 `internal/repositories/customer_identity_repository.go` 以 source 与 ID 查询；`EnsureExternalCustomer` 在 `internal/services/customer_service.go` 使用相同 pair 查找，未命中则分别创建 Customer 和 CustomerIdentity；`ExternalUser` 与 guest/user 解析位于 `internal/pkg/openidentity/openidentity.go`。

### 3.1 REST/session identity behavior

1. `POST /api/customer/session_exchange` 通过 open identity 解析 external user，并调用 `CustomerSessionService.Exchange`。
2. `EnsureExternalCustomer` 以 `(ExternalSource, ExternalID)` 查找已有 identity；未命中时创建 customer record 与映射。
3. Session JWT 签入 CustomerID、ChannelID/Code 和 identity key。
4. `VerifyRequest` 验证签名、expiry、token type、channel 和 customer record；随后按 token identity key 找 mapping，并要求 mapping.CustomerID 等于 claim.CustomerID。
5. 当前验证结果只保留 `ExternalUser`、token、expiry 与 refresh 状态；verified CustomerID 没有进入 `ClientSession`。

因此不同 source、相同 ID 是不同 lookup key，可以创建不同 CustomerID；然而当前 WS 又把两者压成同一个 `guest:<ExternalID>`。

## 4. Current WebSocket Architecture

1. `internal/bootstrap/server.go` 注册 `GET /api/ws/open` 到 `WsService.HandleOpenWS`。
2. `HandleOpenWS` 加载 enabled channel。没有 employee `AuthPrincipal` 时，通过 `CustomerSessionService.VerifyRequest` 验证 customer session；取回的 `ExternalUser` 与 refresh 信息传给 `upgradeConnection`。
3. `upgradeConnection` 创建 `ClientSession`。当前 `ClientSession` 有 connection ID、principal、`ExternalUser`、role、topics 和 send queue，没有 CustomerID。
4. `defaultTopics` 对 customer external session 生成 `guest:<trimmed ExternalID>`；registry 是进程内 `topics map[topic]map[connectionID]*ClientSession`。
5. 客户端 `subscribe` 经 `filterAllowedTopics`。默认 topic 被允许；`conversation:<id>` 通过 `IsCustomerConversationOwner`，当前用 source+ID 查 identity 后比较 `CustomerID`。
6. `PublishMessageCreated`、`PublishMessageRecalled`、`PublishConversationChanged` 将事件交给 `routeConversationTopics`。该方法将 `conversation:<id>`、客户 `guest:<ExternalID>` 及 admin destination 放入 fan-out topic 集。
7. `PublishToTopics` 通过 registry 找到实际 delivery session。event envelope 的 `Topic` 通常保持 `conversation:<id>`，它可与实际 guest destination 不同。
8. `closeSession` 调用 manager `Unregister`，移除该 connection 的所有 topic registrations。

关键源码：`internal/services/ws_service.go`、`internal/services/ws_realtime_types.go`、`internal/services/ws_connection_manager.go`、`internal/bootstrap/server.go`。

## 5. Confirmed Collision Surface

静态碰撞已确认，未声称已通过 runtime 复现。

| Surface | Current identity/destination | Source included? | CustomerID used? | Finding |
|---|---|---:|---:|---|
| Customer default subscription | `guest:<ExternalID>` | No | No | 不同 source、相同 ID 注册到同一 registry topic |
| Explicit conversation subscription | `ExternalSource + ExternalID` 查询后与 `conversation.CustomerID` 比较 | Yes, in lookup | Indirectly | 当前 owner 检查能区分 source；设计改为直接比较已验证 session.CustomerID 与 conversation.CustomerID |
| Customer publication | `routeConversationTopics` 按 conversation 选 identity，再拼 `guest:<ExternalID>` | No, in topic | No | 不同 customer 可加入同一 guest delivery set |
| Message delivery | common `PublishMessageCreated` route | No, in destination | No | customer marker/message 可投递到碰撞 topic 下的其他 customer socket |
| Conversation/recall delivery | shared `PublishConversationChanged` / `PublishMessageRecalled` route | No, in destination | No | 同一 destination 问题适用于 conversation patch 和 recall 事件，不限于 message publisher |
| Online/presence | `IsGuestOnline(ExternalID)` → manager `HasTopic(guest topic)` | No | No | 一个 customer connect 可使另一个相同 ID customer 显示在线 |
| Disconnect/subscriber state | topic registry 中每个 topic 的 session map | No | No | 两个客户共用同 topic subscriber set；一个断开不应单独清除另一个连接，但双方在线/订阅状态不可区分 |
| Connected payload | `connected.data.topics` 包含 default topic；`guestId` 仍是 external ID | Topic no; field is external value | No | 旧 topic string 可被客户端观察；first-party code 不依赖它 |
| Envelope topic | 通常为 `conversation:<id>` | N/A | N/A | 这是 event data，不是 fan-out destination；不得据此识别或重新计算 customer audience |

### 5.1 Publication family inventory

当前已检查的业务发布路径最终收敛至共享 WS publisher 和 `routeConversationTopics`：

| Flow | Current entry/publisher | Events relevant to customer routing |
|---|---|---|
| Conversation create / welcome | `internal/services/conversation_service.go`: `ConversationService.Create` | `conversation.created`；若有 welcome message，还发布 `message.created`、`conversation.updated` |
| Customer message | `internal/services/message_service.go`: common `MessageService` send path | `message.created`、`conversation.updated` |
| Employee reply | `internal/services/message_service.go`: common `MessageService` send path | `message.created`、`conversation.updated` |
| AI/workflow/agent reply | `internal/services/message_service.go`: common `MessageService` send path | `message.created`、`conversation.updated` |
| Recall | `internal/services/message_service.go`: `MessageService.RecallAgentMessage` | `message.recalled`、`conversation.updated` |
| Assignment / transfer / close / update / read | `internal/services/conversation_service.go`: `ConversationService` methods | 对应 conversation event 与 `conversation.updated` |
| Automatic dispatch | `internal/services/conversation_dispatch_service.go`: `ConversationDispatchService` | `conversation.assigned` / conversation change |
| Human dispatch and handoff | `internal/services/conversation_human_dispatch_service.go`: `ConversationHumanDispatchService` | assigned 或 pool conversation changes；相邻 message 仍走 common MessageService path |

实施不得只改 customer message publisher。所有 customer-targeted publisher 必须经统一的 CustomerID destination construction。`PublishNotificationCreated` 是 employee notification destination，不是 customer routing。当前未发现 customer typing topic。customer read cursor 属于相邻的 conversation-scoped persistence：`ConversationReadStateService.GetByCustomerReader` 查询条件包含 `conversationID`、reader type 与 externalReaderID；它不是 WS topic、registry identity 或 presence key。本 PR 不改变该 read-state storage key。

## 6. Presence Model

Presence 目前不是独立持久状态：`WsConnectionManager` 内存 map 记录每个 topic 的连接；`HasTopic` 仅判断订阅集合非空。`ConversationResponse.CustomerOnline` 由 builder 调用 `WsService.IsGuestOnline(identity.ExternalID)` 计算。断连时 `Unregister` 从 topic 的 connection set 删除当前 session；同一 topic 若仍有其他 session，则 topic 仍 online。

新设计保留该 in-memory 模式，不引入 Redis、DB presence、计数表或 migration：

- online key 使用 `customer:<CustomerID>`。
- 同 CustomerID 两 socket 均注册相同 topic；断开一个后，只要另一个仍注册，online 仍为 true。
- 不同 CustomerID 即使 external source/ID 碰撞，也检查不同 topic；A 的 connect/disconnect 不改变 B 的 online 结果。
- Conversation response 的 online 计算直接以 `Conversation.CustomerID` 查询，不能先选择 external identity 再丢弃 source。

## 7. Frontend / SDK Contract

当前 first-party customer support chat：

- `web/lib/im-realtime.ts` 创建 `/api/ws/open` WebSocket，并传 channel ID 与 customer session token；不创建或发送 customer topic。
- `web/lib/stores/support-chat.ts` 处理 session refresh、resync、按 payload `conversationId` 的 message/conversation 更新；没有根据 envelope `topic` 路由或授权。
- `web/public/sdk/agent-desk-sdk.min.js` 是 iframe loader，传配置与 external identity 到支持聊天页面，不自行构造 topic 或 WebSocket protocol。
- dashboard realtime clients 使用 dashboard/notification endpoints，不是 customer endpoint。

因此 first-party client 不需要改变 URL、handshake 参数、REST 或 SDK public API。`connected.topics` 中的 customer topic value 会从 `guest:<externalId>` 变为 `customer:<CustomerID>`；按批准政策这是 **internal realtime transport value change**，不是 public SDK API change。Regression test 必须确认 first-party 客户端不要求旧 `guest:<externalId>` value，也不依赖 envelope topic 作 recipient routing。

## 8. Existing Schema Limitation

`CustomerIdentity` 当前给 `CustomerID`、`ExternalSource`、`ExternalID` 三列标记同一 GORM unique index `uk_customer_external`。该 index 约束的是三列组合，并非 `(ExternalSource, ExternalID)` 的全局唯一约束。Repository lookup 则只按 source+ID 查找。

本 PR 记录但不修复该既有差异：

- 不修改 index 或模型 schema。
- 不新增 migration 或 uniqueness backfill。
- 不声称解决 external identity pair 的全局唯一性或并发首次创建问题。
- 不把 identity persistence integrity 与 WS destination isolation 混为一项。
- 当前 CustomerID destination isolation 不依赖新增 identity pair constraint。

若实施证明确实无法在无 schema 变化下保证 CustomerID isolation，必须停止并升级 Human Gate；不得自行迁移。

## 9. Approaches Considered

### Approach 1 — Server-authoritative internal CustomerID (**APPROVED**)

把 session 已验证的 CustomerID 放进 customer WS session；topic 使用 `customer:<decimal CustomerID>`；所有 realtime customer audience 与 presence 使用该 destination。

**优点**

- 使用已经验证且持久化的唯一 customer primary key。
- 不同 customer record 不会因 source/ID 编码或外部 namespace 碰撞。
- 同一 CustomerID 的多身份/多 socket 仍可共享 customer-level realtime destination。
- 不需 composite serialization/normalization contract。
- 不需 schema、REST、URL 或 SDK API 变更。

**代价**

- CustomerID 必须从 `CustomerSessionVerifyResult` 明确传入 `ClientSession`，并检查每个 customer realtime 边界。
- `connected.topics` 中 topic 字符串变化；旧的活跃 socket 需重连后才采用新 destination。
- 测试需覆盖身份传递、所有 publisher、订阅和 presence，避免遗漏一个旧 external-ID 路径。

### Approach 2 — Composite `ExternalSource + ExternalID` (**REJECTED FOR THIS PR**)

使用安全编码的 source+external ID 构造 customer destination。

拒绝原因：需要额外的无歧义 composite encoding、normalization、长度与稳定性契约；identity key 长期依赖外部 namespace/source 名称；source rename 或规范化增加耦合；服务端已有已验证的唯一 internal CustomerID；组合键没有足够安全收益来抵消额外复杂度。若使用 delimiter/simple concatenation，还会引入歧义；即使用稳健编码仍无法优于直接使用 CustomerID。

## 10. Recommended Design

固定 customer destination 为：

```text
customer:<positive decimal CustomerID>
```

Topic 是进程内 registry key，不是 REST resource 或数据库对象。唯一来源为 CustomerSessionVerifyResult 中通过 `VerifyRequest` 验证的 CustomerID。不得根据 request `externalId`、`ExternalSource`、`IdentityKey`、`conversation.Topic` 或 `RealtimeEvent.Topic` 回推 topic。

CustomerID 应通过 `strconv.FormatInt(id, 10)` 或当前等价的整数 topic helper 格式化；`id <= 0` 返回无 destination。确保新增 `customer:` prefix 不与现有 `user:`、`guest:`、`admin:`、`conversation:`、`notification:` prefixes 冲突。

## 11. CustomerID Propagation Design

身份流固定如下：

```text
verified customer session token
  → VerifyRequest verifies token/channel/customer/mapping
  → CustomerSessionVerifyResult.CustomerID
  → HandleOpenWS passes verified result
  → ClientSession.CustomerID
  → customer topic, subscription policy, publication registry, presence
```

具体约束：

- `CustomerSessionVerifyResult.CustomerID` 来自已验证 claim；验证时已确认 customer 存在且 identity mapping 归属一致。
- `HandleOpenWS` 只使用 VerifyRequest 返回的值，不读取 request 中声明的 CustomerID，也不在 realtime path 二次调用 `GetBy(ExternalSource, ExternalID)` 推断目的地。
- Customer `ClientSession` 保存 verified CustomerID。若结果缺失、非正或身份不匹配，不得注册任何 customer destination；`VerifyRequest` 已拒绝的请求不得升级成 customer socket。
- ExternalUser 可继续保留供现有非-routing 展示/日志语义，但不能被 protected data authorization、subscription、publication 或 presence 读取。
- 不新增 CustomerID 到公开 REST DTO、WebSocket 握手参数或数据库记录。

## 12. Topic / Registry Design

- `ClientSession.ID` 继续唯一标识每条 socket connection。
- 通用 topic registry 继续保存 `topic → connectionID → session`；不新增独立 identity map、计数器或持久 presence store。
- `defaultTopics` 对经过验证的 customer session 返回单个 `customer:<CustomerID>` destination。
- user/customer realtime session 缺少正 CustomerID 时不能退回 `guest:<ExternalID>` 或 `user:<ExternalID>`；其他非-customer role 的既有 destination 按其既有规则保持。
- 同 CustomerID 多连接各以不同 connection ID 注册在同一 customer topic，delivery fan-out 各 socket 一次。
- Unregister 只移除断开的 connection；topic 仍有其他 session 时保留该 topic 与 online 状态。

## 13. Subscription Authorization

For every authenticated customer socket, compute `ownCustomerTopic = customerTopic(session.CustomerID)` exclusively from the verified session CustomerID. The namespace rule is:

> `customer:*` is a server-owned realtime namespace. A client-visible customer topic string is not an authorization capability. Customer topic admission is derived exclusively from the verified session CustomerID.

Customer topic candidate admission is exact-match only:

- If `session.CustomerID` is positive and candidate equals `ownCustomerTopic`, the candidate is eligible. Since the own destination is already automatically registered by `defaultTopics`, an explicit subscribe is idempotent: it must not add another registry entry or create new authority. Preserve the existing acknowledgement behavior for an already-registered default topic (no duplicate `subscribed` acknowledgement when the manager reports no new registration).
- Any other `customer:<id>` candidate is rejected before registry mutation. Do not parse the client supplied ID and look up a Customer, do not consult ExternalSource/ExternalID, and do not use `connected.topics` as a capability.
- `customer:`, `customer:0`, `customer:-1`, `customer:not-a-number`, and any malformed customer topic fail closed.
- A rejected customer topic is absent from the session registry and receives no `subscribed` acknowledgement or protected delivery.

For `conversation:<id>` customer subscription:

1. The topic must parse as a valid positive ID.
2. The session must have a positive verified CustomerID.
3. The conversation must exist and `conversation.CustomerID == session.CustomerID`.
4. Only then may the topic be registered and acknowledged.
5. Mismatch, missing/invalid session identity, malformed topic, or missing conversation fails closed with no registry mutation and no acknowledgement.

Customer A subscribing to `customer:B` where B != A is **NO / REJECTED**. Customer A subscribing to B's conversation is also rejected; the reverse direction is rejected as well. REST conversation authorization remains unchanged. Authorization never comes from a client-supplied CustomerID, external identity re-lookup, envelope topic, or `guest:<externalId>` fallback.

## 14. Publication Routing

统一 customer destination 构造使用 `conversation.CustomerID`：

- `routeConversationTopics` 加入 `customer:<conversation.CustomerID>`，仅当 ID 为正。
- 不再通过 `GetConversationExternalIdentity` 选择 guest ID 来构造 customer WebSocket destination。
- `conversation:<id>` 与 employee/admin destinations 继续按当前产品行为路由；CustomerID identity fix 不改 employee authorization 或 payload policy。
- `message.created`、`message.recalled`、conversation event 共用 customer destination，涵盖 create/welcome、customer message、employee reply、AI/workflow reply、recall、assign、transfer、close、read、dispatch、assigned handoff 与 pool/unassigned handoff。
- `RealtimeEvent.Topic` 是 envelope data，可能仍为 `conversation:<id>`。customer destination 必须来自发布对象的 `Conversation.CustomerID` 与 registry destination，不得从 envelope topic 推导 recipient identity。
- 同一 socket 若因明确授权同时订阅 conversation destination 与 customer destination，应保持现有 single-session deduplication/fan-out 行为，不得对该 session 重复 enqueue 同一 event。

所有当前 conversation/message publisher 已收敛到共享 WS publisher。实现和测试须检查真实 publication entrypoints，不能只 unit test topic string helper 或只修改 message publisher。

## 15. Presence Semantics

提供 customer-ID 语义的 online 查询，并由 conversation response builder 以 `Conversation.CustomerID` 使用：

- A=`customer:ID_A`，B=`customer:ID_B`，即使 ExternalID 相同也相互独立。
- A connect 不影响 B 的 online；A disconnect 不影响仍连接的 B。
- 两个 socket 若 CustomerID 相同，都属于该 Customer destination；关闭一个后另一个存在时 online 保持 true。
- customer 没有可信 CustomerID 时 online 必须为 false，不能查询 external-ID topic。
- presence 继续为内存 registry 推导值；不写 DB/Redis。

## 16. Fail-Closed Behavior

以下任一情况禁止 customer protected realtime destination 或 delivery：

- customer session 验证失败或 identity mapping 不匹配；
- verified CustomerID 缺失、为零、为负或 destination helper 返回空；
- explicit conversation subscription 中 CustomerID 与 owner 不同；
- publisher 的 conversation CustomerID 无效。

不得尝试 `guest:<externalId>`、composite external identity 或 event envelope topic 作为 fallback。错误状态不得创建 customer topic entry、acknowledge unauthorized subscription 或把受保护 payload enqueue 给 customer session。不得把普通连接控制事件作为 protected event 的替代路径。

## 17. Compatibility

| Surface | Change |
|---|---|
| Database schema / migration | NO |
| REST routes and response schema | NO |
| `/api/ws/open` URL and handshake arguments | NO |
| WebSocket event envelope/payload schema | NO |
| Customer SDK public API | NO |
| First-party frontend behavior | NO expected code change; verify no legacy-topic dependency |
| `connected.topics` customer value | YES: `guest:<externalId>` → `customer:<CustomerID>`；这是 internal realtime transport value change |
| Existing active sockets | Existing process-local registrations cannot gain verified CustomerID retroactively; they use old binary/registration until disconnect/restart. Normal server restart drops old in-memory registry and clients reconnect using the new verified session path. No active-socket revocation protocol is added. |
| External identity lookup and customer session issuance | NO; source+ID remains the business lookup identity |

First-party frontend evidence supports no SDK/client code change because the customer client does not construct customer topics. The numeric CustomerID shown inside `connected.topics` is not an authorization secret or capability; a topic string alone cannot grant access. Admission is recomputed against the verified session CustomerID. The design makes no claim about undocumented external consumers; `connected.topics` remains an internal transport value under this fork policy.

## 18. Test Strategy

Implementation must use TDD. Test data is synthetic and clearly marked; no real customer records, secrets, tokens or external identifiers enter logs/reports.

### 18.1 Identity and session propagation

- In isolated test DB, use the open-session-supported `ExternalSourceGuest` (A) and `ExternalSourceUser` (B), both with `collision-id`; assert they resolve to distinct positive CustomerIDs. Here source-A/source-B are logical labels.
- Verify a valid signed customer session exposes the claim-validated CustomerID in `CustomerSessionVerifyResult`; identity mapping mismatch remains rejected.
- Assert request parameters cannot override the verified CustomerID used by the WebSocket session.

### 18.2 Default destination and registry

- Register guest-source A and signed-user-source B sessions with the same synthetic external ID but distinct CustomerIDs; assert destinations are exactly `customer:<A>` and `customer:<B>` and are different.
- Assert an invalid/missing CustomerID produces no customer destination and never creates a `guest:<externalId>` fallback.
- Assert two sessions sharing one CustomerID register under the same customer destination while retaining separate connection IDs.

### 18.3 Real publication and delivery isolation

- Use actual conversation/message publication entrypoints and isolated conversations owned by Customer A/B.
- Publish `CUSTOMER_WS_A_<random>` through A conversation; assert A receives it and B does not. Repeat with a B marker and reverse expectations.
- Cover create/welcome, customer message, employee reply, AI/workflow reply, recall, conversation update, assign, transfer, close, read, dispatch, assigned handoff and unassigned/pool handoff through the shared route.
- Assert all customer event families use the same CustomerID destination construction, and no publisher enqueues a full payload to a different CustomerID.
- Envelope regression: set or preserve `RealtimeEvent.Topic` as a conversation topic that does not equal the actual customer destination; assert recipient selection still follows the verified CustomerID destination, never the envelope topic.
- Preserve per-session deduplication if a session is registered for multiple authorized destinations.

### 18.4 Explicit subscription

- A can subscribe to A conversation; B can subscribe to B conversation.
- A cannot subscribe to B conversation and B cannot subscribe to A conversation; rejected topics receive no `subscribed` acknowledgement and are absent from registry.
- Malformed/non-positive IDs, missing session CustomerID, missing conversation, and owner mismatch all fail closed.

### 18.5 Customer topic namespace authorization

- Own destination: with `session.CustomerID = A`, explicitly subscribe to `customer:A`; it is eligible and idempotent because `defaultTopics` already registered it. Assert no duplicate registry entry and no duplicate `subscribed` acknowledgement under current default-topic semantics.
- Cross-customer destination: with `session.CustomerID = A`, subscribe to `customer:B` where B != A; assert rejection before registry mutation, no `subscribed` acknowledgement, no topic membership, and no B protected marker delivery.
- Predictable-ID regression: give A the valid numeric CustomerID B; changing only the client topic string to `customer:B` must still be rejected and must not expose B data.
- Invalid customer topics `customer:`, `customer:0`, `customer:-1`, and `customer:not-a-number` all fail closed with no registry mutation or acknowledgement.
- Assert admission compares against `customerTopic(session.CustomerID)` from the verified session and does not perform a client-ID lookup, external identity lookup, or envelope-topic check.

### 18.6 Presence and reconnect

- A/B same external ID but distinct CustomerID: A connect/disconnect never changes B online result.
- Same CustomerID with sockets A1/A2: both share one customer identity; closing A1 while A2 stays registered keeps online true; closing A2 then changes it to false.
- CustomerOnline builder queries by conversation CustomerID and not external identity.

### 18.7 First-party client contract

- Verify customer WebSocket URL still uses `/api/ws/open`, channel, and session token, without requiring a client-generated customer topic.
- Feed a `connected` control payload whose topics contain `customer:<id>` and confirm first-party customer consumer does not require the previous `guest:<externalId>` value.
- Verify customer event handling remains keyed by payload `conversationId`, not envelope `topic`.

### 18.8 Existing relevant tests

Current baseline tests include `internal/services/customer_service_test.go`, `internal/services/ws_service_test.go`, `internal/services/ws_employee_publication_test.go`, `internal/services/ws_employee_payload_test.go`, and `internal/pkg/openidentity/openidentity_test.go`. The customer service test uses in-memory SQLite but tests repeated same-source identity/name update only; current WS service tests cover notification topics; existing realtime publication tests exercise multiple publication families for a single synthetic customer. None currently tests same external ID across sources or customer WS isolation.

Baseline run during investigation:

- `go test -tags dev ./internal/services -run '^TestEnsureExternalCustomerUpdatesNameFromExternalIdentity$' -count=1` — PASS.
- `go test -tags dev ./internal/pkg/openidentity -count=1` — PASS.

These runs did not reproduce the collision. Runtime reproduction remains NOT RUN at design stage.

## 19. Runtime Verification

After implementation, use an isolated synthetic environment and fresh random markers only:

- Customer A with `ExternalSourceGuest` + shared synthetic external ID and CustomerID A.
- Customer B with `ExternalSourceUser` + the same external ID and CustomerID B, using a synthetic signed user identity; assert A != B.
- Two owned synthetic conversations and two independently authenticated customer sockets.
- Verify REST/session identity records remain isolated without changing REST contract.
- Verify each socket’s default destination is its own `customer:<CustomerID>` and cross-conversation explicit subscription is denied.
- Send independent A/B message markers through real publication flows; each reaches only its owner socket.
- Disconnect A and confirm B remains online; open two sockets for one customer and confirm closing one does not mark that customer offline.
- Verify a customer with no trusted CustomerID receives no protected customer realtime and has no external-ID topic fallback.
- Do not use actual customer data. Do not include session tokens, credentials, or secrets in logs/final report. Clean all synthetic runtime fixtures using the isolated runtime’s documented cleanup path before completion.

No runtime collision reproduction is claimed by this design document.

## 20. Rollback

No database/schema/data migration is introduced, so rollback requires no data reversal. If deployment reveals an implementation regression, revert the server code and restart/redeploy so all sockets reconnect under one binary. Reverting to the old implementation also restores the known external-ID topic collision risk; record this security consequence in the rollback decision. Do not add dual-topic delivery during rollout, because retaining `guest:<externalId>` as fallback would reintroduce cross-customer delivery.

## 21. Non-goals

- Guest Recovery A2, possession proof, recovery credential, externalId ownership semantics, or customer session protocol redesign.
- Employee realtime authorization/RBAC changes from PR #2.
- Active customer session revoke, logout disconnect, socket revocation or expiry redesign.
- WebSocket send/close race or connection lifecycle redesign.
- Team ACL, database migration, CustomerIdentity pair uniqueness repair, external identity deduplication or customer merge.
- Public REST redesign, new WS URL/handshake parameter, event schema redesign, public SDK API change.
- AI/RAG, transactional outbox, customer message/content redesign, Redis/DB presence.

## 22. Open Questions

NONE. The architecture and compatibility policy are approved. The CustomerIdentity unique-index limitation is explicitly recorded as an existing non-goal; implementation must stop for Human Gate only if evidence proves CustomerID isolation requires schema change.

## 23. Done When

- Verified CustomerID flows from customer session validation into the authenticated WebSocket session.
- Default subscription, explicit `customer:<id>` admission, explicit conversation authorization, publication routing, registry destination and presence all use the same CustomerID authority.
- Customer A subscribing to `customer:B` where B != A is rejected; a visible CustomerID is not an authorization capability.
- No protected customer path falls back to externalId/source+externalId or envelope topic.
- Same-externalId/different-source customers are isolated; same-CustomerID multi-socket behavior is preserved.
- Customer publication families and first-party client compatibility have explicit regression coverage.
- REST, handshake arguments, event schema, SDK public API and DB schema remain unchanged; `connected.topics` value change is documented.
- Existing external identity DB uniqueness limitation is documented and remains out of scope.
- TDD and synthetic runtime acceptance are specified; runtime collision has not been falsely claimed as reproduced.
- This PR’s design artifact is the only file changed/committed at this stage; no implementation plan or feature code is present.
