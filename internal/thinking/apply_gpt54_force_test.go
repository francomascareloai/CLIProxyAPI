package thinking_test

import (
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyThinking_DefaultsBareGPT54ToXHighOnCodex(t *testing.T) {
	t.Parallel()

	body := []byte(`{"reasoning":{"effort":"low"}}`)
	out, err := thinking.ApplyThinking(body, "gpt-5.4", "claude", "codex", "codex")
	if err != nil {
		t.Fatalf("ApplyThinking returned error: %v", err)
	}

	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("expected reasoning.effort=xhigh, got %q", got)
	}
}

func TestApplyThinking_RespectsExplicitGPT54SuffixOnCodex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  string
	}{
		{model: "gpt-5.4(medium)", want: "medium"},
		{model: "gpt-5.4(high)", want: "high"},
		{model: "gpt-5.4(xhigh)", want: "xhigh"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()

			body := []byte(`{"reasoning":{"effort":"low"}}`)
			out, err := thinking.ApplyThinking(body, tc.model, "claude", "codex", "codex")
			if err != nil {
				t.Fatalf("ApplyThinking returned error: %v", err)
			}

			if got := gjson.GetBytes(out, "reasoning.effort").String(); got != tc.want {
				t.Fatalf("expected reasoning.effort=%s, got %q", tc.want, got)
			}
		})
	}
}
