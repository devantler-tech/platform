package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The executor journals the
// original payload digest and a fresh random token before requesting any write.
// A read-only result alone does not qualify the restore or its reattachment.
func nativeRestoreVerifyScript(original, token string) (string, error) {
	if !seedSHA256.MatchString(original) || !seedSHA256.MatchString(token) {
		return "", errSeedEvidence
	}
	return nativeRestoreOriginalCheck(original) + nativeRestoreTokenCheck(token), nil
}

func nativeRestoreWriteScript(original, token, podUID string) (string, error) {
	if !seedSHA256.MatchString(original) || !seedSHA256.MatchString(token) || !nativeRestorePodUIDPattern.MatchString(podUID) {
		return "", errSeedEvidence
	}
	// Exec selects a Pod by name. A normal replacement receives a different
	// Downward API UID and must stop before touching the restored bytes.
	return `test "${LONGHORN_NATIVE_RESTORE_POD_UID:-}" = '` + podUID + `'` + "\n" + nativeRestoreOriginalCheck(original) + `test ! -e reattach-token
test ! -L reattach-token
(set -C; printf '%s\n' '` + token + `' > reattach-token)
sync
` + nativeRestoreTokenCheck(token), nil
}

// Check the external journal digest as well as the checksum stored in the
// original backup: replacing both a payload and its adjacent checksum fails.
func nativeRestoreOriginalCheck(original string) string {
	return nativeSeedVerify + `test ! -L payload.bin
test ! -L payload.sha256
test ! -L sentinel
printf '%s  payload.bin\n' '` + original + `' | sha256sum --check - >/dev/null
`
}

func nativeRestoreTokenCheck(token string) string {
	return `test -f reattach-token
test ! -L reattach-token
test "$(wc -c < reattach-token)" -eq 65
test "$(cat reattach-token)" = '` + token + `'
sha256sum reattach-token
`
}

func verifyNativeRestorePayload(output, original, token string) error {
	if !seedSHA256.MatchString(original) || !seedSHA256.MatchString(token) {
		return errSeedEvidence
	}
	fields := strings.Fields(output)
	digest := sha256.Sum256([]byte(token + "\n"))
	if len(fields) != 4 || fields[0] != original || fields[1] != "payload.bin" || fields[2] != hex.EncodeToString(digest[:]) || fields[3] != "reattach-token" {
		return errSeedEvidence
	}
	return nil
}
