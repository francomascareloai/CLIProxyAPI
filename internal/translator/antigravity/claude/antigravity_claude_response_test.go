package claude

import (
	"context"
	"strings"
	"testing"
)

func TestConvertAntigravityResponseToClaude_ThoughtSignatureStartsThinkingBlock(t *testing.T) {
	raw := []byte(`{
  "response": {
    "candidates": [
      {
        "content": {
          "role": "model",
          "parts": [
            {"thought": true, "thoughtSignature": "sig_only_no_text"},
            {"thought": true, "text": "reasoning text"},
            {"functionCall": {"name": "doThing", "args": {"x": 1}}}
          ]
        },
        "finishReason": "STOP"
      }
    ],
    "usageMetadata": {
      "promptTokenCount": 10,
      "candidatesTokenCount": 5,
      "thoughtsTokenCount": 7,
      "totalTokenCount": 22
    },
    "modelVersion": "claude-opus-4-5-thinking",
    "responseId": "msg_test"
  }
}`)

	var p any
	out := ConvertAntigravityResponseToClaude(context.Background(), "gemini-claude-opus-4-5-thinking", nil, nil, raw, &p)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}
	chunk := out[0]
	if !strings.Contains(chunk, `"content_block_start"`) || !strings.Contains(chunk, `"type":"thinking"`) {
		t.Fatalf("expected thinking block start in chunk, got: %s", chunk)
	}
	if !strings.Contains(chunk, `"signature_delta"`) || !strings.Contains(chunk, "sig_only_no_text") {
		t.Fatalf("expected signature_delta for thoughtSignature in chunk, got: %s", chunk)
	}
	if !strings.Contains(chunk, `"thinking_delta"`) || !strings.Contains(chunk, "reasoning text") {
		t.Fatalf("expected thinking_delta with text in chunk, got: %s", chunk)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_ThinkingIncludesSignature(t *testing.T) {
	raw := []byte(`{
  "response": {
    "candidates": [
      {
        "content": {
          "role": "model",
          "parts": [
            {"thought": true, "thoughtSignature": "sig1"},
            {"thought": true, "text": "hidden"},
            {"text": "visible"}
          ]
        },
        "finishReason": "STOP"
      }
    ],
    "usageMetadata": {
      "promptTokenCount": 1,
      "candidatesTokenCount": 1,
      "thoughtsTokenCount": 1,
      "totalTokenCount": 3
    },
    "modelVersion": "claude-opus-4-5-thinking",
    "responseId": "msg_test"
  }
}`)

	out := ConvertAntigravityResponseToClaudeNonStream(context.Background(), "gemini-claude-opus-4-5-thinking", nil, nil, raw, nil)
	if !strings.Contains(out, `"type":"thinking"`) || !strings.Contains(out, `"signature":"sig1"`) {
		t.Fatalf("expected thinking block with signature, got: %s", out)
	}
}
