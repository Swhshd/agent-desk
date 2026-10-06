import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import test from "node:test"

const realtimeSource = await readFile(
  new URL("../im-realtime.ts", import.meta.url),
  "utf8"
)
const supportChatSource = await readFile(
  new URL("./support-chat.ts", import.meta.url),
  "utf8"
)

test("customer WebSocket keeps the existing endpoint and does not build a topic", () => {
  assert.match(
    realtimeSource,
    /new WebSocket\([\s\S]*?`\$\{baseUrl\}\/api\/ws\/open\?channelId=\$\{channelId\}&customerSessionToken=\$\{encodeURIComponent\(customerSessionToken\)\}`\s*\)/
  )
  assert.match(realtimeSource, /const channelId = encodeURIComponent\(config\.channelId \|\| ""\)/)
  assert.match(realtimeSource, /const customerSessionToken = getCustomerSessionToken\(\)/)
  assert.doesNotMatch(realtimeSource, /customer:\$\{|guest:\$\{/)
  assert.doesNotMatch(supportChatSource, /customer:\$\{|guest:\$\{/)
})

test("connected topic values are ignored and customer events use payload conversationId", () => {
  assert.match(supportChatSource, /onMessage:\s*\(messageEvent\)\s*=>\s*\{/)
  assert.doesNotMatch(supportChatSource, /event\.topic/)
  assert.match(supportChatSource, /if\s*\(payload\?\.conversationId !== conversationId\)\s*\{\s*return\s*\}/)
  assert.doesNotMatch(supportChatSource, /connected\.data\.topics/)
  assert.doesNotMatch(supportChatSource, /customer:\$\{|guest:\$\{/)
})
