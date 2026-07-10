// Package minimax implements thinking configuration for MiniMax models.
//
// MiniMax web chat API uses `thinking.type` for thinking control:
//   - adaptive (default): model decides when to think
//   - disabled: no thinking
//   - reasoning_effort is not supported by the web chat API
package minimax

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Applier implements thinking.ProviderApplier for MiniMax models.
type Applier struct{}

var _ thinking.ProviderApplier = (*Applier)(nil)

// NewApplier creates a new MiniMax thinking applier.
func NewApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("minimax", NewApplier())
}

// Apply applies thinking configuration to MiniMax request body.
//
// MiniMax web chat API supports:
//   - thinking.type = "adaptive" (default, model decides)
//   - thinking.type = "disabled" (no thinking)
func (a *Applier) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	if thinking.IsUserDefinedModel(modelInfo) {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	switch config.Mode {
	case thinking.ModeNone:
		// For MiniMax, disabled thinking = thinking.type = "disabled"
		return setDisabledThinking(body)
	case thinking.ModeAuto, thinking.ModeLevel, thinking.ModeBudget:
		// MiniMax uses adaptive thinking by default when thinking.type is absent.
		// Only set if explicitly configured.
		if config.Level == thinking.LevelNone || string(config.Level) == "disabled" {
			return setDisabledThinking(body)
		}
		// For enabled thinking, MiniMax web chat uses "adaptive" (default)
		return setAdaptiveThinking(body)
	default:
		return body, nil
	}
}

func setDisabledThinking(body []byte) ([]byte, error) {
	result, err := sjson.SetBytes(body, "thinking.type", "disabled")
	if err != nil {
		return body, err
	}
	// Remove reasoning_effort if present (MiniMax web chat doesn't support it)
	result, _ = sjson.DeleteBytes(result, "reasoning_effort")
	return result, nil
}

func setAdaptiveThinking(body []byte) ([]byte, error) {
	result, err := sjson.SetBytes(body, "thinking.type", "adaptive")
	if err != nil {
		return body, err
	}
	return result, nil
}
