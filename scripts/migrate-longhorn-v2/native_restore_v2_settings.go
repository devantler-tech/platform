package main

import (
	"encoding/json"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Decode actual engine strings. A typed map[string]string decode alone turns
// JSON null into an empty string, which is a meaningful log-flags setting.
func nativeRestoreV2SettingValue(raw string) (string, error) {
	data := []byte(raw)
	var engines object
	if strictjson.Validate(data, map[string][]string{"": {"v1", "v2"}}) != nil || json.Unmarshal(data, &engines) != nil {
		return "", errNativeRestoreEvidence
	}
	for engine, raw := range engines {
		if _, ok := raw.(string); !ok || engine != "v1" && engine != "v2" {
			return "", errNativeRestoreEvidence
		}
	}
	value, present := engines["v2"].(string)
	if !present {
		return "", errNativeRestoreEvidence
	}
	return value, nil
}
