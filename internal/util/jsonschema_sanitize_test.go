package util

import (
	"strings"
	"testing"
)

func TestSanitizeGeminiSchemaJSON_RemovesPropertyNames(t *testing.T) {
	in := []byte(`{
		"type": "object",
		"additionalProperties": {"type": "string"},
		"propertyNames": {"pattern": "^[a-z]+$"}
	}`)

	out := SanitizeGeminiSchemaJSON(in)

	s := string(out)
	if s == string(in) {
		t.Fatalf("expected sanitizer to modify schema when propertyNames is present")
	}
	if strings.Contains(s, "\"propertyNames\"") {
		t.Fatalf("expected propertyNames to be removed, got: %s", s)
	}
	if !strings.Contains(s, "\"type\"") {
		t.Fatalf("expected type to be preserved, got: %s", s)
	}
}