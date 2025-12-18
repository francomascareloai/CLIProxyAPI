package claude

import (
	"bytes"
	"testing"
)

func TestConvertClaudeRequestToAntigravity_DoesNotInjectThoughtSignature(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "thinking": {"type":"enabled","budget_tokens":1024},
  "messages": [
    {"role":"user","content":[{"type":"text","text":"hi"}]},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"secret","signature":""},
      {"type":"tool_use","id":"toolu_123","name":"doThing","input":{"x":1}}
    ]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"ok"}]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	// Tool calls must include a thoughtSignature for Gemini tool validation, but we must not
	// inject fake signatures into thinking parts (which can trigger "Corrupted thought signature").
	if !bytes.Contains(out, []byte(`"functionCall"`)) {
		t.Fatalf("expected functionCall in output, got: %s", string(out))
	}
	if !bytes.Contains(out, []byte(`"thoughtSignature":"skip_thought_signature_validator"`)) {
		t.Fatalf("expected thoughtSignature on functionCall part, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"type":"thinking"`)) && bytes.Contains(out, []byte(`"signature":"skip_thought_signature_validator"`)) {
		t.Fatalf("must not inject thought signature into thinking blocks, got: %s", string(out))
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingToolUsePrependsThought(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "max_tokens": 128,
  "thinking": {"type":"enabled","budget_tokens":1024},
  "messages": [
    {"role":"user","content":[{"type":"text","text":"hi"}]},
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"doThing","input":{"x":1}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"ok"}]},
    {"role":"user","content":[{"type":"text","text":"continue"}]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	// When history contains assistant tool_use without a thinking signature, do not enable thinkingConfig
	// to avoid upstream 400 validation errors.
	if bytes.Contains(out, []byte(`"thinkingConfig"`)) {
		t.Fatalf("expected thinkingConfig to be suppressed for incompatible history, got: %s", string(out))
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingToolUseDoesNotDuplicateThought(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "thinking": {"type":"enabled","budget_tokens":1024},
  "messages": [
    {"role":"user","content":[{"type":"text","text":"hi"}]},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"x","signature":"sig"},
      {"type":"tool_use","id":"toolu_123","name":"doThing","input":{"x":1}}
    ]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	// Tool use history => suppress thinkingConfig to avoid upstream signature validation failures.
	if bytes.Contains(out, []byte(`"thinkingConfig"`)) {
		t.Fatalf("expected thinkingConfig to be suppressed when tool_use exists, got: %s", string(out))
	}
	// We should not forward Anthropic thinking signatures to Antigravity.
	if bytes.Contains(out, []byte(`"thoughtSignature":"sig"`)) {
		t.Fatalf("must not forward Anthropic thinking signatures, got: %s", string(out))
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingNoToolUse_ForwardsThinkingConfig(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "max_tokens": 256,
  "thinking": {"type":"enabled","budget_tokens":1024},
  "messages": [
    {"role":"user","content":[{"type":"text","text":"hi"}]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	if !bytes.Contains(out, []byte(`"thinkingConfig":{"thinkingBudget":1024,"include_thoughts":true}`)) {
		t.Fatalf("expected thinkingConfig to be forwarded, got: %s", string(out))
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotAlterThinkingTextWhenSignaturePresent(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "thinking": {"type":"enabled","budget_tokens":128},
  "messages": [
    {"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	if bytes.Contains(out, []byte(`"thoughtSignature":"sig"`)) {
		t.Fatalf("must not forward thinking signatures, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"thought":true`)) {
		t.Fatalf("must not forward thinking blocks to Antigravity, got: %s", string(out))
	}
}

func TestConvertClaudeRequestToAntigravity_ExperimentalThinkingWithToolUse_ForwardsThinkingConfig(t *testing.T) {
	t.Setenv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE", "1")
	in := []byte(`{
  "model": "gemini-claude-opus-4-5-thinking",
  "thinking": {"type":"enabled","budget_tokens":1024},
  "messages": [
    {"role":"user","content":[{"type":"text","text":"hi"}]},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"x","signature":"sig"},
      {"type":"tool_use","id":"toolu_123","name":"doThing","input":{"x":1}}
    ]}
  ]
}`)

	out := ConvertClaudeRequestToAntigravity("gemini-claude-opus-4-5-thinking", in, false)
	if !bytes.Contains(out, []byte(`"thinkingConfig":{"thinkingBudget":1024,"include_thoughts":true}`)) {
		t.Fatalf("expected thinkingConfig to be forced on, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"thoughtSignature":"sig"`)) {
		t.Fatalf("must not forward Anthropic thinking signatures, got: %s", string(out))
	}
	if !bytes.Contains(out, []byte(`"thoughtSignature":"skip_thought_signature_validator"`)) {
		t.Fatalf("expected tool thoughtSignature, got: %s", string(out))
	}
}
