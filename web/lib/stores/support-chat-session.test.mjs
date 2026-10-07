import assert from "node:assert/strict"
import test from "node:test"
import { deferred, loadSupportChatHarness } from "../test/customer-session-harness.mjs"

const now = Date.parse("2026-10-07T01:00:00Z")
const presentation = { title: "Customer support", subtitle: "Here to help", themeColor: "#123456" }
const session = (overrides = {}) => ({
  channelId: "channel-C", identityKey: "guest:hint-X",
  customer: { id: 101, name: "A" }, customerSessionToken: "synthetic-proof-A",
  expiresAt: "2026-10-07T02:00:00Z", ...overrides,
})
const conversation = (id) => ({
  id, channelId: 1, customerName: id === 901 ? "A" : "B", status: 1,
  serviceMode: 1, priority: 0, currentAssigneeId: 0, lastMessageId: id + 1,
  customerUnreadCount: 0, agentUnreadCount: 0, customerLastReadMessageId: id + 1,
  agentLastReadMessageId: 0,
})
const message = (id) => ({
  id: id + 1, conversationId: id, senderType: "customer", senderId: id === 901 ? 101 : 202,
  messageType: "text", content: id === 901 ? "A transcript marker" : "B transcript",
  sendStatus: 1, customerRead: true, agentRead: false,
})

async function initializedA({ exchange = async () => session(), widget = async () => presentation, match = async (id) => conversation(id) } = {}) {
  let matchCount = 0
  const runtime = { channelId: "channel-C", externalId: "hint-X" }
  const harness = await loadSupportChatHarness({
    config: runtime, session: session(), now, scopeControl: "normal",
    request: async (path, options) => {
      if (path.startsWith("/api/channel/config")) return widget(path, options)
      if (path === "/api/customer/session_exchange") return exchange(path, options)
      if (path === "/api/conversation/create_or_match") return match(++matchCount === 1 ? 901 : 911)
      if (path.startsWith("/api/message/list")) {
        const id = Number(new URL(path, "https://fixture.test").searchParams.get("conversationId"))
        return { results: [message(id)], page: { page: 1, limit: 50, total: 1 }, cursor: "older-cursor", hasMore: false }
      }
      throw new Error("Unexpected fixture I/O")
    },
  })
  harness.store.getState().bootstrap()
  await harness.flush()
  assert.equal(harness.store.getState().conversation?.id, 901)
  assert.equal(harness.store.getState().messages[0]?.content, "A transcript marker")
  harness.runtime = runtime
  return harness
}

function writeSession(harness, value) {
  if (value === null) harness.window.sessionStorage.removeItem("cs_ai_agent_customer_session")
  else harness.window.sessionStorage.setItem("cs_ai_agent_customer_session", JSON.stringify(value))
}

test("same guest hint with new customer id clears the transcript", async () => {
  const h = await initializedA({ exchange: async () => session({ customer: { id: 202, name: "B" } }) })
  const previous = h.scope.captureCustomerSessionScope()
  const oldSocket = h.store.getState().socket
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  const state = h.store.getState()
  assert.notEqual(state.conversation?.id, 901, "B must load its conversation instead of retaining A")
  assert.equal(state.messages.some((item) => item.content === "A transcript marker"), false)
  assert.equal(state.customer?.id, 202)
  assert.equal(state.customerChannelId, "channel-C")
  assert.equal(h.scope.isCustomerSessionScopeCurrent(previous), false)
  assert.equal(oldSocket.closed, true)
  for (const [key, value] of Object.entries(presentation)) assert.equal(state[key], value)
  assert.equal(state.isOpen, true)
  assert.equal(state.isVisible, true)
})

function assertDeparted(h, oldScope, oldSocket, channelId = "channel-C") {
  const state = h.store.getState()
  assert.equal(state.conversation, null, "departed transcript is removed before awaiting bootstrap")
  assert.equal(state.messages.length, 0)
  assert.equal(state.messagesCursor, "")
  for (const key of ["messagesHasMore", "messagesLoadingMore", "initialized", "sending", "uploadingAsset", "closingConversation"]) {
    assert.equal(state[key], false, `${key} resets on departure`)
  }
  assert.equal(state.customer, null)
  assert.equal(state.customerChannelId, channelId)
  assert.equal(state.error, "")
  assert.equal(state.readingMessageId, 0)
  assert.equal(state.socket, null)
  assert.equal(state.status, "connecting")
  assert.equal(oldSocket.closed, true)
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), false)
  oldSocket.emit("message", { data: JSON.stringify({ type: "message.created", data: message(901) }) })
  assert.equal(h.store.getState().messages.length, 0, "departed socket cannot repopulate A transcript")
  for (const [key, value] of Object.entries(presentation)) assert.equal(state[key], value)
  assert.equal(state.isOpen, true)
  assert.equal(state.isVisible, true)
}

function dirtyA(h) {
  h.store.setState({ messagesCursor: "902", messagesHasMore: true, messagesLoadingMore: true,
    sending: true, uploadingAsset: true, closingConversation: true, readingMessageId: 902, error: "old error" })
}

function observeAttempts(h) {
  const attempts = []
  const begin = h.im.beginCustomerSessionBootstrap
  h.im.beginCustomerSessionBootstrap = () => { const ticket = begin(); attempts.push(ticket); return ticket }
  return attempts
}

test("near-expiry valid proof renews the same customer without identity reset", async () => {
  const pending = deferred()
  const h = await initializedA({ exchange: async () => pending.promise })
  const oldScope = h.scope.captureCustomerSessionScope(), oldSocket = h.store.getState().socket
  const attempts = observeAttempts(h)
  const before = h.store.getState()
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  const exchanges = h.requests.filter((item) => item.path === "/api/customer/session_exchange")
  assert.equal(exchanges.length, 1)
  assert.ok(exchanges[0].options.headers["X-Customer-Session-Token"] === "synthetic-proof-A", "valid matching proof is presented")
  assert.equal(h.scope.captureCustomerSessionScope().epoch, oldScope.epoch)
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true, "pre-renewal A callback remains current while exchange waits")
  assert.equal(h.store.getState().conversation, before.conversation)
  assert.equal(h.store.getState().messages, before.messages)
  assert.ok(h.store.getState().socket === oldSocket, "same-customer socket is retained")
  assert.equal(attempts.length, 1)
  assert.equal(h.scope.isCustomerSessionAttemptCurrent(attempts[0]), true)
  pending.resolve(session({ customer: { id: 101, name: "A renewed" }, customerSessionToken: "synthetic-proof-renewed" }))
  await h.flush()
  assert.equal(h.scope.captureCustomerSessionScope().epoch, oldScope.epoch)
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(h.store.getState().conversation, before.conversation)
  assert.equal(h.store.getState().messages[0].content, "A transcript marker")
  assert.ok(h.store.getState().socket === oldSocket, "same-customer socket is retained")
  assert.equal(oldSocket.closed, false)
  assert.equal(attempts.length, 1)
  assert.equal(h.scope.isCustomerSessionAttemptCurrent(attempts[0]), true)
  assert.equal(h.store.getState().customer.name, "A renewed")
  assert.ok(h.im.readCustomerSession().customerSessionToken === "synthetic-proof-renewed")
})

test("cached same customer and channel retains initialized conversation and socket", async () => {
  const h = await initializedA()
  const before = h.store.getState(), oldScope = h.scope.captureCustomerSessionScope()
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.store.getState().conversation, before.conversation)
  assert.equal(h.store.getState().initialized, true)
  assert.ok(h.store.getState().socket === before.socket, "cached same-customer socket is retained")
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(h.requests.filter((item) => item.path === "/api/conversation/create_or_match").length, 1)
  assert.equal(h.requests.filter((item) => item.path === "/api/customer/session_exchange").length, 0)
})

test("same-customer renewal preserves connected status on an already-open socket", async () => {
  const pending = deferred()
  const h = await initializedA({ exchange: async () => pending.promise })
  const socket = h.store.getState().socket
  socket.emit("open")
  assert.equal(h.store.getState().status, "connected")
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.store.getState().status, "connected", "retained open socket remains connected while renewing")
  pending.resolve(session())
  await h.flush()
  assert.equal(h.store.getState().status, "connected")
  assert.ok(h.store.getState().socket === socket)
})

for (const [name, change, channelId, identityKey] of [
  ["locally expired proof", (h) => writeSession(h, session({ expiresAt: "2026-10-07T01:00:00Z" })), "channel-C", "guest:hint-X"],
  ["missing proof", (h) => writeSession(h, null), "channel-C", "guest:hint-X"],
  ["channel switch", (h) => { h.runtime.channelId = "channel-D" }, "channel-D", "guest:hint-X"],
  ["hint switch", (h) => { h.runtime.externalId = "hint-Y" }, "channel-C", "guest:hint-Y"],
]) {
  for (const reject of [false, true]) {
    test(`${name} invalidates old identity before fresh bootstrap${reject ? " and failure never restores A" : ""}`, async () => {
      const pending = deferred()
      const h = await initializedA({ exchange: async () => pending.promise })
      const oldScope = h.scope.captureCustomerSessionScope(), oldSocket = h.store.getState().socket
      dirtyA(h)
      change(h)
      h.store.getState().bootstrap()
      assertDeparted(h, oldScope, oldSocket, channelId)
      await h.flush()
      const exchanges = h.requests.filter((item) => item.path === "/api/customer/session_exchange")
      assert.equal(exchanges.length, 1)
      assert.equal("X-Customer-Session-Token" in exchanges[0].options.headers, false, "fresh bootstrap omits departed proof")
      assertDeparted(h, oldScope, oldSocket, channelId)
      if (reject) pending.reject(new Error("fresh bootstrap rejected"))
      else pending.resolve(session({ channelId, identityKey, customer: { id: 202, name: "B" } }))
      await h.flush()
      const state = h.store.getState()
      assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), false)
      assert.equal(state.messages.some((item) => item.content === "A transcript marker"), false)
      if (reject) {
        assert.equal(state.customer, null)
        assert.equal(state.conversation, null)
        assert.equal(state.initialized, false)
        assert.equal(state.socket, null)
        assert.equal(state.status, "disconnected")
        // The external request fixture's Error crosses the VM realm, so the
        // store uses its existing localized fallback for this fixture rejection.
        assert.equal(state.error, "supportChat.initFailed")
      } else {
        assert.equal(state.customer?.id, 202)
        assert.equal(state.customerChannelId, channelId)
        assert.equal(state.conversation.id, 911)
      }
      assert.equal(h.requests.filter((item) => item.path === "/api/customer/session_exchange").length, 1)
    })
  }
}

test("unexpected different customer advances epoch once and atomically installs cleared state", async () => {
  const pending = deferred(), conversationPending = deferred()
  // Hold B's external I/O while retaining the real conversation API call.
  const h = await initializedA({ exchange: async () => pending.promise,
    match: async (id) => id === 911 ? conversationPending.promise : conversation(id) })
  const oldScope = h.scope.captureCustomerSessionScope(), oldSocket = h.store.getState().socket
  dirtyA(h)
  const resetStates = []
  const unsubscribe = h.store.subscribe((state) => resetStates.push(state))
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(oldSocket.closed, false)
  resetStates.length = 0
  pending.resolve(session({ customer: { id: 202, name: "B" } }))
  await h.flush()
  assert.equal(h.scope.captureCustomerSessionScope().epoch, oldScope.epoch + 1)
  const state = h.store.getState()
  assert.equal(state.conversation, null)
  assert.equal(state.messages.length, 0)
  assert.equal(state.messagesCursor, "")
  for (const key of ["messagesHasMore", "messagesLoadingMore", "initialized", "sending", "uploadingAsset", "closingConversation"]) assert.equal(state[key], false)
  assert.equal(state.readingMessageId, 0)
  assert.equal(state.error, "")
  assert.equal(state.socket, null)
  assert.equal(state.status, "connecting")
  assert.equal(oldSocket.closed, true)
  assert.equal(state.customer?.id, 202)
  assert.equal(state.customerChannelId, "channel-C")
  assert.equal(resetStates.length, 1, "accepted new identity reset uses one store update")
  assert.equal(resetStates[0].customer?.id, 202)
  assert.equal(resetStates[0].customerChannelId, "channel-C")
  assert.equal(resetStates[0].messages.length, 0)
  assert.equal(resetStates[0].conversation, null)
  unsubscribe()
  conversationPending.resolve(conversation(911))
  await h.flush()
})

test("matching still-unexpired proof rejection performs one exchange without fresh fallback", async () => {
  const h = await initializedA({ exchange: async () => { throw new Error("presented proof rejected") } })
  const before = h.store.getState(), oldScope = h.scope.captureCustomerSessionScope()
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.requests.filter((item) => item.path === "/api/customer/session_exchange").length, 1)
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(h.store.getState().conversation, before.conversation)
  assert.equal(h.store.getState().messages, before.messages)
  assert.equal(h.store.getState().customer?.id, 101)
  assert.equal(h.store.getState().error, "supportChat.initFailed")
})

test("deliberate close and hide with usable proof do not create a fresh identity", async () => {
  const h = await initializedA()
  const oldScope = h.scope.captureCustomerSessionScope(), before = h.store.getState()
  h.store.getState().setIsOpen(false)
  h.store.getState().setIsVisible(false)
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(h.store.getState().conversation, before.conversation)
  assert.equal(h.store.getState().messages, before.messages)
  assert.equal(h.store.getState().isOpen, false)
  assert.equal(h.store.getState().isVisible, false)
  h.store.getState().setIsOpen(true)
  h.store.getState().bootstrap()
  await h.flush()
  assert.equal(h.scope.isCustomerSessionScopeCurrent(oldScope), true)
  assert.equal(h.requests.filter((item) => item.path === "/api/customer/session_exchange").length, 0)
})

test("widget channel change departs identity and starts a ticket before exchange", async () => {
  const pending = deferred()
  let changed = false
  const h = await initializedA({ widget: async () => ({ ...presentation, ...(changed ? { channelId: "channel-D" } : {}) }), exchange: async () => pending.promise })
  const oldScope = h.scope.captureCustomerSessionScope(), oldSocket = h.store.getState().socket
  const attempts = observeAttempts(h)
  changed = true
  h.store.getState().bootstrap()
  await h.flush()
  assertDeparted(h, oldScope, oldSocket, "channel-D")
  assert.equal(attempts.length, 2)
  assert.equal(h.scope.isCustomerSessionAttemptCurrent(attempts[0]), false)
  assert.equal(h.scope.isCustomerSessionAttemptCurrent(attempts[1]), true)
  const exchange = h.requests.find((item) => item.path === "/api/customer/session_exchange")
  assert.equal("X-Customer-Session-Token" in exchange.options.headers, false)
  pending.resolve(session({ customer: { id: 202, name: "B" } }))
  await h.flush()
  assert.equal(h.store.getState().customerChannelId, "channel-D")
})

test("superseded widget result cannot change the winner channel or presentation", async () => {
  const pending = deferred()
  let widgetCalls = 0
  const h = await initializedA({ widget: async () => (++widgetCalls === 2 ? pending.promise : presentation) })
  h.store.getState().bootstrap()
  h.store.getState().bootstrap()
  await h.flush()
  pending.resolve({ channelId: "channel-D", title: "stale widget" })
  await h.flush()
  assert.equal(h.store.getState().title, presentation.title)
  assert.equal(h.store.getState().conversation.id, 901)
  assert.equal(h.requests.filter((item) => item.path === "/api/customer/session_exchange").length, 0)
})

test("losing bootstrap rejection cannot overwrite accepted winner state", async () => {
  const older = deferred(), winner = deferred()
  let exchanges = 0
  const h = await initializedA({ exchange: async () => (++exchanges === 1 ? older.promise : winner.promise) })
  writeSession(h, session({ expiresAt: "2026-10-07T01:00:03Z" }))
  h.store.getState().bootstrap()
  await h.flush()
  h.store.getState().bootstrap()
  await h.flush()
  winner.resolve(session({ customer: { id: 202, name: "B" } }))
  await h.flush()
  const accepted = h.store.getState()
  assert.equal(accepted.conversation?.id, 911, "winner B conversation is loaded")
  older.reject(new Error("older exchange rejected"))
  await h.flush()
  assert.equal(h.store.getState().conversation, accepted.conversation)
  assert.equal(h.store.getState().messages, accepted.messages)
  assert.equal(h.store.getState().customer?.id, 202)
  assert.equal(h.store.getState().error, "")
})
