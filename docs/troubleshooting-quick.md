# Troubleshooting Quick: CLIProxyAPI + Claude Code (Franco)

This is a 1-page checklist for when Claude Code “stops”, tools fail mid-run, or the proxy starts returning 4xx/5xx.

## 0) First checks (always)

- Is the proxy up?
  - `ss -ltnp '( sport = :8317 )'`
- Check last startup + client count:
  - `tail -n 50 logs/server.out`

## 1) Auth / 401: “Missing API key”

Symptoms:
- `401 Unauthorized {"error":"Missing API key"}` when calling `/v1/models` or `/v1/messages`.

Fix:
- Send `Authorization: Bearer <api-key>` (from `~/.cliproxy/config.yaml` `api-keys`).

### API key rotation (what changes in practice)

- Rotating the *local proxy* API key does **not** change model quality or stability; it only changes **who can call the proxy**.
- Anything that uses `Authorization: Bearer <api-key>` must be updated (Claude Code env/config, curl scripts, SDK clients, etc.).
- Old key stops working as soon as it’s removed from `api-keys`.

Minimal checklist:
1. Add a new key to `~/.cliproxy/config.yaml` `api-keys`.
2. Update clients to use the new key.
3. Remove the old key from `api-keys`.
4. Restart the proxy so it reloads config.

## 2) Tools “param do nada” / “No tool call found …” / tool loop corruption

Symptoms:
- Tools start failing after an interrupt (Ctrl+C), timeout, network hiccup, or 429.
- Errors like missing `tool_result`, orphan `tool_use_id`, or downstream “No tool call found …”.

Root cause:
- Tool loops require strict ordering: every `tool_use` must be followed by a matching `tool_result`.
- Streaming (`text/event-stream`) interruptions can drop part of the stream around tool calls, corrupting history.

Mitigation (CLIProxyAPI):
- Proxy forces **non-streaming JSON** when the request includes `tools` / `tool_choice` or tool-use history.
- Disable only if needed: `CLIPROXY_DISABLE_NONSTREAM_TOOLS_GUARD=1`.

Quick verification:

1) No tools => streaming stays on:
```bash
curl -sS -D /tmp/h.out -o /tmp/b.out -X POST 'http://127.0.0.1:8317/v1/messages' \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer <API_KEY>' \
  -d '{"model":"gpt-5.2","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}'
head -n 8 /tmp/h.out
# expect: Content-Type: text/event-stream
```

2) With tools => forced JSON (no SSE):
```bash
curl -sS -D /tmp/h2.out -o /tmp/b2.out -X POST 'http://127.0.0.1:8317/v1/messages' \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer <API_KEY>' \
  -d '{"model":"gpt-5.2","stream":true,"max_tokens":16,"tools":[{"name":"add","description":"Add two integers","input_schema":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}}],"messages":[{"role":"user","content":"Call add with a=2 b=2"}]}'
head -n 8 /tmp/h2.out
# expect: Content-Type: application/json
```

## 3) 429 / model_cooldown / usage_limit_reached

Symptoms:
- `429` with cooldown messaging, sometimes with `Retry-After`.

Fix:
- Respect `Retry-After`.
- Reduce concurrency/bursts.
- Add more accounts/projects in `~/.cli-proxy-api/` if needed.

## 4) Network timeouts / i/o timeout

Symptoms:
- OAuth refresh or upstream calls fail with `dial tcp ... i/o timeout`.

Fix:
- Verify system DNS/network.
- If using upstream proxies (socks/http), confirm `proxy-url` in `~/.cliproxy/config.yaml`.

## 5) Antigravity “thinking + tools” 400s

Symptoms:
- `400 invalid_request_error: Expected thinking, but found tool_use` / missing `thinking.signature`.

Fix:
- See `docs/troubleshooting-antigravity.md`.
- Default behavior suppresses thinking when tool_use exists in history.
- Experimental override: `CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE=1`.

## 6) MCP tools + GPT-5.2 (Codex) "para do nada" / random stops

Symptoms:
- Claude Code stops mid-task when using MCP tools (e.g., `sequential-thinking`) with GPT-5.2.
- Background tasks fail with "No task found with ID: xxx".
- Session becomes unresponsive after heavy MCP tool usage.

Root cause (NOT CLIProxy):
- This is a **Claude Code CLI bug** (see GitHub issue #7291), not a CLIProxy issue.
- CLIProxy logs show `response.completed` arriving correctly - the proxy is working fine.
- The bug is in Claude Code CLI's handling of background tasks and MCP tool call state.
- Race conditions in task ID tracking when MCP tools take time to respond.

Investigation results (2025-12-25):
- Analyzed CLIProxy logs: no stream disconnect errors, no 408 errors.
- Codex executor correctly receives and processes all SSE events.
- All requests show `response.completed` event arriving as expected.
- The failure occurs AFTER CLIProxy delivers the response correctly.

Workarounds:
1. **Avoid `run_in_background: true`** for critical tasks using MCP tools - run in foreground instead.
2. **Use `/clear`** immediately if session becomes unstable after MCP+Codex usage.
3. **Limit rapid MCP calls** - don't chain many `sequential-thinking` calls quickly.
4. **Consider alternative models** for heavy MCP workloads - opus/sonnet may be more stable than gpt-5.2 for complex tool orchestration.
5. **Start fresh sessions** proactively after long MCP-heavy work.

What does NOT help:
- Restarting CLIProxy (it's not the cause).
- Changing CLIProxy config/timeouts.
- Adding more auth tokens.

## 7) Recovery playbook (fast)

- If a session is corrupted: start a fresh Claude Code thread (`/clear` or new session).
- If proxy is unstable: restart it and re-run the 2 curl probes above.

