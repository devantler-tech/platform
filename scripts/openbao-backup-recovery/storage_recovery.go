package main

import (
	"encoding/json"
	"regexp"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const storageCredentialPath = "infrastructure/email/storage"

func recordValueAtPath(path string, raw json.RawMessage) (map[string]any, bool) {
	if path == storageCredentialPath {
		var value map[string]string
		if strictjson.Validate(raw, map[string][]string{"": {"passphrase"}}) != nil || json.Unmarshal(raw, &value) != nil || len(value) != 1 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(value["passphrase"]) {
			return nil, false
		}
		return map[string]any{"passphrase": value["passphrase"]}, true
	}
	switch path {
	case "apps/private-mail/admin", "apps/private-mail/maintainer", "apps/private-mail/agent", "infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent":
		return recordValue(raw)
	default:
		return nil, false
	}
}
