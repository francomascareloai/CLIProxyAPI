package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_RemovesDeferLoadingFromTools(t *testing.T) {
	input := []byte(`{
		"model":"claude-test",
		"messages":[{"role":"user","content":[{"type":"text","text":"oi"}]}],
		"tools":[
			{
				"name":"tool_a",
				"description":"desc",
				"defer_loading":true,
				"input_schema":{"type":"object","properties":{"q":{"type":"string"}}}
			},
			{
				"name":"tool_b",
				"description":"desc",
				"deferLoading":true,
				"input_schema":{"type":"object","properties":{"q":{"type":"string"}}}
			}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.3-codex", input, false)
	parsed := gjson.ParseBytes(out)

	if parsed.Get("tools.0.defer_loading").Exists() || parsed.Get("tools.0.deferLoading").Exists() {
		t.Fatalf("tools.0 defer_loading/deferLoading should be removed")
	}
	if parsed.Get("tools.1.defer_loading").Exists() || parsed.Get("tools.1.deferLoading").Exists() {
		t.Fatalf("tools.1 defer_loading/deferLoading should be removed")
	}
}
