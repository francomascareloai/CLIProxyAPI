package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestApplyClaudeOpenAIPriorityConfig(t *testing.T) {
	cfg := &config.Config{ClaudeOpenAIPriority: true}

	t.Run("injects priority for claude requests when tier absent", func(t *testing.T) {
		payload := []byte(`{"model":"gpt-5.4","messages":[]}`)
		got := applyClaudeOpenAIPriorityConfig(cfg, sdktranslator.FromString("claude"), payload)
		if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
			t.Fatalf("service_tier = %q, want %q", tier, "priority")
		}
	})

	t.Run("upgrades auto to priority when toggle is enabled", func(t *testing.T) {
		payload := []byte(`{"model":"gpt-5.4","messages":[],"service_tier":"auto"}`)
		got := applyClaudeOpenAIPriorityConfig(cfg, sdktranslator.FromString("claude"), payload)
		if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
			t.Fatalf("service_tier = %q, want %q", tier, "priority")
		}
	})

	t.Run("preserves explicit non-priority tier", func(t *testing.T) {
		payload := []byte(`{"model":"gpt-5.4","messages":[],"service_tier":"default"}`)
		got := applyClaudeOpenAIPriorityConfig(cfg, sdktranslator.FromString("claude"), payload)
		if tier := gjson.GetBytes(got, "service_tier").String(); tier != "default" {
			t.Fatalf("service_tier = %q, want %q", tier, "default")
		}
	})

	t.Run("does not affect non-claude sources", func(t *testing.T) {
		payload := []byte(`{"model":"gpt-5.4","messages":[]}`)
		got := applyClaudeOpenAIPriorityConfig(cfg, sdktranslator.FromString("codex"), payload)
		if gjson.GetBytes(got, "service_tier").Exists() {
			t.Fatalf("service_tier should be absent for non-claude source")
		}
	})
}
