# PR #5 — Employee WebSocket Session Lifecycle & Active Revocation Design

## 1. Goal

Make authenticated employee WebSockets obey the current login-session validity and current employee authorization state for their entire lifetime. A socket must not receive protected data while its initial post-upgrade state is being revalidated. Explicit session revoke, employee-wide revoke, account disable/delete, and authorization changes must close the exact affected live sockets. Login-session expiry and direct permission-override expiry must close sockets without waiting for traffic.

The approved architecture is EVENT_DRIVEN_PRECISE. It scans the current process-local session registry, uses one lifecycle deadline timer per employee socket, computes precise affected employee IDs for authorization mutations, and asks the existing profile endpoint to classify browser reconnects. It adds no custom WebSocket close code.

## 2. Approved Product Semantics

| Mutation | Required live-socket effect | Fresh authentication |
| --- | --- | --- |
| Revoke LoginSession A | Close every socket carrying A's server-bound LoginSession ID; preserve every socket for B | A is denied |
| Revoke all sessions for employee U | Close every employee socket for U | All revoked sessions are denied |
| Disable or delete employee U | After the disabled/deleted status write succeeds, close every employee socket for U, even if the later bulk session-revoke write fails | Current server status check denies U |
| Assign roles to U | After the role-assignment transaction commits, close U's sockets; retain the existing bulk LoginSession revoke behavior | Current revoked-session behavior applies |
| Change role, role permissions, permission sync, or direct override | Close only employees whose effective authorization inputs are affected; retain their LoginSession unless an existing path already revokes it | A valid session receives the newly computed principal on reconnect |

Role assignment continues to revoke all of that user's login sessions, as it does today. Other RBAC changes do not revoke LoginSessions solely because authorization changed.

## 3. Current Gaps

- `AuthService.Authenticate(*gin.Context) (*dto.AuthPrincipal, error)` validates a bearer token and reads a LoginSession, but retains only AuthPrincipal in Gin context. The session ID and absolute ExpiredAt are discarded before the dashboard WS handler runs.
- `wsService.upgradeConnection` in `internal/services/ws_service.go` registers employee sessions on default topics immediately after upgrade, then starts pumps. There is no pending/non-deliverable state and no authoritative post-registration revalidation.
- `WsConnectionManager` in `internal/services/ws_connection_manager.go` indexes sessions by connection ID and topics, but has no session-ID lifecycle operation. A user-wide close would otherwise require a scan, which is the approved approach.
- `wsService.closeSession` already provides PR #4 close-once and enqueue/close synchronization. Lifecycle closure must reuse that primitive.
- `LoginSessionService.Revoke` and `RevokeByUser` persist revocation but do not close sockets. `AuthService.Logout` currently writes RevokedAt through the generic update path.
- `UserService.UpdateStatus` and `DeleteUser` persist status separately from `RevokeByUser`. If the status write succeeds and the later revoke fails, an existing socket still remains open unless invalidated immediately after the status write.
- `UserService.AssignRoles` commits its role transaction before calling `RevokeByUser`; this leaves a required post-commit invalidation point before the later revoke call can fail.
- `RoleService.UpdateStatus`, `RoleService.AssignPermissions`, and `PermissionService.SyncBuiltinPermissions` can change effective permission inputs without invalidating already-authenticated principals.
- `OIDCLoginService.ensureDefaultOIDCRole` can insert a UserRole inside the OIDC transaction for an existing employee without an after-commit realtime invalidation.
- `UserPermissionService` exposes direct override CRUD methods, though no current handler calls them. They need a supported service boundary so a future/current internal caller cannot silently leave active snapshots stale.
- LoginSession expiry is checked only on a later authentication request. Direct UserPermission expiry is included in authorization calculation only when a later authentication occurs. Neither deadline closes an idle socket.
- `web/lib/realtime-connection.ts` retries employee sockets without first distinguishing an invalid session from a transient network failure. Conversation Monitor has its own retry loop in `web/app/(dashboard)/dashboard/conversation-monitor/page.tsx`.

## 4. Architecture Decision

Adopt the approved event-driven, precise design:

1. Authenticated employee WS upgrade creates a PENDING connection.
2. PENDING is registered in the same process-local session map as ACTIVE sockets, but has no protected topics and receives no protected fan-out.
3. A trusted LoginSession ID is revalidated against the database after PENDING registration.
4. The socket receives a single deadline equal to the earlier of LoginSession ExpiredAt and the next active direct-override ExpiredAt.
5. Activation atomically installs the fresh principal and employee default topics only if the pending connection remains registered and open.
6. Supported mutation services persist first and invalidate after commit or after the successful single-row write.
7. The coordinator scans the existing manager sessions map. No user/session secondary indexes are added.
8. The server closes at the deadline even with no client activity. A browser checks the existing profile endpoint before deciding whether to stop or continue reconnecting.

The flow is:

~~~text
authenticated request
→ WebSocket upgrade
→ PENDING registration (no topic membership)
→ authoritative LoginSession / employee / RBAC revalidation
→ deadline timer installed
→ atomic ACTIVE transition + default employee topics
→ connected event and pumps
→ protected fan-out only to ACTIVE, unexpired employee sessions
~~~

Transactional mutations follow:

~~~text
DB mutation
→ COMMIT
→ after-commit callback
→ precise realtime invalidation
~~~

There is no invalidation before commit, no generic event framework, no session-token replay, and no Redis/pubsub. The approved design does not require changing the public handshake or event contract.

## 5. Trusted Login Session Binding

An employee `ClientSession` must carry server-derived lifecycle metadata:

- `EmployeeID int64`
- `LoginSessionID int64`
- `LoginSessionExpiresAt time.Time`
- `Principal *dto.AuthPrincipal`
- `NextAuthzChangeAt *time.Time`
- `LifecycleDeadline time.Time`
- lifecycle state PENDING, ACTIVE, or CLOSED
- the existing connection ID, connection, topics, send queue, and PR #4 close synchronization

`EmployeeID` must equal the validated session's UserID and `Principal.UserID`. The LoginSession ID and ExpiredAt come only from the LoginSession row selected by AuthService after validating the bearer token. No client-provided ID, query parameter, connected payload, or event envelope may set or override them.

The raw bearer token is never copied to `ClientSession`. LoginSession ID, lifecycle timestamps, and lifecycle state are internal fields and do not appear in the WS handshake, connected event, event envelope, or any public response.

Customer and guest sockets do not receive employee lifecycle metadata and remain outside this policy.

## 6. Internal Authentication Metadata

Keep the existing public `AuthService.Authenticate` signature and public `dto.AuthPrincipal` unchanged. After successful validation, it also stores a private typed value in `ctx.Request.Context()` under a package-private context key:

~~~go
type authenticatedEmployeeSession struct {
    EmployeeID              int64
    LoginSessionID          int64
    LoginSessionExpiresAt    time.Time
    Principal                *dto.AuthPrincipal
    NextAuthzChangeAt        *time.Time
}

func (s *authService) GetAuthenticatedEmployeeSession(ctx *gin.Context) (*authenticatedEmployeeSession, bool)
~~~

The value is derived in the same authentication pass that validates the token and computes the principal; it is not a second independent token parse. REST handlers continue to receive the same principal and response. Dashboard WS handlers require both AuthPrincipal and this internal metadata and fail closed if either is absent.

The metadata getter reads only the private request-context value. It does not read a client header, query parameter, or public DTO field. A request context that contains a manually injected principal but no trusted lifecycle metadata cannot create an employee WS session.

## 7. Authoritative Revalidation

Introduce a narrow internal port:

~~~go
type EmployeeSessionRevalidator interface {
    RevalidateEmployeeSession(loginSessionID int64, now time.Time) (EmployeeSessionSnapshot, error)
}

type EmployeeSessionSnapshot struct {
    EmployeeID            int64
    LoginSessionID        int64
    LoginSessionExpiresAt time.Time
    Principal             *dto.AuthPrincipal
    NextAuthzChangeAt     *time.Time
}
~~~

The query/aggregation implementation is an `employeeAuthStateReader` in `internal/services/employee_auth_snapshot.go`, not `AuthService` and not `wsService`. `employeeRealtimeLifecycle` implements `EmployeeSessionRevalidator` by delegating to that reader. The reader takes only the trusted session ID and a single captured `now`; it loads the LoginSession by ID through repositories (not service globals) and checks:

- the row exists;
- RevokedAt is nil;
- `now` is before LoginSession ExpiredAt;
- the employee exists and has enabled status;
- current enabled roles and current enabled permissions;
- current direct overrides whose permission is enabled and whose ExpiredAt is nil or later than `now`;
- current effective permission aggregation using the existing semantics: role grants are unioned first, then direct overrides remove for `Effect < 0` and add for `Effect >= 0`.

It returns a fresh principal and the earliest future active direct-override expiry. Query errors, missing rows, revoked/expired sessions, missing/disabled employees, or a deadline already reached reject activation. The post-upgrade failure path closes the socket without adding protected topic membership.

Authentication and revalidation share the same authorization-snapshot query/aggregation helpers. Revalidation does not call `AuthService.Authenticate`, does not need the bearer token, and does not update LastSeenAt merely to revalidate a socket.

## 8. Pending / Active Connection State Machine

Use explicit states NEW → PENDING → ACTIVE → CLOSED. CLOSED is terminal.

The employee connection sequence is:

1. Middleware authenticates the request and supplies trusted internal metadata.
2. The handler upgrades the connection and creates a session in NEW.
3. `WsConnectionManager.RegisterPending(session)` inserts it into `sessions` only and transitions it to PENDING. It adds no topic memberships.
4. The lifecycle coordinator revalidates by LoginSession ID.
5. The handler computes the single lifecycle deadline and installs its timer while the session is still pending.
6. `WsConnectionManager.Activate(session, freshSnapshot, defaultTopics)` takes the manager lock, verifies the pointer is still registered under its connection ID, the session is not closed, and the state is still PENDING; it atomically replaces the principal and deadline metadata, transitions to ACTIVE, and inserts the employee default topics.
7. Only after activation does the handler enqueue connected/control output and start the read and write pumps. If the session closed between activation and pump start, it does not start new pumps.

No connected event containing employee state is emitted before successful activation. A close during pending registration is terminal and cannot be undone by a later activation.

## 9. Protected Delivery Boundary

Only ACTIVE employee sessions with a future LifecycleDeadline may receive employee protected queue, conversation, or notification delivery. The manager's topic membership is added only at activation, and delivery policy also checks ACTIVE state and the deadline before enqueue. If the deadline is reached before the timer callback runs, the delivery check closes the socket and rejects the event.

PENDING receives no protected fan-out, including conversation queue data, notifications, and user-specific employee messages. It is present only so a concurrent invalidator can find and close it.

Customer WS authorization, topic membership, event payload, and close behavior remain unchanged. Employee lifecycle policy must branch only for employee realtime roles; it must not be applied to customer topics.

## 10. Employee Realtime Invalidator

Define the mutation-facing port in `internal/services/employee_realtime_lifecycle.go`:

~~~go
type EmployeeRealtimeInvalidator interface {
    InvalidateLoginSession(loginSessionID int64) int
    InvalidateEmployees(employeeIDs []int64) int
}
~~~

`employeeRealtimeLifecycle` implements both this port and `EmployeeSessionRevalidator`. It uses the shared `WsConnectionManager` and the independent `employeeAuthStateReader`.

- `InvalidateLoginSession` matches only employee sessions whose trusted LoginSessionID equals the requested ID.
- `InvalidateEmployees` matches only employee sessions whose EmployeeID belongs to the precise supplied set.
- Both operations include PENDING and ACTIVE sessions and exclude customer/guest sockets.
- Return value is the count of sessions that transitioned to CLOSED during this call; already-closed or absent targets are harmless and do not inflate the count.

No target is a normal zero-count result. A database mutation is not rolled back because a socket is absent or already closed.

## 11. Manager Scan / Lock Contract

`WsConnectionManager.sessions` remains the sole lifecycle targeting source. Add no user-ID or LoginSession-ID secondary indexes.

Add a manager scan that copies matching employee session pointers while holding `RLock`, then releases the lock before invoking the close primitive. The scan includes PENDING and ACTIVE states. It filters on server-bound EmployeeID/LoginSessionID and employee realtime role, never client input.

Registration and activation occur under the manager write lock. PENDING and ACTIVE remain in the same `sessions` map. PENDING has no entries in `topics`. Activation checks registration identity and terminal state while holding the lock.

The manager lock is never held while closing a target. `closeSession` unregisters through the manager; calling it while holding the manager lock would recursively lock and deadlock.

## 12. PR #4 Close Primitive Reuse

Reuse the PR #4 close-once/enqueue synchronization exactly: `closeOnce`, `sendMu`, setting terminal closed state before closing the Send channel, manager unregister, and connection close. Do not invent a second shutdown path or weaken send/close ordering.

To keep the constructor graph acyclic, extract the existing close primitive from `wsService.closeSession` into one manager-owned operation, `WsConnectionManager.CloseSession(session) bool`. `wsService.closeSession` remains the wrapper for normal pump shutdown and logging; the lifecycle coordinator calls the same manager operation. The `bool` is true only for the caller that performed the close-once transition.

The manager operation stops any installed lifecycle timer before unregistering, releases timer/session locks before manager Unregister, and does not hold the manager lock while closing. The existing `sendMu`/`closeOnce` roles and lock order remain unchanged.

## 13. Login Session Expiry

LoginSession ExpiredAt is an absolute server-side deadline. On activation, schedule closure at `LoginSessionExpiresAt` even if the browser sends no message and no event is published. Authentication/revalidation rejects a deadline that is equal to or earlier than `now`.

At the timer deadline the server closes the employee socket through the shared PR #4 primitive. The LoginSession row is not modified by the timer. A later authentication request is rejected by the existing ExpiredAt validation.

The timer belongs to each employee socket, so expiry of session A closes A1/A2 while a different session B remains connected.

## 14. Direct Override Authorization Deadline

The auth-state reader returns the earliest future ExpiredAt among active direct overrides attached to enabled permissions. It uses the same captured `now` and the same `t_user_permission` / `t_permission` enabled-permission universe as the effective permission calculation. Nil expiry contributes no deadline; expired rows and disabled permissions contribute none.

If a direct override expiry is earlier than LoginSession ExpiredAt, it becomes the socket lifecycle deadline. At that time the server closes the old socket without requiring traffic or a DB mutation. A valid LoginSession can reconnect; revalidation recomputes the principal with the expired override removed. This covers both an expiring allow that loses a permission and an expiring deny that restores the role-derived permission.

If the earliest override is redundant with role permissions, the socket may reconnect unnecessarily at that expiry. This is accepted as a safe, bounded consequence of using the earliest active override deadline; it does not expand permission or extend authorization.

## 15. Timer Concurrency and Cleanup

Each employee socket has exactly one timer for `min(LoginSessionExpiresAt, NextAuthzChangeAt when present)`. No timer is created for customer/guest sockets.

- A deadline at or before activation time rejects activation and closes the pending socket.
- The timer callback uses the same `WsConnectionManager.CloseSession` primitive as explicit revoke and pump termination.
- Any earlier terminal close stops the timer. If the callback is already running, `closeOnce` makes the race harmless.
- Timer callbacks hold no manager lock and no timer/session mutex while closing.
- The timer is installed before activation. If it fires while PENDING, it closes the pending connection and activation fails.
- Timer/revoke races have one terminal close, one channel close, and no panic/deadlock.
- A closed socket does not retain an avoidable long-duration timer.

## 16. Session Mutation Invalidation

| Existing mutation path | Lifecycle action |
| --- | --- |
| `AuthService.Logout` | Resolve the validated token to its LoginSession ID and call `LoginSessionService.Revoke`; exact-session invalidation follows the successful persisted RevokedAt write |
| Dashboard `SessionPostRevoke` → `LoginSessionService.Revoke` | Invalidate that exact LoginSession ID after the update succeeds |
| Dashboard `SessionPostRevokeByUser` → `LoginSessionService.RevokeByUser` | Invalidate every employee socket for that employee after the bulk update succeeds, including when zero rows were updated |
| `UserService.ChangeOwnPassword` / `ResetPassword` → `changePassword` → `RevokeByUser` | Preserve existing revoke-all behavior; successful bulk revocation closes every employee socket for the user |
| `UserService.UpdateStatus` disabling/deleting | Invalidate employee sockets immediately after the status update succeeds, before calling `RevokeByUser` |
| `UserService.DeleteUser` | Invalidate after status/deleted_at persistence, before calling `RevokeByUser` |
| `UserService.AssignRoles` | Register precise user invalidation after the role transaction commits and before the later `RevokeByUser` result can be returned |
| `RoleService.UpdateStatus` | Enumerate role members before mutation; after a successful write, invalidate those employees |
| `RoleService.AssignPermissions` | Enumerate members in the transaction and register an after-commit invalidation |
| `PermissionService.SyncBuiltinPermissions` | Compute employees affected by changed enabled permissions and new role-permission links; register after-commit invalidation |
| `OIDCLoginService.ensureDefaultOIDCRole` | When an existing employee receives the default role, register that employee for invalidation only after successful role insertion and transaction commit |
| `UserPermissionService` direct override CRUD | Capture the previous and resulting UserID and invalidate those employee IDs only after successful persistence |

Normal login, LastSeenAt updates, profile reads, role sort/name/remark updates, and failed/rolled-back writes do not invalidate sockets.

## 17. Employee Disable / Delete

`UserService.UpdateStatus` and `DeleteUser` must preserve their existing two-write order but add a fail-safe boundary:

1. Persist disabled/deleted employee status.
2. If that write fails, return without invalidation.
3. Invalidate all PENDING and ACTIVE employee sockets for that UserID immediately.
4. Attempt the existing `LoginSessionService.RevokeByUser`.
5. Return any later revoke error as today; the sockets remain closed.

Fresh `AuthService.Authenticate` and post-upgrade revalidation reject the disabled/deleted employee using current server status. The socket close does not depend on bulk LoginSession revocation succeeding.

## 18. User Role Assignment

`UserService.AssignRoles` retains its transaction and existing subsequent `RevokeByUser` behavior. It registers invalidation for exactly the target employee with `ctx.RegisterCallback` inside the role transaction. The callback runs only after commit and before `replaceUserRoles` returns; therefore a later RevokeByUser failure cannot leave a pre-change socket alive.

On rollback, the callback does not run and existing sockets remain. Fresh profile/reconnect observes the current LoginSession revocation semantics; PR #5 does not change the established behavior.

## 19. Role and Role-Permission Mutation

For `RoleService.UpdateStatus`, capture the assigned employee IDs before the single-row update. If the enumeration fails, do not mutate. After a successful status write, invalidate only those members. A no-op status update may invalidate that role's members but cannot affect other roles.

For `RoleService.AssignPermissions`, enumerate current employee members within the transaction and register the invalidation callback after all replacement rows have been written. Commit invokes it; rollback does not. Fresh authentication recomputes enabled role and permission sets.

`RoleService.UpdateRole` changes display metadata only in current code and does not affect authorization. `DeleteRole` refuses deletion while assigned users exist, so it has no affected live employee set. Generic `UserRoleService` and `RolePermissionService` CRUD wrappers have no current application write callsites; direct writes through them are outside the supported mutation boundary. New application writes must use `UserService.AssignRoles` or `RoleService.AssignPermissions`.

## 20. Permission-Wide Mutation

`PermissionService.SyncBuiltinPermissions` is the current application permission-wide write path. Within its existing transaction:

1. Compare pre-write and resulting builtin permission enabled status and role-permission rows.
2. Collect only permission IDs whose enabled state changes and role IDs receiving a newly inserted relation.
3. Resolve affected employee IDs as the union of employee members of changed roles and employees with direct overrides for changed permission IDs.
4. Register one after-commit invalidation callback for the deduplicated employee set.

An enumeration/query error aborts the transaction. Rollback does not invalidate. Re-running an idempotent sync with no authorization-relevant changes does not close unrelated users. Generic permission CRUD has no current handler write path; future permission writes must use the same precise affected-user calculation.

## 21. OIDC Default Role Mutation

`OIDCLoginService.ensureDefaultOIDCRole` is a real UserRole mutation inside the OIDC transaction. It affects only the employee whose first default role is inserted. The helper reports whether the insert succeeded; only a successful insert registers an after-commit callback. Existing users who already have a role are not invalidated. A failed or rolled-back OIDC transaction does not invalidate.

The OIDC transaction may also issue a new LoginSession. That new session has no socket before the transaction returns, while any pre-existing employee sockets are invalidated after the role commit.

## 22. Direct User Permission Mutation

Support direct override writes at the existing `UserPermissionService` boundary because it is exported and has Create/Update/Updates/UpdateColumn/Delete methods, even though no current HTTP handler calls it.

- Create invalidates the new row's UserID after successful insert.
- Update captures the old row's UserID, persists, then invalidates the old and resulting UserIDs.
- Updates and UpdateColumn capture the old UserID, persist, reload the row, then invalidate the deduplicated old/resulting UserIDs.
- Delete captures the old UserID, performs a checked repository delete, then invalidates that user only after success.
- If a row does not exist, preserve existing not-found/no-op semantics and do not invent another target.

Because the current repository Delete wrapper discards database errors, `UserPermissionRepository.Delete` and its service wrapper must return the DB error so the lifecycle callback cannot claim a failed write committed. No REST route or public wire contract is added.

The generic `UserRoleService` and `RolePermissionService` wrappers remain unsupported for writes in this PR because they have no current application write callsites; domain mutation paths above are the only supported application boundary.

## 23. Transaction / After-Commit Ordering

Use `ctx.RegisterCallback(func() { ... })` inside `sqls.WithTransaction`, matching the existing callback mechanism and the `customer_service.go` usage. `github.com/mlogclub/simple/sqls` runs callbacks only after the DB transaction succeeds. Never call the invalidator inside the transaction body.

For single-row writes that are not transactional, persist successfully first and then invalidate synchronously. For employee disable/delete, that invalidation is deliberately between the status write and the existing bulk session-revoke write. For role assignment, the after-commit invalidation runs before the later revoke call.

Rollback never invalidates. No reconnect can authenticate against uncommitted role or permission state because invalidation happens after commit.

## 24. Dependency Direction and Bootstrap Wiring

Keep all new ports in `internal/services/employee_realtime_lifecycle.go` so mutation services and WS services can depend on interfaces without importing each other:

- `EmployeeRealtimeInvalidator` is owned by the lifecycle boundary and consumed by `LoginSessionService`, `UserService`, `RoleService`, `PermissionService`, `OIDCLoginService`, and `UserPermissionService`.
- `EmployeeSessionRevalidator` is owned by the lifecycle boundary and consumed by `wsService`.
- `employeeAuthStateReader` in `employee_auth_snapshot.go` implements the authoritative read without depending on `AuthService`.
- `employeeRealtimeLifecycle` owns the shared `WsConnectionManager`, session scan, deadline callbacks, and invalidation implementation.
- `WsConnectionManager.CloseSession` owns the one PR #4 close primitive. The lifecycle coordinator does not depend on `wsService`.
- `AuthService` continues to validate bearer tokens and call the same independent auth snapshot helpers. It has no invalidator field.

The package's existing global service style is wired once through constructor arguments with this acyclic dependency order:

~~~text
var employeeWSManager = newWsConnectionManager()
var employeeRealtime = newEmployeeRealtimeLifecycle(employeeWSManager, employeeAuthStateReader{})
var WsService = newWsService(employeeWSManager, employeeRealtime)
var AuthService = newAuthService() // independent; no lifecycle dependency
var LoginSessionService = newLoginSessionService(employeeRealtime)
var UserService = newUserService(employeeRealtime)
var RoleService = newRoleService(employeeRealtime)
var PermissionService = newPermissionService(employeeRealtime)
var OIDCLoginService = newOIDCLoginService(employeeRealtime)
var UserPermissionService = newUserPermissionService(employeeRealtime)
~~~

`AuthService` is independently constructed and shares only pure/repository-backed auth snapshot helpers. `AuthService.Logout` calls `LoginSessionService.Revoke`; it does not call WsService. There is no AuthService → WsService → AuthService constructor or runtime service cycle.

`bootstrap.NewServer()` calls `services.ValidateEmployeeRealtimeLifecycleWiring() error` before registering routes. It verifies the production lifecycle, shared manager, WsService revalidator, and every lifecycle-sensitive mutation service are non-nil and use the same concrete production coordinator; failure returns a startup error. There is no arbitrary global setter and no production nil/no-op fallback.

Tests construct `newEmployeeRealtimeLifecycle`, `newWsService`, and mutation services with explicit in-memory managers and test implementations of the narrow ports. Test no-op implementations are local to tests and cannot be selected by production bootstrap.

## 25. Frontend Reconnect / Profile Validation

Add an explicit three-way result to the existing `SessionProvider` path and expose it through `AuthProvider`; do not create another auth store:

~~~ts
type RealtimeProfileResult = "valid" | "invalid" | "transient"
~~~

Before scheduling a retry after an unexpected employee socket close or a failed connection cycle, both reconnect paths call the existing `fetchProfile()` endpoint via `web/lib/api/auth.ts`.

- Valid profile: merge the returned user, permissions, and roles into the current AuthSession through SessionProvider's existing `writeSession`/`setSession` path, then allow normal reconnect/backoff.
- Invalid session/auth result (the existing auth error codes 3000 or 3002): call the existing centralized `clearSession` flow, stop that socket manager's reconnect loop, and let `AuthProvider`'s existing route effect redirect to the login page.
- Transient profile/network/server error: retain the stored session, do not log out, and continue the existing bounded exponential retry (2 seconds base, 30 seconds cap in the shared manager and Conversation Monitor).

The shared `createRealtimeConnectionManager` receives an optional asynchronous pre-reconnect validator for employee dashboard sockets. It runs at most once for one close/failure cycle before scheduling. A result of invalid disables reconnect; valid and transient preserve retry. Existing consumers without the option keep their current behavior, so customer/support WS behavior is unchanged.

Conversation Monitor keeps its independent retry loop but uses the same exposed validation method before its next retry. It must cancel its pending timer when validation returns invalid. No custom close code, new event, REST endpoint, SDK, second auth store, or broad auth redesign is introduced.

## 26. Failure Semantics

- No matching live session returns zero and is normal.
- A target already closed or concurrently closing is harmless; PR #4 `closeOnce` owns the only terminal transition.
- Database revalidation errors fail activation closed.
- Timer scheduling or firing does not rely on client activity.
- A socket close is best-effort with respect to whether the browser observes a graceful close frame. Authorization correctness comes from committed DB state, fail-closed reconnect validation, and the protected-delivery deadline check.
- A DB transaction rollback never triggers an invalidation callback.
- An invalidator does not turn a successful persisted mutation into a failure merely because no socket matched.
- A transient profile failure never clears the local session.

## 27. Single-Instance Limitation

`WsConnectionManager` is process-local. The current Compose deployment has one AgentDesk service instance. This PR therefore guarantees revocation for all employee WebSockets in the current server process/current single-instance deployment only.

There is no Redis/pubsub, distributed registry, or cross-process revocation guarantee. Multi-instance revocation is a future architecture limitation.

## 28. Test Strategy

No test is implemented in this design stage. Future implementation uses the existing Go SQLite/Gin/Gorilla fixture style and the repository's Node built-in test files.

### Planned production file map

| File | Responsibility and reason |
| --- | --- |
| `internal/services/auth_service.go` | Preserve the existing Authenticate API while attaching trusted private LoginSession metadata; route Logout through exact-session revoke; reuse the shared auth snapshot calculation |
| `internal/services/employee_auth_snapshot.go` (new) | Independently load session, employee, current RBAC, and next direct-override expiry without depending on AuthService or WsService |
| `internal/services/employee_realtime_lifecycle.go` (new) | Define the invalidation/revalidation ports, own the process-local coordinator, compose constructor dependencies, and expose startup wiring validation |
| `internal/services/ws_realtime_types.go` | Add employee session identity, lifecycle state, deadline, and timer fields without changing the public event types |
| `internal/services/ws_connection_manager.go` | Register pending sessions, activate atomically, scan exact employee/session targets, and own the shared PR #4 close primitive |
| `internal/services/ws_service.go` | Implement upgrade → pending → revalidate → deadline → activate and enforce the active/unexpired protected delivery boundary |
| `internal/services/login_session_service.go` | Invalidate exact session after Revoke and employee sessions after successful RevokeByUser |
| `internal/services/user_service.go` | Invalidate after disable/delete status persistence and after role-assignment commit, before later session-revoke errors |
| `internal/services/role_service.go` | Precisely invalidate role members after status and role-permission changes |
| `internal/services/permission_service.go` | Diff permission-sync authorization changes and invalidate the precise affected employees after commit |
| `internal/services/oidc_login_service.go` | Register after-commit invalidation for successful default-role insertion |
| `internal/services/user_permission_service.go` | Make direct override CRUD invalidate old/resulting owners after successful persistence |
| `internal/repositories/user_permission_repository.go` | Return Delete persistence errors so failed direct-override writes cannot trigger a false successful invalidation |
| `internal/bootstrap/server.go` | Refuse to register production routes if the lifecycle coordinator, shared manager, or required invalidator wiring is missing |
| `web/components/session-provider.tsx` | Classify profile validation as valid, invalid, or transient and update/retain/clear the existing session accordingly |
| `web/components/auth-provider.tsx` | Expose the existing centralized validation/cleanup path to employee realtime consumers |
| `web/lib/realtime-connection.ts` | Add optional asynchronous pre-reconnect validation to the shared manager without changing consumers that omit it |
| `web/hooks/use-agent-conversation-realtime.ts` | Pass employee auth validation into the shared manager used by the employee realtime provider |
| `web/app/(dashboard)/dashboard/conversation-monitor/page.tsx` | Add the same profile decision to the monitor's independent retry loop |

### Backend files

| File | Responsibility |
| --- | --- |
| `internal/services/employee_realtime_lifecycle_test.go` (new) | Manager scans include pending and active, exact LoginSession/user targeting, no secondary index, close count, missing/already-closed targets, and manager-lock release before close |
| `internal/services/auth_service_test.go` | Trusted metadata propagation, no raw token, exact session ID/expiry, fresh RBAC snapshot, direct override deadline and past-deadline rejection |
| `internal/services/ws_service_test.go` | Pending has no protected topics/events, activation installs fresh principal/default topics, missing metadata and revalidation errors fail closed |
| `internal/services/ws_session_concurrency_test.go` | Mutation-before-registration and registration-before-callback races, activation-vs-close, expiry-vs-revoke, timer cleanup, and no double channel close |
| `internal/services/login_session_service_test.go` (new) | Logout/revoke-one/revoke-all target exact sockets and occur only after successful persistence |
| `internal/services/employee_rbac_invalidation_test.go` (new) | Disable/delete before revoke failure, assignment after commit before revoke failure, role status, role permissions, permission sync, precise impacted/unaffected employee sets, and rollback no-invalidation |
| `internal/services/user_permission_service_test.go` (new) | Create/update/column update/delete invalidates old/new direct-override owners only after successful writes |
| `internal/services/oidc_login_service_test.go` | Existing-user default-role insertion invalidates after commit; existing role, failed insert, and rollback do not |

### Frontend files

| File | Responsibility |
| --- | --- |
| `web/lib/realtime-connection.test.mjs` (new) | Shared manager waits for profile classification; invalid stops reconnect, valid refreshes and retries, transient retains session and retries |
| `web/components/auth-provider.test.mjs` | Existing centralized session cleanup and redirect remain the invalid-auth behavior |
| `web/components/session-provider.test.mjs` (new) | Profile success updates user/roles/permissions; auth-invalid clears; transient retains session |
| `web/components/agent-realtime-provider.test.mjs` and `web/hooks/use-agent-conversation-realtime.test.mjs` | Shared employee manager uses validation while preserving selected-conversation behavior |
| `web/app/(dashboard)/dashboard/conversation-monitor/conversation-monitor-realtime.test.mjs` (new) | Independent monitor loop uses the same three-way validation and cancels only on invalid auth |

Tests assert no customer/support-chat reconnect behavior changes. Test names in backend must use the `TestEmployee...` / `TestWsPending...` prefixes so focused verification and the race suite can select the new behavior precisely.

## 29. Synthetic Runtime Acceptance

Use only clearly tagged synthetic employee A/B, enabled/disabled roles, permission rows, LoginSessions A/B, and a short-lived direct override. Use existing in-memory SQLite fixtures where supported, a real Gin handler, Gorilla WebSockets, actual mutation service calls, and the shared safe close primitive. Do not use production employee data or print tokens/secrets.

Required in-process acceptance:

- A1/A2 use LoginSession A and B1/B2 use LoginSession B. Revoke A closes A1/A2 only; B1/B2 remain open.
- Revoke all for employee A closes all of A's sessions.
- Disable/delete A closes all A sockets immediately after status persistence; fresh REST authentication and a fresh WS connection are denied even when the later RevokeByUser call is configured to fail.
- Role assignment commits, closes A, then the existing revoke-all behavior runs; transaction rollback leaves A connected.
- Role status, role-permission, permission-wide sync, direct override CRUD, and OIDC default-role insertion close only their affected employee set.
- A short LoginSession expiry closes an idle socket without client traffic.
- A short direct allow expiry removes the permission on reconnect; a short direct deny expiry restores the role-derived permission on reconnect.
- Pending activation cannot receive protected delivery. Mutation-before-registration is caught by revalidation; registration-before-callback is found and closed as PENDING.
- Timer callback racing manual revoke produces one terminal close without panic or deadlock.

When executed, report the evidence as **in-process synthetic employee session/WebSocket lifecycle acceptance**. It is not production evidence or browser E2E.

## 30. Verification Strategy

This is design-only; do not run tests now. Future implementation gates:

1. Focused backend lifecycle and mutation tests: `go test -tags dev ./internal/services -run 'Test(Employee|WsPending)'`.
2. Focused existing auth, OIDC, WS, and RBAC regressions: `go test -tags dev ./internal/services`.
3. Full backend suite: `go test -tags dev ./...`.
4. Static checks: `go vet -tags dev ./...` and `git diff --check`.
5. Frontend tests use Node's existing built-in test runner because `web/package.json` has no test script: `node --test web/lib/realtime-connection.test.mjs web/components/session-provider.test.mjs web/components/auth-provider.test.mjs web/components/agent-realtime-provider.test.mjs web/hooks/use-agent-conversation-realtime.test.mjs 'web/app/(dashboard)/dashboard/conversation-monitor/conversation-monitor-realtime.test.mjs'`.
6. Frontend typecheck: `pnpm --dir web typecheck`.
7. Repository build gate: `task build`, which builds the Flowgram editor, web SDK/static export, and Go server in Taskfile order.
8. Focused race suite: run the lifecycle-focused Go tests with `-race -tags dev` in the official Linux Go 1.26 image, following the already-used PR #4 Linux container approach. The local Windows race toolchain is known broken; do not claim a local Windows race result. Do not change CI just for this PR.
9. Run the synthetic runtime acceptance in Section 29, then final branch/diff review.

## 31. Compatibility

| Contract | Change |
| --- | --- |
| Database schema / migration | No |
| Public REST API contract | No |
| WebSocket handshake wire | No |
| WebSocket event schema | No |
| SDK contract | No |
| Customer WS behavior | No |
| Dependencies | No |
| Frontend production behavior | Yes; minimal employee profile-before-reconnect handling |
| Deployment guarantee | Current single-instance process only |
| Custom close code | No |

## 32. Rollback

Rollback is a normal code revert: remove lifecycle invalidator hooks, the internal employee session metadata and pending/activation path, per-socket timers, and frontend profile-before-reconnect handling. There is no schema or persisted lifecycle data, so no data repair or migration rollback is needed.

## 33. Known Separate Findings

`ClientSession.Topics` is mutated under manager locking, while `topicList()` currently iterates it without that lock. Keep this concurrent-map risk recorded as a **KNOWN SEPARATE FINDING**. PR #5 must not redesign or claim to fix it and must not include it in Done When.

The new activation path must avoid worsening it: return the default-topic slice copied as part of activation under manager locking, and do not call `topicList()` concurrently with subscription/unregistration. Existing independent `topicList()` use remains outside this PR's acceptance criteria.

## 34. Non-goals

- Guest Recovery A2
- Customer WS session lifecycle redesign or customer identity changes
- Redis/pubsub or multi-process revocation
- Database schema changes or new dependencies
- PR #4 close synchronization redesign
- `ClientSession.Topics` race fix
- Team ACL
- Generic domain-event framework
- WebSocket protocol redesign or custom close code
- New REST auth endpoint
- Generic frontend auth rewrite or second auth store
- Message/outbox atomicity
- AI turn concurrency
- Knowledge Base rebuild work

## 35. Open Questions

NONE. The approved architecture and all implementation-level choices in this specification are resolved from current source and the user's approved decisions.

## 36. Done When

The design is complete when implementation preserves all of the following:

- trusted LoginSession ID, ExpiredAt, employee ID, principal, and next authorization deadline flow from validated server auth context; no raw token is stored;
- post-registration authoritative revalidation checks session validity, employee status, current roles, enabled permissions, and valid overrides;
- PENDING is registered before revalidation, has no protected delivery, and cannot activate after terminal close;
- mutation-before-registration and registration-before-callback races both fail closed;
- exact-session revoke never closes another LoginSession; employee-wide operations close all of that employee's sockets;
- mutations invalidate only after commit/successful persistence, with disable/delete closing before later bulk revoke can fail;
- LoginSession expiry and direct override expiry close idle sockets through one per-socket earliest deadline;
- timer cleanup, past deadlines, close races, and manager lock ordering are safe;
- role/permission invalidation targets precise employee sets, including OIDC and direct override paths;
- frontend distinguishes invalid auth, valid profile, and transient profile failure in both reconnect paths;
- customer/support WS, public contracts, database schema, and dependencies remain unchanged;
- the process-local single-instance boundary and the Topics race separate finding remain explicit;
- synthetic runtime and official Linux Go 1.26 race verification are included in future implementation acceptance.
