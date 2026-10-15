package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Inspect the served JSON, so the test catches both schema drift and loss in
// YAML-to-JSON conversion. Null is a documented accepted input, not a string.
func TestDispatchOpenAPIHintNullability(t *testing.T) {
	var spec map[string]any
	require.NoError(t, json.Unmarshal(openapiJSON, &spec))
	properties := spec["paths"].(map[string]any)["/agents/{id}/inbox"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"model", "provider"} {
		hint := properties[key].(map[string]any)
		require.Equal(t, "string", hint["type"])
		require.Equal(t, true, hint["nullable"], "%s accepts JSON null at the delivery boundary", key)
	}
}
