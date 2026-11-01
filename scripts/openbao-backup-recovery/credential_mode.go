package main

import "slices"

// credentialMode selects only a declared credential set. Legacy modes remain
// available to the one-time copy workflow and retained-history recovery.
func credentialMode(requested string) (string, []string, bool) {
	mode := map[string]string{
		"image": "image", "collect": "collect", "verify": "verify", "fixture": "fixture",
		"collect-email": "collect", "verify-email": "verify", "fixture-email": "fixture",
		"collect-email-storage": "collect", "verify-email-storage": "verify", "fixture-email-storage": "fixture",
		"seed-email-storage": "seed-storage",
	}[requested]
	if mode == "" {
		return "", nil, false
	}
	if requested != mode {
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
	return ok && slices.Equal(credentialPaths, paths)
}
