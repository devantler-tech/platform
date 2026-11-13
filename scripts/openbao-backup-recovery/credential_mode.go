package main

import (
	"slices"

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
)

// credentialMode selects only a declared credential set. Legacy modes remain
// available to the one-time copy workflow and retained-history recovery.
func credentialMode(requested string) (string, []string, bool) {
	mode := map[string]string{
		"image": "image", "collect": "collect", "verify": "verify", "fixture": "fixture",
		"collect-email": "collect", "verify-email": "verify", "fixture-email": "fixture",
		"collect-email-storage": "collect", "verify-email-storage": "verify", "fixture-email-storage": "fixture",
		"seed-email-storage":            "seed-storage",
		"collect-email-retained-inputs": "collect", "verify-email-retained-inputs": "verify", "fixture-email-retained-inputs": "fixture",
	}[requested]
	if mode == "" {
		return "", nil, false
	}
	if requested != mode {
		if requested == "collect-email-retained-inputs" || requested == "verify-email-retained-inputs" || requested == "fixture-email-retained-inputs" {
			return mode, []string{emailreaderinputs.Path}, true
		}
		paths := []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent"}
		switch requested {
		case "collect-email-storage", "verify-email-storage", "fixture-email-storage":
			paths = append(paths, storageCredentialPath)
		}
		return mode, paths, true
	}
	return mode, []string{"apps/private-mail/admin", "apps/private-mail/maintainer", "apps/private-mail/agent"}, true
}

func canonicalRecoverySet() bool {
	_, paths, ok := credentialMode("collect-email")
	if ok && slices.Equal(credentialPaths, paths) {
		return true
	}
	_, paths, ok = credentialMode("collect-email-storage")
	if ok && slices.Equal(credentialPaths, paths) {
		return true
	}
	_, paths, ok = credentialMode("collect-email-retained-inputs")
	return ok && slices.Equal(credentialPaths, paths)
}
