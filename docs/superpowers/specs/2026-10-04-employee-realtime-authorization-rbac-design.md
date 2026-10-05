# PR #2 — Employee Realtime Authorization + RBAC Hardening

- 日期：2026-10-04
- 状态：Design spec，等待用户审查
- 基线：`main` @ `4d8bca6b5a67b298f088443f543b3f7a67421cb1`
- 范围：本文只定义设计；不包含 implementation plan 或生产代码。

## 1. Goal

让员工 dashboard WebSocket 遵守已批准的全局 `conversation.view` 读取权限，并让 RBAC 最终权限只来自启用角色、启用权限和未过期 direct override。`admin:all` 只传送队列所需 metadata；完整 conversation/message 数据只通过已授权的 `conversation:<id>` realtime path 传送。

## 2. Approved Product Semantics

- `conversation.view` 是全局 conversation read capability。它不按团队或 assignee 切分。
- team membership 与 assignee 仍用于分派、队列组织、filter、UI/workflow；本 PR 不将它们变成 authorization boundary。
- 已认证但没有 `conversation.view` 的员工可以完成 dashboard WS handshake，但不能订阅或收到 conversation 内容、消息或队列事件。保留 handshake 行为，避免扩大协议变更。
- `admin:all` 仅对有 `conversation.view` 的员工开放，且仅承载最小队列 metadata。
- 员工 dashboard 的完整 message/content/payload/history/attachment 只发往通过 `conversation.view` 授权的 conversation-scoped topic。客户 WebSocket 保持现有受客户 session/identity 控制的 delivery path；本 PR 不改变客户 WS。
- 显式 `conversation:<id>` subscription 继续要求 `conversation.view`。
- disabled role 不贡献 role permissions；disabled permission 不通过 role 或 direct allow 生效；过期 grant 无效。
- role change 不会撤销已建立的 WebSocket permission snapshot；active socket revocation 明确留给 Phase 1C。

## 3. Current Architecture

### Dashboard WebSocket lifecycle

1. `internal/bootstrap/server.go` 注册 `GET /api/ws/dashboard`，经过 `middleware.AuthMiddleware` 后调用 `WsService.HandleDashboardWS`。
2. Auth middleware 调用 `AuthService.Authenticate`，验证 LoginSession、有效期及 employee 用户状态，并为本次请求加载 roles 与 permissions。它要求 employee 身份，但该路由没有调用 `RequirePermission(conversation.view)`。
3. `HandleDashboardWS` 读取 request context 中的 `AuthPrincipal` 并升级连接。`AuthPrincipal` 包含 user ID、username、nickname、avatar、user status、role codes 和 permission codes；不包含 team、conversation scope、LoginSession ID 或 session expiry。
4. `upgradeConnection` 创建 `ClientSession` 并由 `defaultTopics` 构造默认 topic，之后由 connection manager 注册 topic。
5. 客户端 `subscribe` 命令经过 `filterAllowedTopics`。它允许默认 topic；对 `conversation:<id>` 调用 `canSubscribeConversation`，其中员工必须拥有 `conversation.view`。拒绝的 topic 不会返回 `subscribed` ack，也不会被注册。
6. `PublishToTopics` 通过 topic registry 找到 session，序列化同一 event 后 enqueue；当前没有按 recipient 检查 principal 权限或按 audience 变换 payload 的统一发送边界。

### Current default topics and publication

- 有 user ID 的每个 admin session 默认获得 `admin:<userID>` 和 `admin:all`。
- `routeConversationTopics` 总包含 `conversation:<id>` 和客户 guest topic；assigned conversation 另加 assignee 的 `admin:<userID>`，unassigned conversation 另加 `admin:all`。
- `PublishMessageCreated` 创建的 `message.created` event 同时携带 response.Message、content、payload、message/conversation IDs、sender、status 和 assignee 信息。
- `PublishConversationChanged` 创建的 conversation event 带有 status、assignee/team、last message IDs/timestamps、unread counts、read state 和 `lastMessageSummary`。同一个 event 被发到路由所得的全部 topic。
- 因而仅移除 `admin:all` 的默认订阅或仅修改 `message.created` publisher 都不够：其他 conversation event 仍含 `lastMessageSummary`，assigned conversation 也走个人 `admin:<id>` topic。

### Conversation/message publication path inventory

| Flow | Publisher and entry | Employee topics today | Event/payload today | Recipient authorization at publication |
|---|---|---|---|---|
| Conversation creation and welcome message | `ConversationService.Create` | `conversation:<id>` plus `admin:all` when unassigned or `admin:<assignee>` when assigned; also current customer topic | `conversation.created`; if a welcome message exists, `message.created` and `conversation.updated`. The message event is full; the conversation event includes `lastMessageSummary`. | Create flow has the caller's external identity, but the WS publisher does not evaluate employee recipient permissions. |
| Customer message | API message handler → `MessageService.SendCustomerMessageWithRequestID` → common validated send path | Same route; unassigned goes to `admin:all`, assigned goes to assignee personal topic | `message.created` with full `Message`, content and payload; then `conversation.updated` with summary and unread/queue fields. | Customer session/ownership is checked before write; no employee recipient authorization is checked before fan-out. |
| Employee reply | Dashboard conversation handler → `MessageService.SendAgentMessageWithRequestID` → common validated send path | Same route | `message.created` with full message; then `conversation.updated`. | The HTTP actor has a principal and send permission, but the publisher does not authorize each receiving employee. Sender authorization does not establish recipient authorization. |
| AI/agent reply | AI reply commit service → `MessageService.SendAIMessageWithRequestIDAndWorkflowRunID` → common validated send path | Same route | `message.created` with full message; then `conversation.updated`. | AI execution context identifies the actor, but there is no recipient permission check in WS fan-out. |
| Message recall | Dashboard handler → `MessageService.RecallAgentMessage` | Same route | `message.recalled` (message/conversation IDs, sender/status/time) plus `conversation.updated` (including summary/read/unread fields). | The actor is permission-checked at the REST handler; WS recipient authorization is absent. |
| Human handoff and queue-pool change | `ConversationHumanDispatchService` publishes `conversation.assigned` or `conversation.updated`; automatic dispatch also calls `PublishConversationChanged` | Current assignee's `admin:<id>` after assignment; `admin:all` while unassigned | Conversation patch includes assignment/team/status, unread/read metadata and `lastMessageSummary`; handoff's preceding or subsequent AI/customer message uses the common message publisher above. | Dispatch service knows the conversation/team/assignee, but WS delivery does not use that information as an ACL. |
| Manual assignment, transfer, close, read and other queue state changes | `ConversationService` / `ConversationDispatchService` → `PublishConversationChanged` | `admin:<assignee>` if assigned; otherwise `admin:all` | `conversation.assigned`, `.transferred`, `.closed`, `.read`, or `.updated` with the shared conversation patch including summary. | Actor action may be authenticated/authorized at its handler/service; no per-recipient authorization is performed by `PublishToTopics`. |

All paths converge on `routeConversationTopics` and `PublishToTopics`; that function finds sessions by topic and enqueues a shared serialized event. The envelope `topic` currently remains `conversation:<id>` even when the event is routed to `admin:all` or an assignee topic. Frontend consumers in this repository do not use the envelope topic to authorize or render content.

## 4. Confirmed Gaps

1. `AuthMiddleware` 证明登录身份，不证明 `conversation.view`；dashboard WS 路由默认向所有登录员工授予 `admin:all`。
2. 默认 topic 的自动订阅绕过 explicit topic 的 permission check。
3. `filterAllowedTopics` 只控制订阅，不在消息发布时检查接收者权限。
4. message 与 conversation publishers 对匹配多个 topic 的接收者复用同一完整 payload；未分配 conversation 的完整 message 进入 `admin:all`，已分配 conversation 的完整 message 进入 assignee 个人 topic。
5. `conversation.updated` 等 event 的 `lastMessageSummary` 是客户消息摘要，也会随 `admin:all` topic 发送。
6. RBAC 的 role-code 查询过滤 disabled role；role-derived permission SQL 没有 join/filter `t_role.status`。Direct permission SQL 检查 expiry，但没有检查 `t_permission.status`。
7. 没有一个覆盖 default topics、explicit subscription 和 event delivery 的共享 conversation-read policy boundary。

## 5. Trust Boundary

| Actor | Resource | Policy decision | Current enforcement | Required enforcement |
|---|---|---|---|---|
| Authenticated employee without `conversation.view` | Dashboard WS connection | Login may establish a socket; login alone grants no conversation access | Handshake authenticates employee | Keep handshake; no protected conversation default topic, subscription, or event delivery |
| Employee without `conversation.view` | `admin:all` queue | No queue event access | `admin:all` is default for every employee | Exclude from topic eligibility and check again before event enqueue |
| Employee with `conversation.view` | Queue metadata | Global conversation read permits approved queue realtime | Default topic is granted without permission evaluation | Grant queue metadata topic and send only the metadata variant |
| Employee with `conversation.view` | `conversation:<id>` | Global read permission; team/assignee is not an ACL | Explicit subscription checks `conversation.view` | Preserve subscription check and enforce the same rule before protected event delivery |
| Customer WS client | Customer topic | Existing customer identity/session rules | Existing customer topic and ownership checks | Preserve current customer publication/authorization behavior; customer identity isolation is out of scope |
| Employee with disabled role | REST and new WS session permissions | Disabled role contributes no role permissions | Role list excludes it; permission aggregation may still include its grants | Filter disabled role in permission aggregation; new handshakes use the corrected set |
| Employee with revoked/changed active WS snapshot | Existing socket | Active socket revocation is Phase 1C | Principal is a handshake snapshot | No change in this PR; do not claim old sockets are revoked |

## 6. Approaches Considered

### Approach 1 — Centralized WS authorization boundary (recommended)

Use one dashboard realtime policy boundary for default destination eligibility, explicit destination subscription, and per-recipient event delivery. Classify events by audience and build separate metadata/full event variants before fan-out. Reuse the existing `conversation.view` constant and existing AuthService permission model; do not add an external authorization framework.

**Pros**

- One reviewable enforcement point covers default topics, explicit subscriptions, and sends.
- A newly added publisher cannot make a protected event readable merely by selecting `admin:all`.
- Audience-specific payload construction makes queue metadata separation testable at serialized JSON level.
- RBAC permission filtering remains in the existing permission aggregation service.

**Cons**

- Requires refactoring topic fan-out so one event can have a safe queue representation and a protected conversation representation.
- Needs first-party frontend handling for metadata-only `message.created` events and generic content-free notifications.

**Files/modules likely touched**

`internal/services/ws_service.go`, `ws_realtime_types.go`, `ws_connection_manager.go` if recipient delivery needs a policy hook, `auth_service.go`, `web/hooks/use-agent-conversation-realtime.ts`, `web/lib/im-realtime-state.ts`, `web/lib/stores/agent-conversations.ts`, and focused tests.

**Testability / regression risk**

Test the subscription decision and the serialized event delivered for each `deliveryTopic` using the existing in-memory DB and captured-session patterns. Regression risk is moderate because handoff, queue refresh, unread state and assignee toast behavior currently share broad event routes.

### Approach 2 — Patch individual publishers/subscribers

Add permission checks independently to default topic registration, explicit subscribe, each message/conversation publisher, and payload builders.

**Pros**

- Each local change can be small.
- Some publisher-specific behavior can remain close to its current business flow.

**Cons**

- A future publisher can omit the check and recreate the leak.
- Permission logic and queue sanitization will be duplicated across default topics, personal topics, message created/recalled, conversation changes, handoff, assignment, and employee/customer/AI replies.
- Tests can pass for one path while another still broadcasts a full payload.
- Maintenance cost and drift risk grow with each realtime event type.

**Files touched / testability / regression risk**

Likely the same files as Approach 1, with checks spread over more branches. Local unit tests are straightforward, but proving every path remains covered is harder; missing-path regression risk is high.

### Recommendation

Choose **Approach 1**. Current code centralizes delivery-destination lookup and enqueue in `PublishToTopics`, while authorization is limited to one subscription helper. That makes a common recipient policy plus audience-aware fan-out the smallest coherent boundary. Approach 2 would repeat policy in precisely the multiple publication paths that currently share the broad route.

## 7. Recommended Design

Introduce a small dashboard realtime policy boundary, owned by the existing WS service layer, with two explicit decisions:

Use these names to distinguish the fan-out destination from the event's serialized data:

- **`deliveryTopic`** is the actual destination selected by fan-out, such as `admin:all`, `admin:<userID>`, `conversation:<id>`, or a customer topic. It is the only topic identity allowed as input to employee delivery authorization or employee payload-variant selection.
- **`envelopeTopic`** is the existing serialized event's `topic` field. It remains protocol/event data and is never an authorization input or an audience selector.

The security invariant is:

> Employee authorization and queue/full payload selection MUST use the actual delivery topic chosen by fan-out. They MUST NOT derive audience authorization from `event.Topic`, envelope topic, or any equivalent serialized event field.

The policy exposes two explicit decisions:

- `CanSubscribeTopic(session, candidateTopic)` is limited to registration admission for a default or client-requested destination. On success, the exact registered destination becomes the `deliveryTopic` used by fan-out; this admission check does not select an event payload.
- `CanReceiveEvent(session, deliveryTopic, eventClass)` decides every recipient immediately before enqueue.

Both decisions use the same `conversation.view` permission code through one principal-based helper; do not retype the permission string in destination and publisher branches. `CanSubscribeTopic` denies an unrecognized `candidateTopic`; `CanReceiveEvent` denies an unknown `deliveryTopic`, unknown conversation event class, missing employee principal, missing permission, or policy error. Do not add team or assignee authorization.

Conversation publication chooses the employee variant from `deliveryTopic`, never from `envelopeTopic` or another serialized event field:

- **`deliveryTopic = admin:all` or `admin:<userID>`:** conversation queue events are metadata-only.
- **`deliveryTopic = conversation:<id>`:** a full employee conversation event is allowed only for a subscribed employee whose session principal includes `conversation.view`.
- **Customer delivery:** preserve the existing customer path; it does not enter the employee payload policy. Customer isolation/recovery are out of scope.

Keep handshake authentication login-only. A user without `conversation.view` may receive connection control events (`connected`, `pong`) but no protected conversation topic or event. The frontend can continue to open its current socket without gaining conversation data.

## 8. WebSocket Authorization Design

### Default topics

- Construct employee default delivery destinations through the shared policy, not by unconditional inclusion of `admin:all`.
- Include `admin:all` only for a principal with `conversation.view`.
- The personal `admin:<userID>` delivery destination may remain registered for compatibility, but conversation/message publishers send it metadata only; it never carries full conversation content.
- No-view sessions remain connected but have no conversation-content topic. No separate handshake permission contract is introduced.

### Explicit topics

- Keep a requested `conversation:<id>` destination gated by `conversation.view` exactly as required today; after registration, that destination is the `deliveryTopic` for fan-out.
- Preserve fail-closed rejection: an unauthorized `requestedTopic` is not registered and receives no `subscribed` acknowledgement. Do not add a new error event contract in this PR.
- Unknown, malformed, or unauthorized `requestedTopic` input never falls back to `admin:all` or another destination.

### Event delivery

- Run `CanReceiveEvent(session, deliveryTopic, eventClass)` for every recipient immediately before enqueue, using the exact destination selected by fan-out, including sessions reached through default destinations.
- Full employee conversation/message data requires both a recognized protected event and `deliveryTopic = conversation:<id>`, with the receiving employee principal holding `conversation.view`.
- If `deliveryTopic = admin:all` or `admin:<userID>`, employee conversation queue delivery is metadata-only and requires `conversation.view`.
- If delivery is to a customer topic, preserve the existing customer delivery path and do not apply the employee payload policy.
- Never use `event.Topic`, `envelopeTopic`, or any equivalent serialized event field to authorize a recipient or select a full versus metadata payload. For example, an event whose `envelopeTopic` is `conversation:<id>` but whose `deliveryTopic` is `admin:all` or `admin:<userID>` must still receive only metadata.
- Deny protected event types not explicitly classified for the target audience. Policy evaluation errors do not send the event; logs may include event type, topic class and conversation ID, but not message content or payload.
- Keep active-socket permission snapshot/revocation behavior unchanged; enforcement uses the permissions held by that socket at handshake. Phase 1C owns active connection invalidation.

## 9. Queue Payload Design

Reuse the existing `conversation.*` and `message.created` event types and their existing optional fields; do not introduce a new event type or field schema in this design. Select the employee queue/full variant exclusively from `deliveryTopic`; `envelopeTopic` is event data only.

### Allowed queue metadata

For `conversation.*` queue updates, serialize only existing fields needed to identify/invalidate the row and update queue indicators:

- `conversationId`
- `status`
- `serviceMode`
- `currentAssigneeId` and `currentTeamId` (queue organization only, not authorization)
- `lastMessageId`, `lastMessageAt`, `lastActiveAt`
- `customerUnreadCount`, `agentUnreadCount`

For a queue/personal `message.created` notification, allow only existing `conversationId`, `messageId`, `senderType`, `status`, and `currentAssigneeId`. This is enough for a queue refresh and a generic “new message” toast for the assigned employee without revealing message content.

### Prohibited queue fields

Do not serialize `lastMessageSummary`, `message.content`, `message.payload`, top-level `content`/`payload`, sender name/avatar, customer name/contact/profile, history, attachment metadata or attachment content. The queue path carries no rendered message or conversation detail. It also omits exact customer/agent last-read message IDs and timestamps; those are delivered only on an authorized conversation-scoped path where the transcript consumer uses them. The JSON test must inspect both top-level and nested `message` values so zero-value structs cannot accidentally expose content fields.

### Consumer behavior

- `Conversation Monitor` currently treats any non-control event as a signal to reload its list via REST, and reloads open detail via REST when `conversationId` matches. It does not use message body fields for queue refresh. Its rows render `lastMessageSummary` and both agent/customer unread counts from the REST conversation response.
- `useAgentConversationRealtime` currently consumes full `message.created` to patch the selected transcript, derive list `lastMessageSummary`/unread state, and build the assigned-agent notification body. It consumes `conversation.*` patches directly and reloads list membership when status/assignee/team changes. `ConversationList` renders the summary and agent unread count from that store.
- The frontend must recognize metadata-only `message.created` as an invalidation/notification signal, not normalize it into an empty message. It reloads queue data through the existing permission-protected REST API. Toasts remain generic and content-free; clicking one opens the conversation, where the authorized REST/detail and conversation-scoped WS path load full content.
- `ConversationList`/monitor rows continue to display `lastMessageSummary` from authorized REST results; the WS queue event no longer updates that summary directly.

The current structs already contain the listed IDs, states, counts, and sender classification as optional fields. No additional JSON field or event name is required. Omitting message body from the admin queue is an observable change to the existing dashboard WS delivery semantics, but the first-party consumers can use the existing event envelope and REST refresh flow.

## 10. RBAC Aggregation Design

### Confirmed fields and current reduction semantics

- `enums.StatusOk = 0`, `StatusDisabled = 1`, `StatusDeleted = 2`.
- Role and Permission each have a `Status` field. Role/permission records use `StatusOk` as enabled.
- `UserRole` links user to role; `RolePermission` links role to permission; `UserPermission` is unique by user and permission and has `Effect` and nullable `ExpiredAt`.
- The model comment documents effect `1` = allow and `-1` = deny. The aggregator currently applies `effect < 0` as deny/remove and every `effect >= 0` as allow/add. No separate effect validator was found in the service aggregation path; this PR does not change that operator or add new effect validation.
- Null expiry is non-expiring; a direct grant is active only when `expired_at > now`. Equal-to-now and past expiry are excluded.
- Reduction order is: union role permissions, apply each user override; deny removes a code from the union, allow adds it. A direct deny therefore wins over a role grant; an allow grants a code even when no role grants it.

### Required query changes

1. Preserve role-code loading’s `r.status = StatusOk` filter.
2. In role-derived permission aggregation, join `t_role AS r` through `t_role_permission` and `t_user_role`, and require both `r.status = StatusOk` and `p.status = StatusOk`.
3. In direct override aggregation, require `p.status = StatusOk` as well as the existing non-expired predicate before applying the existing effect reduction.
4. Disabled permissions are absent from the effective set. A direct deny for a disabled permission does not need to remain as a permission: the code is globally disabled and must not be present in the set. Enabled-permission deny/allow precedence stays unchanged.
5. Sort the resulting permission codes as today so output ordering remains deterministic.

These changes belong in the existing `authService.loadUserPermissionCodes` aggregation boundary; no schema, migration, or new RBAC framework is required.

## 11. Error / Fail-Closed Behavior

- Missing principal or missing `conversation.view`: no protected queue or conversation event.
- Unauthorized explicit conversation `requestedTopic`: do not register it and do not emit a success acknowledgement.
- Invalid `requestedTopic`/`deliveryTopic` or unknown protected event class: deny; no fallback destination.
- Authorization query/evaluation error: do not enqueue protected conversation data.
- JSON serialization failure: retain current drop behavior and error logging, without logging payload contents.
- Permission status not enabled, role status not enabled, or grant expired: omit it from the effective permission set.

## 12. Frontend Compatibility

- **Dashboard no-view user:** repository consumers of `/api/ws/dashboard` are the conversation workbench and conversation monitor; these are conversation surfaces whose REST reads require `conversation.view`. The notification WebSocket is separate. Keep the login-only handshake for compatibility, but deliver no conversation queue/content data to a no-view principal; connection control events alone do not grant access.
- **Queue UI:** current `Conversation Monitor` refreshes over REST on realtime events. It remains compatible with metadata-only events. Agent workbench requires a small frontend update to refresh the queue on metadata message events and make assigned-message notifications content-free.
- **Realtime event contract:** `INTERNAL_CHANGE`. Reuse the same event names and optional payload field schema; no new event type. First-party consumers must accept metadata-only event variants. No public REST API changes.
- **Dashboard transport compatibility policy:** for this fork, `/api/ws/dashboard` is treated as a first-party internal dashboard transport for PR #2. The discovered consumers are the first-party conversation workbench and Conversation Monitor. No versioned public dashboard WebSocket protocol or dashboard WebSocket SDK was found. The customer-facing embeddable SDK and its customer realtime path are separate and do not constitute a dashboard WS contract. This PR does not guarantee the previous full `admin:*` payload compatibility for undocumented external consumers; the security fix takes priority over preserving that undocumented behavior. This does not claim upstream has never had private consumers. If a public dashboard realtime API is needed in the future, design a separate versioned contract.
- **SDK changes:** none for the repository’s existing SDKs.
- **Schema / dependencies:** no database migration and no new dependency.
- **AI handoff / unassigned queue:** assignment/team/status fields remain available as queue metadata. Full content is fetched through the authorized detail path after a user opens the conversation.
- **Existing admin behavior:** employees with `conversation.view` retain queue and full conversation realtime capability; employees without it stop receiving conversation data. Team/assignee remain organization and filter dimensions only.

## 13. Test Strategy

### Existing Test Coverage

| Existing test | Covered behavior | Missing behavior |
|---|---|---|
| `internal/services/auth_service_test.go` (`TestValidateSessionTokenStates`, `TestAuthServiceLogoutRevokesCurrentTokenOnly`, login tests) | Session validity, expiry/revocation record handling, login/session creation. | No role/permission aggregation tests; no WebSocket state after revocation. |
| `internal/services/ws_service_test.go` | Notification topic formatting and notification-created event type. | No admin default topic, permission, queue payload, conversation subscription, or recipient delivery test. |
| `internal/services/conversation_human_dispatch_realtime_test.go` | Captured-session tests for assigned personal-topic and unassigned `admin:all` handoff state events. | The helper registers raw topics without an AuthPrincipal; tests do not exercise permissions or assert message-body isolation. |
| `internal/services/permission_service_test.go` | Built-in permission synchronization. | No disabled-role, disabled-permission, direct allow/deny, expiry, or final-set precedence test. |
| `internal/services/notification_service_test.go` and notification event-handler tests | Notification persistence/event behavior. | They do not cover the dashboard conversation WS authorization boundary. |
| `web/lib/agent-conversation-realtime.test.mjs` | Which conversation patches cause a list reload versus local patching. | No websocket envelope, metadata-only event, toast privacy, or queue authorization test. |
| `web/components/agent-realtime-provider.test.mjs` | Realtime provider placement in dashboard layouts/pages. | No authorization or payload behavior. |
| Conversation handler / role service test search | No dedicated conversation-handler authorization test file or role-service status/aggregation test file was found. | No employee A/B REST-vs-WS matrix for `conversation.view`. |

The existing auth test DB helper migrates the RBAC models, so it is a suitable base for isolated permission aggregation fixtures. The existing realtime capture helper creates a `ClientSession` with manually registered topic strings and no principal; new WS tests need explicit principals both with and without `conversation.view`, and should exercise registration, subscription, event fan-out and serialized payload together.

### Planned Tests

Use TDD in the implementation run. Keep RBAC tests in the existing AuthService SQLite test pattern; keep WS tests in the existing captured `ClientSession`/in-memory DB pattern; add frontend reducer/hook behavior tests beside existing realtime tests. Do not add implementation code in this design stage.

### RBAC tests

- Enabled role + enabled permission grants the permission.
- Disabled role + enabled permission does not grant a role code or role permission.
- Enabled role + disabled permission does not grant the permission.
- Enabled direct allow on enabled permission grants it.
- Disabled permission + direct allow does not grant it.
- Expired direct allow is ignored; a future or null expiry follows current behavior.
- Enabled role grant plus direct deny removes the permission.
- Direct allow still adds a permission after role union; this preserves current precedence.
- Reduction retains current `effect < 0` deny / `effect >= 0` allow operation.
- Assert deterministic sorted output and query compatibility with SQLite and MySQL.

### WebSocket tests

- Authenticated employee without `conversation.view` may handshake under the selected design but gets no `admin:all` default topic and no protected conversation event.
- Employee with `conversation.view` gets the approved metadata queue topic and can explicitly subscribe to `conversation:<id>`.
- Explicit subscription without `conversation.view` is not registered and has no successful subscribe acknowledgement.
- Test permission at both default destination construction and per-recipient publish; bypassing one layer must not deliver protected data.
- For `admin:all` and `admin:<assignee>`, marshal each event and assert that no content, payload, summary, history, customer profile or non-empty message body is present.
- Regression: construct an event whose serialized `envelopeTopic` is `conversation:<id>` while its actual `deliveryTopic` is `admin:all`; assert fan-out selects only the metadata variant and never the full message. Repeat with `deliveryTopic = admin:<userID>`.
- For `conversation:<id>`, an authorized employee receives complete message/content/payload; an employee without the permission does not.
- Verify the same publication behavior for customer message, employee reply, AI/agent reply, recall, conversation create/update, manual assign/transfer/close/read, dispatch, and both assigned and unassigned human handoff paths.
- Preserve customer-facing topic delivery tests without expanding this PR into customer identity changes.

### Frontend tests

- Metadata `message.created` refreshes queue state without adding an empty message to the transcript.
- Metadata events do not expose a message body in toast/notification text.
- Full event on the selected authorized conversation continues to update transcript, unread state and list summary.
- `conversation-monitor` refreshes the queue/detail through REST and does not depend on message body in WS events.

## 14. Runtime Verification

Implementation-stage local verification uses only synthetic employees, roles, permissions, and conversations; never use real user/customer records and never print passwords or tokens.

1. Create Employee A with enabled role granting `conversation.view`; create Employee B with no such permission; create a synthetic conversation and a unique marker message through normal application flows.
2. REST A reads the conversation/messages successfully. REST B receives the application’s permission-denied result; assert the JSON application code rather than assuming an HTTP status, because the existing middleware writes JSON results with HTTP 200.
3. Connect fresh dashboard WS sessions for A and B. A receives only the metadata variant on `admin:all`; B receives no protected queue event. Neither gets full message content from `admin:all` or `admin:<userID>`.
4. A explicitly subscribes to `conversation:<id>` and receives the full marker event. B’s explicit subscription is not acknowledged and B never receives the marker.
5. Disable the role that grants `conversation.view` through the normal application API. New REST requests for A are denied; a newly established WS session for A receives no protected content. This validates role filtering on new permission snapshots.
6. Do not assert that already-open sockets lose permissions after role disable/logout/expiry. Their lifecycle remains a Phase 1C requirement and is intentionally not part of PR #2.
7. Open the built dashboard in the local browser and confirm the queue refreshes, unread indicators update, generic toast works, and opening a conversation loads its content through the authorized detail path.

Keep synthetic runtime records clearly tagged for later cleanup by the implementation Run; this spec creates none.

## 15. Rollback

Revert the PR #2 implementation commit(s) as a unit so backend audience separation and first-party frontend metadata handling remain compatible. If queue UI regresses, keep the authorization enforcement and use REST refresh/polling or correct the metadata consumer. Do not roll back by restoring full-message broadcast to `admin:all`.

## 16. Non-goals

- Guest recovery, customer identity, customer WS identity collision, or customer online status.
- LoginSession-to-active-WebSocket revocation, logout disconnect, session expiry disconnect, employee-disable disconnect, or permission-change forced disconnect (Phase 1C).
- Team ACL, assignee-only access, supervisor hierarchy, cross-team policy.
- WebSocket send/close race, transactional outbox, AI concurrency, RAG, schema migration, or new dependencies.
- Changing REST conversation permission semantics or adding an external authorization framework.

## 17. Open Questions

NONE. The repository evidence supports treating `/api/ws/dashboard` as a first-party internal dashboard transport for this fork, and the compatibility policy above resolves the implementation boundary. No remaining unknown is expected to change the implementation architecture.

## 18. Done When

This design is ready for review when the current source behavior, first-party consumers, delivery-topic authorization invariant, regression case against envelope-topic confusion, all conversation publication families, RBAC effect/expiry precedence, both architecture approaches, event payload allowlist, test strategy, runtime boundary, compatibility, rollback, and non-goals are explicit. No code or implementation plan is authorized by this document. The next stage begins only after the user approves this written spec.
