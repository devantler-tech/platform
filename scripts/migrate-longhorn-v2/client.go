package main

import (
	"encoding/json"
	"errors"
)

func decodeList(data []byte) ([]object, error) {
	var list object
	if err := json.Unmarshal(data, &list); err != nil || list == nil {
		return nil, errors.New("resource inventory is not a JSON object")
	}
	if next := value(list, "metadata", "continue"); next != nil && next != "" {
		return nil, errors.New("resource inventory has an unread continuation")
	}
	items, ok := list["items"].([]any)
	if !ok {
		return nil, errors.New("resource inventory has no complete items array")
	}
	result := make([]object, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		o, ok := item.(map[string]any)
		name, uid := text(o, "metadata", "name"), text(o, "metadata", "uid")
		key := text(o, "metadata", "namespace") + "/" + name
		if !ok || name == "" || uid == "" || seen[key] {
			return nil, errors.New("resource inventory has incomplete or duplicated identities")
		}
		seen[key] = true
		result = append(result, o)
	}
	return result, nil
}
