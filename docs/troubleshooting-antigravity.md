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

- **Request translation (Claude → Antigravity):**
  - **Default (safe):** if *any* assistant `tool_use` exists in history, CLIProxyAPI **suppresses `thinkingConfig`** and **drops all Claude `thinking` blocks** from the outbound request to avoid upstream signed-thinking validation failures.
  - **No tools:** if there is no `tool_use` in history, CLIProxyAPI forwards `thinkingConfig` (`thinkingBudget` + `include_thoughts`) normally.
  - **Experimental override:** you can force thinking even with tool use by setting `CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE=1` (may reintroduce 400s depending on backend validation).
- **Tool calls (Claude → Antigravity):** attaches a Gemini `thoughtSignature` to `functionCall` parts (required by some Gemini/Antigravity endpoints) using a sentinel value: `skip_thought_signature_validator`.
- **Schema sanitization (tools):** removes Gemini-incompatible JSON-Schema keywords (e.g. `propertyNames`, `patternProperties`, `dependentSchemas`, ...) to avoid `400 INVALID_ARGUMENT`.
- **Response translation (Antigravity → Claude):** converts Gemini `thought` text into Claude `thinking` blocks **without emitting a `signature` / `signature_delta`**, because Antigravity/Gemini `thoughtSignature` is not compatible with Anthropic signed-thinking and can break future requests if echoed back.
- **Model support:** treats models ending in `-thinking` as thinking-capable even if registry metadata is missing.

Operational guidance (max quality):

- For maximum thinking quality, start a **fresh conversation** after upgrading. Old sessions with “signature-less tool_use history” will force thinking to be suppressed.
- Ensure `max_tokens` is **greater than** your thinking budget (Claude requirement). If `budget_tokens` >= `max_tokens`, upstream may reject or effectively disable thinking.

## Rollback / How to revert if the experimental override fails

If you enabled `CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE=1` and you start seeing 400s again, revert to the stable mode by:

1. Stop the running server process (find PID with `pgrep -af cli-proxy-api` and `kill <pid>`).
2. Restart **without** the env var:

```bash
nohup ./cli-proxy-api -config "/home/franco/.cliproxy/config.yaml" > logs/server.out 2>&1 &
```

Notes:
- Stable mode is the default; you only need to unset/remove the env var.
- If you want to revert code changes locally (instead of toggling runtime behavior), you can `git restore CLIPROXY/CLIProxyAPI`.

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

