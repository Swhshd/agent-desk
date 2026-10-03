# Agent Tool Provider-Safe Alias Design

Date: 2026-10-03

## Goal

Keep AgentDesk capability identities unchanged internally while exposing provider-compatible function names to Eino and OpenAI-compatible chat providers. A model-returned provider alias must resolve to exactly the original internal capability before AgentDesk authorization or execution begins.

## Current behavior and root cause

The Agent Loop builds tool definitions from agentLoopTurn.AllowedTools. These include builtin and graph codes such as builtin/conversation_context, Skill codes such as skill/7, Workflow codes such as workflow/47, and MCP codes such as crm/update_customer. agentLoopToolDefinitions currently places each code directly into ai.ToolDefinition.Name.

The Eino adapter copies that name to schema.ToolInfo.Name. Eino registers and dispatches tools using that name, so the OpenAI-compatible provider receives slash-containing function names and rejects the request before a tool can execute. The Phase 0 AgentRun reproduced this with Invalid tools[1].function.name and the provider pattern ^[a-zA-Z0-9_-]+$.

After dispatch, the existing executor relies on the original code for Agent allow-list checks, Skill policy, risk/confirmation checks, MCP and Workflow selection, audit records, and business-tool idempotency. Those meanings and identities must remain unchanged.

## Approved boundary

The mapping exists only for one Eino model/tool-set construction:

Internal definitions -> provider-safe ToolInfo names -> Eino dispatch -> alias lookup -> original ai.ToolCall.Name -> existing Agent Loop executor.

No provider alias reaches authorization, persistence, audit identity, idempotency, Skill, MCP, Workflow, public API, or database schema. Tool descriptions and JSON schemas remain associated with their original definitions.

tool_search and conversation_decision remain their existing provider-facing reserved names. They are included in final-set name validation. Every other tool definition, including already-valid internal identifiers, receives a deterministic provider alias so all capability identities follow one rule and cannot collide with reserved names by direct pass-through.

## Alias construction

For each non-reserved internal name, generate:

cap_<sanitized-readable-prefix>_<stable-hash>

- Sanitize the complete original internal name to ASCII letters, digits, underscore, and hyphen; replace every other rune with underscore. Reject an all-whitespace identity without normalizing a nonblank identity.
- Trim leading/trailing separators from the readable part and use tool if it is empty.
- Truncate the readable part to at most 35 ASCII bytes.
- Append the first 24 lowercase hexadecimal characters of SHA-256 over the complete original internal identity exactly as supplied.
- Prefix with cap_ and separate the hash with _. The resulting maximum is 64 characters.
- Validate non-empty, allowed ASCII character set, and maximum length for every final ToolInfo name, including reserved names.
- Build explicit alias -> original internal name and original internal name -> alias maps for this one tool set. Preserve the exact original identity in both directions. Duplicate internal definitions, duplicate aliases, or collisions with reserved names fail before Eino agent creation. There is no overwrite or suffix retry behavior.
- Unknown aliases fail closed and never fall back to treating an alias as an internal code.

The readable segment is only a hint. The complete identity hash distinguishes names that sanitize to the same text and long identities whose readable prefix is truncated. A hash collision is still checked and fails fast.

## Eino adapter behavior

The adapter produces Eino tools from the original definitions and the completed alias map. Each wrapper returns its provider alias from Info(), preserving description and parameter schema. On invocation, the wrapper resolves the ToolInfo name through the explicit alias map; only the resulting original name is passed to the existing executor. The wrapper returns a safe error if the mapping is absent.

Eino v0.9.6 APIs were inspected locally: ReAct collects names from each tool's Info(), and the ToolNode dispatches a model call by the returned function name to that registered tool. Therefore the wrapper is the correct boundary and no dependency upgrade is needed.

## Compatibility and safety

- No global capability rename, migration, API/protocol change, or provider-specific workaround.
- Existing internal ToolCode-based allow lists, confirmation flow, execution policy, business tool idempotency, audit, and persistence continue unchanged.
- Alias state is per Eino invocation and never persisted.
- Alias construction and unknown-alias errors must not include API keys or other provider credentials.
- Unknown model-returned names are rejected by Eino dispatch; the wrapper also rejects any alias lookup miss before calling the executor.

## Verification contract

Unit tests exercise a registered Eino tool from ToolInfo creation through InvokableRun dispatch, proving alias-to-original mapping. Coverage includes slash-containing and valid internal names, sanitize collisions, long identities, 64-character bound, allowed character set, final-set uniqueness including reserved names, unknown aliases, and duplicate-alias failure. A runtime test exercises the mapped wrapper with the existing low-risk conversation-context executor.

Phase 0 runtime validation must additionally prove provider acceptance, at least one real tool call, the correct original capability executing, the result returning to the Agent loop, a completed AgentRun, a persisted assistant message readable by Support Chat API and UI, and regressions for Normal Chat, embedding, Qdrant, knowledge retrieval, and RAG.

## Non-goals

Visitor identity, WebSocket authorization or races, RBAC, outbox work, Agent concurrency redesign, RAG threshold or reranker changes, migrations, frontend redesign, deployment, and remote Git operations.
