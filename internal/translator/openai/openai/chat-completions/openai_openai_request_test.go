package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToOpenAI_RemovesToolsDeferLoadingObject(t *testing.T) {
	input := []byte(`{
		"model":"old-model",
		"messages":[{"role":"user","content":"hello"}],
		"tools":{"defer_loading":true,"type":"web_search_preview"}
	}`)

	out := ConvertOpenAIRequestToOpenAI("new-model", input, false)
	parsed := gjson.ParseBytes(out)

	if got := parsed.Get("model").String(); got != "new-model" {
		t.Fatalf("model = %q, want %q", got, "new-model")
	}
	if parsed.Get("tools.defer_loading").Exists() {
		t.Fatalf("tools.defer_loading should be removed")
	}
}

func TestConvertOpenAIRequestToOpenAI_RemovesToolsDeferLoadingArrayItems(t *testing.T) {
	input := []byte(`{
		"model":"old-model",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[
			{"type":"function","defer_loading":true,"function":{"name":"foo","description":"x","parameters":{"type":"object"}}},
			{"type":"web_search_preview","defer_loading":false}
		]
	}`)

	out := ConvertOpenAIRequestToOpenAI("new-model", input, false)
	parsed := gjson.ParseBytes(out)

	if got := parsed.Get("model").String(); got != "new-model" {
		t.Fatalf("model = %q, want %q", got, "new-model")
	}
	if parsed.Get("tools.0.defer_loading").Exists() || parsed.Get("tools.1.defer_loading").Exists() {
		t.Fatalf("tools[i].defer_loading should be removed")
	}
}
