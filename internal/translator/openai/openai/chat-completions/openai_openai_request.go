// Package openai provides request translation functionality for OpenAI to OpenAI API compatibility.
// It converts OpenAI Chat Completions requests into OpenAI-compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToOpenAI converts an OpenAI Chat Completions request (raw JSON)
// into a complete OpenAI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in OpenAI API format
func ConvertOpenAIRequestToOpenAI(modelName string, inputRawJSON []byte, _ bool) []byte {
	// Update the "model" field in the JSON payload with the provided modelName
	// The sjson.SetBytes function returns a new byte slice with the updated JSON.
	updatedJSON, err := sjson.SetBytes(inputRawJSON, "model", modelName)
	if err != nil {
		// If there's an error, return the original JSON or handle the error appropriately.
		// For now, we'll return the original, but in a real scenario, logging or a more robust error
		// handling mechanism would be needed.
		return inputRawJSON
	}
	// Some clients send non-standard "tools.defer_loading" hints that are not accepted
	// by OpenAI-compatible providers. Remove them before forwarding.
	updatedJSON, _ = sjson.DeleteBytes(updatedJSON, "tools.defer_loading")

	tools := gjson.GetBytes(updatedJSON, "tools")
	if tools.IsArray() {
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			if arr[i].Get("defer_loading").Exists() {
				updatedJSON, _ = sjson.DeleteBytes(updatedJSON, fmt.Sprintf("tools.%d.defer_loading", i))
			}
		}
	}

	return updatedJSON
}
