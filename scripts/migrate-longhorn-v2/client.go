package main

import (
	"encoding/json"
	"errors"
	"strings"
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

func verifyResetLayout(raw, ephemeral object, originalUUID string) error {
	if originalUUID == "" || text(raw, "metadata", "id") != "r-longhorn" || text(ephemeral, "metadata", "id") != "EPHEMERAL" ||
		text(ephemeral, "spec", "uuid") == "" || text(ephemeral, "spec", "uuid") == originalUUID {
		return errors.New("a new ephemeral partition is not proven")
	}
	if err := verifyEncryptedLocalLayout(raw, ephemeral); err != nil {
		return err
	}
	if value(ephemeral, "spec", "size").(float64) > 60*1024*1024*1024 {
		return errors.New("permanent worker root exceeds its reviewed reset geometry")
	}
	return nil
}

func verifyEncryptedLocalLayout(raw, ephemeral object) error {
	if text(raw, "metadata", "id") != "r-longhorn" || text(ephemeral, "metadata", "id") != "EPHEMERAL" || text(ephemeral, "spec", "uuid") == "" {
		return errors.New("encrypted local partition identities are missing")
	}
	for _, volume := range []object{raw, ephemeral} {
		if text(volume, "spec", "phase") != "ready" || text(volume, "spec", "type") != "partition" ||
			text(volume, "spec", "encryptionProvider") != "luks2" || value(volume, "spec", "encryptionLockedToState") != true ||
			text(volume, "spec", "location") == "" || !strings.HasPrefix(text(volume, "spec", "mountLocation"), "/dev/dm-") {
			return errors.New("encrypted and unlocked partition state is not proven")
		}
		keys, ok := value(volume, "spec", "configuredEncryptionKeys").([]any)
		if !ok || len(keys) != 1 || keys[0] != "nodeID" {
			return errors.New("partition key does not match the reviewed encryption contract")
		}
	}
	if text(raw, "spec", "parentLocation") == "" || text(raw, "spec", "parentLocation") != text(ephemeral, "spec", "parentLocation") ||
		text(raw, "spec", "location") == text(ephemeral, "spec", "location") || text(raw, "spec", "mountLocation") == text(ephemeral, "spec", "mountLocation") {
		return errors.New("separate partitions on the same local disk are not proven")
	}
	rawSize, rawOK := value(raw, "spec", "size").(float64)
	ephemeralSize, ephemeralOK := value(ephemeral, "spec", "size").(float64)
	if !rawOK || !ephemeralOK || rawSize != 96*1024*1024*1024 || ephemeralSize < 40*1024*1024*1024 {
		return errors.New("partition capacities do not match the reviewed storage layout")
	}
	return nil
}
