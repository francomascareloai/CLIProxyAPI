// Package claude provides request translation functionality for Claude Code API compatibility.
// This package handles the conversion of Claude Code API requests into Gemini CLI-compatible
// JSON format, transforming message contents, system instructions, and tool declarations
// into the format expected by Gemini CLI API clients. It performs JSON data transformation
// to ensure compatibility between Claude Code API format and Gemini CLI API's expected format.
package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"

	client "github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiClaudeThoughtSignature = "skip_thought_signature_validator"

// ConvertClaudeRequestToAntigravity parses and transforms a Claude Code API request into Gemini CLI API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini CLI API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini CLI API format
// 3. Converts system instructions to the expected format
// 4. Maps message contents with proper role transformations
// 5. Handles tool declarations and tool choices
// 6. Maps generation configuration parameters
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func ConvertClaudeRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := bytes.Clone(inputRawJSON)
	rawJSON = bytes.Replace(rawJSON, []byte(`"url":{"type":"string","format":"uri",`), []byte(`"url":{"type":"string",`), -1)

	allowThinkingWithToolUse := isTruthyEnv("CLIPROXY_ANTIGRAVITY_ALLOW_THINKING_WITH_TOOL_USE")

	// Determine whether the caller requested Anthropic-style thinking.
	// We intentionally do not gate on registry metadata here:
	// - Some upstreams enforce strict "thinking + tool_use" ordering when thinking is enabled.
	// - Capability stripping is handled later by executor normalization (StripThinkingConfigIfUnsupported).
	thinkingRequested := false
	thinkingBudget := 0
	if t := gjson.GetBytes(rawJSON, "thinking"); t.Exists() && t.IsObject() {
		if t.Get("type").String() == "enabled" {
			if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
				thinkingRequested = true
				thinkingBudget = int(b.Int())
			}
		}
	}

	// Some upstream Claude implementations require that any assistant tool_use message in the
	// request history begins with a thinking block containing a valid signature when thinking
	// is enabled. If the client session history is missing those signatures (common after
	// prior proxy/client bugs), enabling thinking will hard-fail with 400.
	//
	// We therefore only forward thinkingConfig when the request history appears compatible.
	thinkingHistoryCompatible := true
	if thinkingRequested {
		hasAnyToolUse := false
		messagesResult := gjson.GetBytes(rawJSON, "messages")
		if messagesResult.IsArray() {
			for _, msg := range messagesResult.Array() {
				if msg.Get("role").String() != "assistant" {
					continue
				}
				content := msg.Get("content")
				if !content.IsArray() {
					continue
				}
				contentArr := content.Array()
				hasToolUse := false
				for _, c := range contentArr {
					if c.Get("type").String() == "tool_use" {
						hasToolUse = true
						hasAnyToolUse = true
						break
					}
				}
				if !hasToolUse {
					continue
				}
				if len(contentArr) == 0 {
					thinkingHistoryCompatible = false
					break
				}
				firstType := contentArr[0].Get("type").String()
				switch firstType {
				case "thinking":
					if !allowThinkingWithToolUse {
						if sig := contentArr[0].Get("signature").String(); strings.TrimSpace(sig) == "" {
							thinkingHistoryCompatible = false
						}
					}
				case "redacted_thinking":
					// OK: redacted thinking does not require a signature.
				default:
					// Missing thinking/redacted_thinking prefix on a tool_use message.
					if !allowThinkingWithToolUse {
						thinkingHistoryCompatible = false
					}
				}
				if !thinkingHistoryCompatible {
					break
				}
			}
		}
		// Antigravity's Claude-on-Gemini backend returns a thoughtSignature that is NOT a valid
		// Anthropic signed-thinking signature. When tool_use is involved, forwarding thinkingConfig
		// can cause upstream strict signature validation to fail.
		if hasAnyToolUse && !allowThinkingWithToolUse {
			thinkingHistoryCompatible = false
		}
		if allowThinkingWithToolUse {
			// Experimental mode: force-enable thinkingConfig even when tool_use exists in history.
			// This may reintroduce upstream 400s depending on strict signed-thinking validation.
			thinkingHistoryCompatible = true
		}
	}

	// system instruction
	var systemInstruction *client.Content
	systemResult := gjson.GetBytes(rawJSON, "system")
	if systemResult.IsArray() {
		systemResults := systemResult.Array()
		systemInstruction = &client.Content{Role: "user", Parts: []client.Part{}}
		for i := 0; i < len(systemResults); i++ {
			systemPromptResult := systemResults[i]
			systemTypePromptResult := systemPromptResult.Get("type")
			if systemTypePromptResult.Type == gjson.String && systemTypePromptResult.String() == "text" {
				systemPrompt := systemPromptResult.Get("text").String()
				systemPart := client.Part{Text: systemPrompt}
				systemInstruction.Parts = append(systemInstruction.Parts, systemPart)
			}
		}
		if len(systemInstruction.Parts) == 0 {
			systemInstruction = nil
		}
	}

	// contents
	contents := make([]client.Content, 0)
	messagesResult := gjson.GetBytes(rawJSON, "messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()
		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			roleResult := messageResult.Get("role")
			if roleResult.Type != gjson.String {
				continue
			}
			role := roleResult.String()
			if role == "assistant" {
				role = "model"
			}
			clientContent := client.Content{Role: role, Parts: []client.Part{}}
			contentsResult := messageResult.Get("content")
			if contentsResult.IsArray() {
				contentResults := contentsResult.Array()
				for j := 0; j < len(contentResults); j++ {
					contentResult := contentResults[j]
					contentTypeResult := contentResult.Get("type")
					if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "thinking" {
						// Default: do NOT forward Anthropic signed-thinking blocks to Antigravity.
						// Experimental override: if enabled, forward thinking text as Gemini thought parts
						// WITHOUT a thoughtSignature.
						if allowThinkingWithToolUse && thinkingRequested {
							prompt := contentResult.Get("thinking").String()
							if strings.TrimSpace(prompt) != "" {
								clientContent.Parts = append(clientContent.Parts, client.Part{Thought: true, Text: prompt})
							}
						}
						continue
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "text" {
						prompt := contentResult.Get("text").String()
						clientContent.Parts = append(clientContent.Parts, client.Part{Text: prompt})
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "tool_use" {
						functionName := contentResult.Get("name").String()
						functionArgs := contentResult.Get("input")
						functionID := contentResult.Get("id").String()
						var args map[string]any
						if functionArgs.Exists() && functionArgs.IsObject() && json.Unmarshal([]byte(functionArgs.Raw), &args) == nil {
							clientContent.Parts = append(clientContent.Parts, client.Part{
								FunctionCall:     &client.FunctionCall{ID: functionID, Name: functionName, Args: args},
								ThoughtSignature: geminiClaudeThoughtSignature,
							})
						}
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "tool_result" {
						toolCallID := contentResult.Get("tool_use_id").String()
						if toolCallID != "" {
							funcName := toolCallID
							toolCallIDs := strings.Split(toolCallID, "-")
							if len(toolCallIDs) > 1 {
								funcName = strings.Join(toolCallIDs[0:len(toolCallIDs)-1], "-")
							}
							responseData := contentResult.Get("content").Raw
							functionResponse := client.FunctionResponse{ID: toolCallID, Name: funcName, Response: map[string]interface{}{"result": responseData}}
							clientContent.Parts = append(clientContent.Parts, client.Part{FunctionResponse: &functionResponse})
						}
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "image" {
						sourceResult := contentResult.Get("source")
						if sourceResult.Get("type").String() == "base64" {
							inlineData := &client.InlineData{
								MimeType: sourceResult.Get("media_type").String(),
								Data:     sourceResult.Get("data").String(),
							}
							clientContent.Parts = append(clientContent.Parts, client.Part{InlineData: inlineData})
						}
					}
				}

				contents = append(contents, clientContent)
			} else if contentsResult.Type == gjson.String {
				prompt := contentsResult.String()
				contents = append(contents, client.Content{Role: role, Parts: []client.Part{{Text: prompt}}})
			}
		}
	}

	// tools
	var tools []client.ToolDeclaration
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if toolsResult.IsArray() {
		tools = make([]client.ToolDeclaration, 1)
		tools[0].FunctionDeclarations = make([]any, 0)
		toolsResults := toolsResult.Array()
		for i := 0; i < len(toolsResults); i++ {
			toolResult := toolsResults[i]
			inputSchemaResult := toolResult.Get("input_schema")
			if inputSchemaResult.Exists() && inputSchemaResult.IsObject() {
				inputSchema := inputSchemaResult.Raw
				tool, _ := sjson.Delete(toolResult.Raw, "input_schema")
				tool, _ = sjson.SetRaw(tool, "parametersJsonSchema", inputSchema)
				tool, _ = sjson.Delete(tool, "strict")
				tool, _ = sjson.Delete(tool, "input_examples")
				if schema := gjson.Get(tool, "parametersJsonSchema"); schema.Exists() && (schema.IsObject() || schema.IsArray()) {
					sanitized := util.SanitizeGeminiSchemaJSON([]byte(schema.Raw))
					tool, _ = sjson.SetRaw(tool, "parametersJsonSchema", string(sanitized))
				}
				var toolDeclaration any
				if err := json.Unmarshal([]byte(tool), &toolDeclaration); err == nil {
					tools[0].FunctionDeclarations = append(tools[0].FunctionDeclarations, toolDeclaration)
				}
			}
		}
	} else {
		tools = make([]client.ToolDeclaration, 0)
	}

	// Build output Gemini CLI request JSON
	out := `{"model":"","request":{"contents":[]}}`
	out, _ = sjson.Set(out, "model", modelName)
	if systemInstruction != nil {
		b, _ := json.Marshal(systemInstruction)
		out, _ = sjson.SetRaw(out, "request.systemInstruction", string(b))
	}
	if len(contents) > 0 {
		b, _ := json.Marshal(contents)
		out, _ = sjson.SetRaw(out, "request.contents", string(b))
	}
	if len(tools) > 0 && len(tools[0].FunctionDeclarations) > 0 {
		b, _ := json.Marshal(tools)
		out, _ = sjson.SetRaw(out, "request.tools", string(b))
	}

	// Map Anthropic thinking -> Gemini thinkingBudget/include_thoughts when type==enabled.
	// Only enable it when the request history contains valid tool-use thinking signatures.
	if thinkingRequested && thinkingHistoryCompatible {
		out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.thinkingBudget", thinkingBudget)
		out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.include_thoughts", true)
	}
	if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.temperature", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.topP", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.topK", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "max_tokens"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.maxOutputTokens", v.Num)
	}

	outBytes := []byte(out)
	outBytes = common.AttachDefaultSafetySettings(outBytes, "request.safetySettings")

	return outBytes
}

func isTruthyEnv(key string) bool {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return false
	}
	val = strings.ToLower(val)
	return val == "1" || val == "true" || val == "yes" || val == "y" || val == "on"
}
