# Troubleshooting: Antigravity (Claude/Gemini) via CLIProxyAPI

This guide focuses on failure modes seen with Antigravity-backed models (Claude/Gemini), especially when using Claude Code with extended thinking and tools.

## 400 invalid_request_error: Expected thinking, but found tool_use (or missing thinking.signature)

Symptoms (examples):

- `messages.N.content.0.type: Expected thinking or redacted_thinking, but found tool_use`
- `messages.N.content.0.thinking.signature: Field required`

Root cause:

- When *thinking is enabled*, some Claude implementations require that **any assistant message containing `tool_use`** starts with a **`thinking` block that includes a valid `signature`**.
- If the conversation history contains tool-use messages **without** those thinking signatures (common after prior client/proxy bugs), enabling thinking later can hard-fail with `400 INVALID_ARGUMENT`.

What CLIProxyAPI now does:

- **Request translation (Claude → Antigravity):** only forwards `thinkingConfig` when the request history appears compatible (i.e., assistant `tool_use` messages start with a `thinking.signature`). If history is not compatible, CLIProxyAPI suppresses thinking for that request to avoid hard 400s.
- **Response translation (Antigravity → Claude):** preserves upstream `thoughtSignature` and ensures Claude Code receives `thinking` + `signature_delta` in the correct order (even when the provider sends a signature without any thought text).
- **Model support:** treats models ending in `-thinking` as thinking-capable even if registry metadata is missing.

Operational guidance (max quality):

- For maximum thinking quality, start a **fresh conversation** after upgrading. Old sessions with “signature-less tool_use history” will force thinking to be suppressed.
- Ensure `max_tokens` is **greater than** your thinking budget (Claude requirement). If `budget_tokens` >= `max_tokens`, upstream may reject or effectively disable thinking.

## 400 invalid_request_error: tool_use without tool_result

Symptoms:

- Tool-use IDs are present, but no `tool_result` blocks follow immediately.

Fix:

- Ensure your client always appends a `tool`/`tool_result` message right after each tool call (matching the same `tool_use`/`tool_call_id`).

## 400 INVALID_ARGUMENT: Unknown name "$ref" (tools schema)

Symptoms:

- `Unknown name "$ref" at request.tools[...]...`

Cause:

- Some Antigravity/Gemini endpoints reject JSON Schemas that contain `$ref` / `$defs`.

Fix:

- Inline schemas (avoid `$ref` / `$defs`) or sanitize tool schemas before sending upstream.

## 403 PERMISSION_DENIED: SUBSCRIPTION_REQUIRED after a 429

Symptoms:

- First: `429 RESOURCE_EXHAUSTED RATE_LIMIT_EXCEEDED`
- Then: `403 PERMISSION_DENIED SUBSCRIPTION_REQUIRED`

Cause:

- The executor fell back to another base URL/project that requires a Gemini Code Assist license.

Fix:

- Pin Antigravity to the intended base URL by setting `base_url` on the auth JSON (top-level).

## 429 model_cooldown (all credentials cooling down)

Symptoms:

- `429` with message like: `All credentials for model ... are cooling down`
- `Retry-After: <seconds>`

Fix:

- Respect `Retry-After` and reduce concurrency/bursts, or add more accounts/projects.

