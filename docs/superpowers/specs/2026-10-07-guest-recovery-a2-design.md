# Guest Recovery A2 Design

## Status

PR #6 architectural design for human review. The user approved Option A and the decisions recorded here. This document specifies future behavior; it does not claim implementation, tests, builds, or runtime acceptance have completed.

Investigation base: `3790b4d8fa016fddd1d335e50ef4d47ff3a173aa` on `codex/guest-recovery-a2`. PRs #1-5 are merged foundations and are not reopened. Implementation planning starts only after review and explicit approval of this written spec.

## Context

The current customer chat uses a server-signed CustomerSessionToken for protected REST and customer WS. The token already binds a customer, channel, external identity, and expiry. Guest externalId is supplied by a client: headers/form/query, SDK runtime configuration, or a browser-generated identifier. The default guest identifier persists in localStorage; the session response, including token and customer.id, persists in sessionStorage. Conversation and message state lives in Zustand memory.

Source anchors at the investigation base:

| Source | Relevant behavior |
|---|---|
| `internal/pkg/openidentity/openidentity.go`: `GetExternalUser`, `getGuestUser`, `verifyUserToken` | Unauthenticated guest hints versus signed host-user identity |
| `internal/handlers/api/customer_handler.go`: `CustomerPostSession_exchange` | Parses external identity before session exchange |
| `internal/services/customer_session_service.go`: `Exchange`, `Sign`, `VerifyRequest`, `externalUserFromClaims`, `shouldRefresh` | Session issuance, verification, global mapping lookup, refresh |
| `internal/services/customer_service.go`: `EnsureExternalCustomer` | Reuse-or-create by source/externalId; updates reused customer metadata |
| `internal/repositories/customer_identity_repository.go`: `GetBy` | Lookup without CustomerID |
| `internal/models/models.go`: `CustomerIdentity` | Unique index includes CustomerID, source, externalId |
| `internal/middleware/chat_middleware.go`: `ExternalUserMiddleware` | Currently retains ExternalUser but drops verified CustomerID for REST |
| `internal/services/conversation_service.go`: `Create`, `IsCustomerConversationOwner` | Re-resolves external identity for creation and ownership |
| `web/lib/api/im.ts`: `ensureCustomerSession`, `exchangeCustomerSession` | Cached guest proof reuse; exchange currently omits session proof |
| `web/lib/stores/support-chat.ts`: `bootstrap`, history and realtime callbacks | Does not reset on changed customer.id or guard all stale writes |
| `internal/services/ws_service.go`, `internal/services/ws_customer_identity.go` | Existing verified-CustomerID routing and subscription ownership |

## Problem Statement

Current guest exchange permits the chain:

```text
client knows guest:X
  -> EnsureExternalCustomer looks up (guest, X)
  -> reuses old Customer A without possession proof
  -> signs a new session for A
  -> protected APIs and customer WS act as A
```

Repairing issuance alone is insufficient. The schema permits A -> guest:X and B -> guest:X, while token verification and REST ownership select a mapping without CustomerID. Conversation creation also calls reuse-or-create again after authentication. A global first-row result must never select guest authority. GORM First ordering does not make that lookup safe.

## Goals

- Only valid server-verifiable CustomerSessionToken possession may continue an existing guest CustomerID.
- Missing or normally expired guest proof creates a fresh customer, preserving caller externalId as a hint.
- Malformed, tampered, mismatched, or invalid-customer proof is rejected; infrastructure failures fail closed.
- Carry verified CustomerID through customer REST and preserve PR #3 customer WS routing.
- Support duplicate guest externalId hints without ambiguous authorization or history inheritance.
- Switch frontend customer state atomically and discard old-customer asynchronous callbacks.
- Preserve signed user-source behavior, public response/token/WS/SDK shapes, TTL, and existing data.

## Non-Goals

Attachment authorization remediation; history merge/migration; cross-device guest recovery; long-lived recovery secrets; email/SMS recovery; employee sessions; RBAC; customer WS routing redesign; new token types or refresh tokens; DPoP/mTLS; device/browser fingerprints; schema migration; Redis; SDK public API redesign; generic identity framework or frontend async architecture rewrites. No customer-session-secret rotation, unrelated Topics concurrency work, or new customer account UX is included.

## Terminology / Trust Boundary

- **Guest hint:** caller-controlled externalId/name. Knowing, predicting, copying, or persisting these values proves no ownership.
- **Host user proof:** existing user JWT verified with the configured channel user-token secret. It yields source=user before stable identity reuse.
- **Guest continuity proof:** existing CustomerSessionToken supplied in the optional exchange header `X-Customer-Session-Token`.
- **Verified CustomerID:** positive CustomerID established by token verification and exact DB validation, or freshly created by the server during exchange. Client request CustomerID and cached customer.id cannot establish backend authority.
- **Exact mapping:** CustomerID AND ExternalSource AND ExternalID match one identity row.
- **Normally expired proof:** authentic, structurally valid, correctly bound proof whose only remaining verification failure is expiry. Expiry does not override a signature, source, channel, identity, customer, or infrastructure failure.
- **Identity epoch:** small frontend generation identifying the currently active customer/channel state; it guards writes, not server authentication.

## Current Failure Mode

For the single existing mapping guest:X -> A, another client without TA can exchange X for a new A token, obtain A's active conversation through create_or_match, read/send/read-mark/close/upload against A's conversation, and connect to customer:A. This is established by static source tracing, not a runtime claim.

Current protected REST is gated by token middleware and then ExternalUser ownership lookup. REST authentication errors use HTTP 200 JsonResult; customer WS authentication rejects its handshake with HTTP 401. Current customer read eligibility rejects missing/deleted Customer records; it does not reject StatusDisabled. Current token identity validation does not apply a separate identity-status policy. PR #6 preserves these status semantics and does not add customer lifecycle revocation. In this spec, invalid customer means absent, deleted, non-positive, or inconsistent with verified binding.

## Approved Architecture

Option A is fixed: the existing CustomerSessionToken is the sole guest continuity proof. Caller externalId remains unchanged. Different CustomerIDs may intentionally have the same guest externalId. No new canonical guest identifier, recovery token, dependency, or schema is introduced.

```text
guest + optional session proof
  -> dedicated exchange proof classification
     valid -> exact verified CustomerID -> sign response for that customer
     missing/expiry-only -> explicit fresh creation -> new CustomerID -> sign response
     malformed/tampered/mismatch/invalid customer -> reject
     DB/config/internal failure -> error
```

Fresh creation never queries a global guest mapping to find a customer. Successful continuity never replaces its verified CustomerID with a customer found by hint. Current response fields remain customerSessionToken, expiresAt, identityKey, and customer { id, name }.

### Exchange request and token domains

The endpoint remains `POST /api/customer/session_exchange`. Authorization and the existing userToken parameter retain their host-user JWT meaning. The new optional guest continuity input is `X-Customer-Session-Token`, never a URL parameter or a host-user Authorization substitute. Existing CORS allow/expose lists already contain this header.

Preserve existing source selection: if a host user token is presented, verify it and enter the existing signed source=user flow. Invalid host user proof fails; it never downgrades to guest. The guest continuity header is not consumed as authority by the user flow; when both are present, the existing user flow takes precedence and the guest header is ignored. First-party user exchange omits the guest header. With no host user token, parse the guest hint through existing input rules and classify the optional guest header. A source=user CustomerSessionToken in this header is rejected as a guest-source mismatch.

The guest hint remains required and is trimmed consistently with persisted identity/token construction. A supplied proof's guest externalId must match that hint. A caller changing the hint or channel deliberately must explicitly discard incompatible proof and bootstrap without it. Name is display metadata, never a binding or deduplication credential.

## Guest Bootstrap State Machine

| Input | Outcome | Existing-customer mutation |
|---|---|---|
| Missing or empty guest proof header | Create fresh guest using hint | None |
| Valid guest proof with exact binding | Continue precisely claims.CustomerID | Only that verified customer's permitted metadata/activity updates |
| Normally expired proof | Create fresh guest using hint | None |
| Malformed JWT, unsupported algorithm, invalid signature, invalid claims | Reject | None |
| Wrong channel, source, hint, or exact mapping | Reject | None |
| Absent/deleted/invalid customer | Reject | None |
| DB/config/signing/internal failure | Error, no successful session | No reassignment or recovery |

The normally expired case creates a different CustomerID even if identityKey remains guest:X. It never renews the old customer's authority. Ordinary signature/claim/channel/mapping/customer checks still apply to classifying an authentic expired proof; expiry-only is decided after these checks. Combined expired+tampered, expired+mismatched, and expired+DB-failure cases must not create a fresh guest.

## Customer Session Proof Verification

Keep distinct entry points for protected request verification and optional exchange proof verification, sharing low-level validation where appropriate. Protected VerifyRequest continues requiring proof and never invokes fresh creation. Exchange classification uses explicit internal outcomes Missing, Valid, Expired and explicit rejected-proof versus internal-error failures. These are internal classifications, not new public error codes or DTO fields.

For a presented guest exchange proof:

1. Require usable signing configuration; parse JWT with the existing permitted HMAC methods and authenticate its signature before trusting claims.
2. Require typ=customer_session, positive customer/channel IDs, nonempty channelCode/identityKey, and a valid required expiry. Enforce other existing registered-claim validity checks; do not broaden algorithm or token-domain acceptance.
3. Check channel ID and code against the enabled requested channel; reconstruct only the known identity source and require guest for this path. Match signed externalId to the request hint.
4. Perform error-aware customer lookup; reject missing/deleted/invalid customer, propagate DB failures separately. Validate the exact identity triple with the repository primitive below.
5. Only after all non-expiry checks pass, classify expiry: unexpired is Valid, expired is Expired. Neither an unauthenticated exp value nor errors.Is(ErrTokenExpired) on a combined error is sufficient for Expired.

An expired proof is never accepted to authorize protected data. Its authenticated classification merely determines whether exchange may create a new, unrelated guest. A missing proof still requires valid channel/config and successful persistence/signing before exchange can succeed. A missing DB result cannot be silently conflated with DB failure.

Signing remains HS256; verification retains the current HS256/384/512 allowlist. Claims remain typ, channelId, channelCode, customerId, customerName, identityKey, iat, exp. No format change, refresh token, or custom recovery secret is permitted. Exact DB validation is also used by protected verification for guest and user sessions, preserving legitimate user semantics.

Default TTL is 120 minutes and refresh threshold is 30 minutes. Existing protected verification and appropriate new WS handshake verification may refresh an unexpired session. WS ping alone is not a token-refresh scheduler. No TTL change or new socket-lifecycle policy is included. A guest offline beyond valid proof expiry intentionally loses automatic recovery by externalId. Existing bearer semantics remain: a valid token holder may act as that guest.

## Exact Identity Resolution

Add a small handwritten repository extension in `internal/repositories/customer_identity_lookup.go` with the contract:

```go
func (r *customerIdentityRepository) GetByCustomerIdentity(
    db *gorm.DB,
    customerID int64,
    externalSource enums.ExternalSource,
    externalID string,
) (*models.CustomerIdentity, error)
```

It accepts the transaction DB when applicable; matches all three columns; rejects invalid inputs; reports not-found distinctly from DB failures; and cannot fall back to source/externalId-only selection. Authorization treats not-found as rejection and DB failure as internal failure; neither may lead to fresh creation. Expiry-only may be classified only after this lookup succeeds. Preserve current identity-status semantics; do not introduce an unrelated disable/revocation policy.

The existing unique index uk_customer_external contains CustomerID + ExternalSource + ExternalID. idx_external_id is nonunique. Global GetBy can remain for authenticated non-guest reuse where that is existing behavior, but it MUST NOT select a guest customer in exchange, token validation, REST ownership, or protected conversation creation. Keep generated CRUD intact. Customer validation also needs error-aware repository reads rather than relying on a legacy Get that collapses all errors to nil; handwritten extensions belong at the repository layer.

## Verified CustomerID Propagation

```text
CustomerSessionToken
  -> CustomerSessionService.VerifyRequest
  -> ExternalUserMiddleware stores verified CustomerID and ExternalUser
  -> HTTP handler reads trusted context
  -> customer-specific service authorization uses that CustomerID
```

Use narrowly scoped httpx context accessors for the verified value. They are populated only from successful server verification, never from a body/query/header customerId. A missing/non-positive context value fails closed. Keep ExternalUser for display/reader/participant metadata; it cannot replace verified CustomerID.

Customer-facing service paths receive the verified ID explicitly or an equivalent trusted internal request value. They compare it to conversation.CustomerID and use exact mapping validation where identity metadata is required. They must not call the old global IsCustomerConversationOwner lookup for guest authority. Do not make HTTP context or permission logic a repository dependency. Existing non-web inbound adapter paths keep their verified-source semantics; do not route them through guest anonymous recovery or require a browser token from them.

## Fresh Guest Creation

Define explicit `CreateFreshGuestCustomer` semantics: require guest source and a valid hint, then atomically create a new Customer plus its CustomerIdentity row in one service-owned transaction. Preserve the caller externalId and current display-name construction. No old-row search, update, detach, reassignment, delete, or merge occurs.

Keep verified external reuse separately expressed, conceptually `EnsureVerifiedExternalCustomer` for signed user identity or equivalent existing verified-adapter operations. Guest continuity uses its already verified ID, not this reuse-by-hint operation. Do not add boolean recovery/security flags to EnsureExternalCustomer. A retained legacy helper must not leave a guest web path capable of hint-only recovery.

Configuration errors must prevent successful fresh exchange; persistence/signing errors are returned through existing project conventions. Do not publish or return a usable session on a partial failure. Fresh customer/identity writes stay atomic; no unrelated transaction refactor is included.

## Authenticated User Preservation

Keep channel-secret verification of the host user JWT, required userId/name/expiry, stable source=user reuse, and existing name synchronization. Guest duplicate handling must not turn signed users into fresh guests. User-session exact validation still uses the signed CustomerID without changing public claims. Invalid user proof never enters guest fallback. Other verified inbound adapters preserve behavior; shared internal signature adaptation may be mechanical only.

## Conversation / History Authorization

Protected create_or_match uses the verified CustomerID established by the session. Its active-conversation lookup is by that customer, never a fresh EnsureExternalCustomer(guest:X). Fresh B can match only B's conversations. Existing channel/product conversation matching semantics are otherwise unchanged.

Conversation detail/close, message list/history/send/read, and conversation-scoped upload authorization compare verified customer to conversation.CustomerID. A client-supplied conversation ID is a resource selector, not ownership proof. Fresh B cannot operate on A's conversation even if it knows the ID and shares X. Reuse of metadata, participants, or read cursors must remain scoped to the authorized conversation.

Ticket and CustomerContact retain their CustomerID relations; no new guest endpoints or inherited associations are added. Fresh identity does not inherit conversation/message/ticket/read/unread/realtime history. No externalId reconciliation, automatic merge, or display-name deduplication is performed. Same-X display-name similarity is cosmetic and does not grant ownership.

## Frontend Identity Transition

Keep localStorage guest hint and sessionStorage customer session storage. Existing valid cached guest session for the matching channel/identity continues to be reused on reload. When exchange is needed, a matching cached proof is supplied only in X-Customer-Session-Token. Do not send a customer token as user Authorization or add host SDK config. Expiry alone may be handled by omitting locally expired proof and fresh exchange; a presented malformed/tampered proof rejection must not trigger an automatic discard-and-anonymous retry that masks rejection. Protected request rejection is an error, not automatic fresh creation inside that operation.

Read customer.id from the ensure/exchange result. Compare with the store's active customer.id; identityKey may remain guest:X for both A and B. Also invalidate scoped state on a deliberate channel switch. Same valid customer/channel retains appropriate state; changed customer triggers the identity transition before loading or exposing B's conversation.

Invalidate the old epoch, disconnect the old realtime manager/socket, and commit one Zustand reset containing the new customer identity and cleared conversation, messages, cursor, hasMore/loadingMore, read cursor/in-flight read ID, initialized, customer-scoped error/action/loading flags, and bootstrap result state. Preserve widget appearance/open/visibility settings. Write accepted new session state coherently with this transition; do not render A's transcript as B's history. Failed fresh bootstrap must not restore invalidated A state.

## Stale Async Isolation

Use a small monotonically increasing identity epoch plus current customer/channel identity. Keep bootstrap attempt generation separate enough to prevent an older exchange/bootstrap from winning a newer attempt. Suspend old-customer writes as soon as a proof replacement/channel/identity transition begins, including the interval before the new customer response arrives.

Capture epoch and customer/channel at the start of each scoped request or socket instance. Before any result, error, finally/loading-flag, session-refresh, or store write, check that the captured scope remains current; discard stale callbacks. Apply this to bootstrap/ensure/exchange, conversation creation, history refresh/sync/pagination, send/upload/close/read actions, and customer realtime callbacks that can mutate the store. Old socket customer_session.refresh and old REST refresh headers must not overwrite B's token or expiry in sessionStorage.

Same-customer token refresh does not change identity epoch, so concurrent valid A requests remain usable. Refresh acceptance checks request/socket identity and epoch rather than requiring raw token equality; concurrent refresh can rotate the token while preserving A. Clearing/departing/replacing a session invalidates its scoped callbacks even if the externalId/identityKey string is unchanged. Epoch state is internal, not a protocol field. No global async framework or employee concurrency redesign is required.

## WebSocket Interaction

PR #3 remains authoritative: verified session CustomerID feeds customer:<CustomerID>, explicit conversation subscriptions compare conversation.CustomerID, and cross-customer/legacy guest destinations remain rejected. There is no externalId routing fallback. Customer event schema, connected payload shape, endpoint, and public handshake inputs are unchanged. Existing WS uses Authorization or customerSessionToken query for protected authentication; the new exchange proof header does not replace that contract.

New B cannot subscribe to customer:A/conversation:A or receive A's events. The frontend disconnects or ignores the old socket on identity transition. PR #6 introduces no customer socket expiry/revocation scheduler; protected handshake verification remains required for new connections.

## Concurrency

- Two no-proof exchanges for the same X may create two fresh customers. Do not lock or deduplicate them by X to recreate hint authority.
- Concurrent valid proof validation/refresh always resolves precisely its encoded CustomerID. Different same-X customer tokens both validate through their own triple.
- A no-proof request racing an old valid session neither changes nor steals the old identity row/customer. Existing valid proof remains valid under current eligibility rules.
- A valid A request finishing after fresh B bootstrap cannot mutate B's frontend/session state. Guards apply to success, failure, refresh, and cleanup callbacks.
- Existing data is never rewritten to make duplicate hints globally unique. No database locking or schema changes are added for this behavior.

## Error Semantics

This table concerns guest customer-session proof; signed host-user verification remains its existing separate flow.

| Condition | Exchange | Protected REST/WS |
|---|---|---|
| No guest proof | Fresh guest | Unauthorized; token required |
| Valid proof | Continue exact CustomerID | Continue exact CustomerID |
| Normally expired proof | Fresh guest | Unauthorized |
| Malformed token / invalid claims | Reject | Unauthorized |
| Invalid signature / unsupported algorithm | Reject | Unauthorized |
| Channel/source/request-hint mismatch | Reject | Unauthorized for applicable session binding |
| Exact identity mapping mismatch/not-found | Reject | Unauthorized |
| Customer deleted/absent/invalid | Reject | Unauthorized |
| Expired plus any rejection condition | Reject; no fresh fallback | Unauthorized |
| DB failure, including during expired-proof classification | Internal error; no fresh fallback | Internal/auth failure, fail closed |
| Signing config secret missing | Existing internal/business error | Fail closed |
| Other internal/signing failure | Error; no successful session | Fail closed |

Use existing errorsx/i18nx, JsonResult, and HTTP status conventions. REST currently may return HTTP 200 with an unauthorized JsonResult, while failed customer WS handshake uses HTTP 401. No new error numbers/status redesign is specified. Client-visible errors omit crypto diagnostics, SQL details, raw tokens, and secrets; new user-visible messages, if required, follow both backend locales.

## Data / Schema

DB migration: NO. Existing identity/customer rewrite: NO. History migration/merge: NO. Existing valid tokens may continue old exact CustomerID; future no-proof bootstrap adds fresh guest rows. Do not rotate customer session secrets or introduce a uniqueness constraint on source/externalId.

Later isolated verification must read deployed index metadata and confirm agreement with model expectations for SQLite/MySQL. An unexpected index is a decision-level discrepancy, not permission to migrate automatically. Runtime verification must not query or modify real production customer history.

## Compatibility

| Contract | Approved impact |
|---|---|
| DB migration | NO |
| REST request | YES: additive optional X-Customer-Session-Token on exchange |
| REST response | NO shape change |
| CustomerSessionToken format | NO CHANGE |
| WS handshake public contract | NO CHANGE |
| WS event schema | NO CHANGE |
| Frontend production | YES: proof bootstrap, CustomerID reset, scoped async isolation |
| SDK public API | NO CHANGE; no new host configuration |
| Authenticated user source | UNCHANGED |
| Customer WS routing | UNCHANGED |
| Dependencies | NO NEW DEPENDENCIES |

Guest clients that previously relied on X alone to recover history intentionally lose that behavior. Same-tab valid-proof reload continues. Token loss/new browser/inactivity past expiry means fresh guest, not recovery. No undocumented externalId-only continuity compatibility is promised.

## Security Invariants

1. Guest externalId MUST NOT authenticate an existing customer.
2. Only a valid server-verifiable CustomerSessionToken may continue an old guest.
3. Missing or normally expired proof MUST NOT trigger old guest lookup/recovery by hint.
4. Malformed/tampered/mismatched proof MUST be rejected, without anonymous downgrade.
5. Verified CustomerID MUST remain authoritative through protected REST.
6. Guest authorization MUST NOT select a customer by global (source, externalId).
7. Exact identity validation MUST match CustomerID AND source AND externalId.
8. Fresh CustomerID MUST NOT inherit/reopen old customer history or associations.
9. Customer WS routing MUST remain verified-CustomerID-authoritative.
10. Signed source=user authentication/reuse MUST remain unchanged.
11. Old/new guest identities MUST NOT be automatically merged or reassigned.
12. Raw customer tokens/secrets MUST NOT appear in application, test, or acceptance logs.
13. DB/config/internal failures MUST NOT become fresh-guest success.
14. Frontend customer identity changes MUST be detected by customer.id, not identityKey alone.
15. Stale old-customer callbacks MUST NOT mutate current store or session storage.
16. Expiry classification MUST NOT hide another invalidity; expired+bad-signature is rejected.
17. Client-provided CustomerID, topic strings, names, and cached IDs MUST NOT establish backend authority.

## Testing Strategy

Later implementation follows TDD for each changed behavior: write a desired-behavior test, run it against pre-fix production behavior and observe the expected failure, make the minimum repair, rerun targeted/regression tests, and review. An already-passing characterization test is not RED evidence. Capture commands/exits without token values. No tests are written or executed during this design stage.

### Backend coverage

- Existing A -> guest:X; no-proof exchange produces B != A, preserves X, and leaves A's mapping/name/history unchanged. This must fail the old reuse behavior.
- Valid TA exchange continues A; two guest:X mappings validate TA as A and TB as B regardless of insertion order. Both first-row orders are covered.
- Missing/expiry-only exchange creates fresh; expired protected REST/new WS handshake rejects without creation. Test authentic expired proof separately from malformed/invalid-signature expired proof.
- Tampered, unsupported algorithm, missing required claims, wrong source, different hint/channel, absent/deleted customer, and exact mapping mismatch reject with no fresh inserts. Include expired+mismatch and expired+tampered precedence.
- Repository not-found versus injected DB failure, config secret missing, and signing/persistence failure fail closed. Assert no successful session or fallback.
- Fresh B cannot detail/list-history/send/read/close/upload against A's conversation. Inject client customerId=A into request selectors and prove verified B remains authoritative.
- B create_or_match never calls global guest lookup/reopens A. Valid A retains its own authorized active conversation.
- Same-X no-proof concurrency permits distinct fresh customers; concurrent valid-token use/refresh stays exact; fresh bootstrap never alters an old mapping.
- Signed user JWT stable reuse/name behavior stays green; invalid user token never becomes guest. Other verified inbound adapters retain behavior.
- PR #3 customer default topics, ownership, cross-customer rejection, publication, presence, and connected/event shapes remain green.

### Frontend coverage

- Valid cached reload preserves matching customer/channel and guest-session cache behavior; an exchange with valid matching proof uses only the dedicated header.
- Lost/expired proof yields B, even with identityKey guest:X unchanged; clear A conversation/messages/cursors/read/loading/action state and disconnect old realtime.
- Controlled promise/barrier: start A history load, switch to B, then resolve A; no A data or error/finally flags are written. Cover send/read/upload/close and bootstrap/exchange winners where they write scoped state.
- Old REST refresh-header and old socket customer_session.refresh callbacks cannot replace B token/expiry; concurrent same-A refresh remains accepted.
- Rejected tampered proof/protected auth failure is not automatically downgraded into a fresh successful operation. Deliberate channel/hint reset is explicit and produces fresh identity.
- SDK configuration and signed user Authorization remain unchanged. Use executable storage/store behavior tests, not only existing source-text assertions. Avoid sleep-only race proofs.

Existing reusable test files include `internal/services/customer_session_service_test.go`, `internal/services/customer_service_test.go`, `internal/pkg/openidentity/openidentity_test.go`, `internal/services/message_service_test.go`, `internal/services/ws_customer_identity_test.go`, `internal/services/ws_customer_identity_integration_test.go`, `internal/services/ws_customer_publication_test.go`, `internal/builders/conversation_builder_test.go`, `web/lib/stores/support-chat-realtime.test.mjs`, and `web/lib/sdk/agent-desk-sdk.test.mjs`. Existing PR #3 collision fixtures primarily compare guest versus user source; add same-source guest duplication coverage.

### Intended later verification gate

Run focused backend tests in services/repositories/customer HTTP middleware/handlers and focused executable frontend session/store tests, followed by `go test -tags dev ./...`, `go vet -tags dev ./...`, `pnpm --dir web typecheck`, frontend tests/build, `task build`, isolated acceptance, `git diff --check`, and independent whole-branch review. Add focused official Linux race tests if implementation changes concurrency-sensitive code.

Verified repository command sources are `web/package.json` and `Taskfile.yml`. Existing frontend tests use `node --test`; for example, from web: `node --test lib/stores/support-chat-realtime.test.mjs lib/sdk/agent-desk-sdk.test.mjs`. The later plan names the new executable tests and exact focused commands.

Task build order: build Flowgram assets using frozen lockfile installation/build, then `pnpm build:sdk` and `pnpm build` in web, then production Go assembly into dist. `web/embed.go` embeds web/out for production, so the production backend build follows static export; the dev build tag uses `web/embed_dev.go`. A task-environment failure is reported honestly and does not waive a gate. These commands are future requirements, not current PASS claims.

## Runtime Acceptance

Later acceptance uses an isolated runtime/database, a synthetic enabled Channel C/agent, synthetic customers/messages with unique markers, and private in-memory/session token handling. No real persistent customer records or secret-bearing output. Record an explicit fixture cleanup boundary before completion.

1. Client A with guest X and no proof obtains fresh A/TA; creates Conversation A and marker A.
2. A with valid TA exchanges through the header and remains A; authorized REST and reconnect WS retain access; near-expiry refresh keeps A.
3. Separate Client B supplies X without TA and receives B != A. B's own bootstrap yields only B's conversation; requests for A detail/history/send/read/close/upload are denied.
4. Real A/B sockets: connected destinations reflect each verified CustomerID; B subscribing to customer:A or conversation:A receives no acknowledgement/registry admission or protected A delivery. Publish marker A and prove only A receives it.
5. Authentic expired proof at exchange creates another fresh customer; at protected API/new WS handshake it rejects. Tampered/malformed proof and expired+tampered reject without fresh creation. Cross-channel/identity mismatch rejects; explicit no-proof channel reset creates fresh identity.
6. Signed source=user retains stable reuse. Verify duplicate same-guest hints, exact TA/TB validation, existing identities untouched, and deployed index metadata.
7. Real frontend: A transcript loaded, proof lost/expired, B bootstrap accepted, old transcript removed. A controlled delayed A callback/old socket refresh cannot restore A content or session into B. No host SDK parameter is added.

Acceptance output includes IDs, marker presence/absence, request classifications, and safe summaries only. Raw tokens/passwords/session secrets are never printed. Direct known-asset URL isolation is excluded by the explicit deferred finding below.

## Deferred Security Finding — Attachment Authorization

The investigation found Asset records without direct CustomerID/ConversationID binding; `validateConversationAsset` checks existence/status, while local static storage in `internal/bootstrap/server.go` can serve already-known URLs without customer authentication. Conversation-scoped upload remains protected, but direct asset authorization is a separate boundary.

A2 prevents unauthorized recovery of old CustomerID/history and thus discovery of old attachment metadata through recovered history. It does NOT prove that an already-known direct URL/AssetID is customer-authorized. Separate security hardening must address that issue. PR #6 does not change storage, Asset schema, download paths, or attachment architecture and must not claim that finding repaired.

## Rollback

No DB migration or data rewrite exists. Code rollback is structurally simple, but restoring global guest externalId lookup/reuse reintroduces the vulnerability, including for intentional duplicate hints. A security-safe emergency response rejects/disables vulnerable guest exchange rather than restoring unauthenticated recovery; retain exact validation for already-issued duplicate-customer tokens. Do not delete, merge, or rewrite new/old identity rows as rollback. No migration rollback is invented.

## Probable File Map

Candidates indicate responsibility, not permission for unrelated edits or a requirement to modify every listed file. New files below are proposed, not present implementation.

| Production candidate | Responsibility |
|---|---|
| `internal/handlers/api/customer_handler.go` | Preserve user flow; pass optional guest header to exchange verification |
| `internal/services/customer_session_service.go` | Dedicated optional proof classification; exact protected validation; issuance/continuity |
| `internal/services/customer_service.go` | Explicit fresh guest primitive; verified external reuse boundary |
| `internal/repositories/customer_identity_lookup.go` (new) | Handwritten error-aware exact identity lookup |
| `internal/repositories/customer_session_lookup.go` (new if needed) | Error-aware customer validation without generated CRUD changes |
| `internal/middleware/chat_middleware.go` | Retain verified CustomerID with ExternalUser |
| `internal/pkg/httpx/context.go` | Trusted customer context accessors |
| `internal/handlers/api/conversation_handler.go` | Pass verified customer into customer conversation operations |
| `internal/services/conversation_service.go` | Exact customer creation/matching/ownership; preserve other adapters |
| `internal/handlers/api/message_handler.go` | Verified customer history/send/read/upload boundary |
| `internal/services/message_service.go` | Customer sender ownership with verified ID, no guest re-resolution |
| `web/lib/api/im.ts` | Optional proof header, session acceptance and scoped refresh/exchange writes |
| `web/lib/stores/support-chat.ts` | CustomerID reset, epoch and old-callback/socket isolation |

Likely new focused tests: `internal/repositories/customer_identity_lookup_test.go`, `internal/handlers/api/customer_recovery_test.go`, `web/lib/api/im-session.test.mjs`, and `web/lib/stores/support-chat-session.test.mjs`, alongside the reusable tests above. Later planning locks executable test interfaces and commands. Existing `web/lib/im-realtime.ts` transport and SDK public configuration need no behavioral change; any small internal socket-callback adaptation must preserve their public contracts.

Models/schema, employee/RBAC code, PR #3 routing, storage remediation, and SDK public config are not production change candidates. Discovering an actual architecture/schema/public-contract contradiction requires a Human Gate, not a silent scope expansion.

## Open Questions

None for implementation architecture.

## Design Self-Review

Placeholder scan: no unresolved placeholders. Internal consistency: missing/expiry-only is fresh, malformed/tampered/mismatch is rejection, infrastructure is failure; user behavior and exact triple validation remain consistent. Scope: one guest recovery PR with frontend identity isolation; attachment hardening remains deferred. Ambiguity: expiry is the sole tolerated validation failure at bootstrap, protected requests never create fresh identities, and CustomerID remains authoritative throughout.

This records design review only. The written spec awaits human review; no implementation plan or execution is authorized by completion of this document alone.
