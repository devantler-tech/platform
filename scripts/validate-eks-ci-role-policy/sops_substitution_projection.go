package main

import "regexp"

// sopsEncryptedScalar matches one whole SOPS-encrypted scalar and captures its
// declared plaintext type. Only a complete match is projected: a string that
// merely contains ciphertext stays exact, so an unexpected shape still moves
// the fingerprint instead of being silently normalized.
var sopsEncryptedScalar = regexp.MustCompile(
	`^ENC\[AES256_GCM,data:[A-Za-z0-9+/=]*,iv:[A-Za-z0-9+/=]+,tag:[A-Za-z0-9+/=]+,type:([a-z]+)\]$`,
)

// sopsSubstitutionSourceSurfaceDocument projects a SOPS-encrypted Secret that
// supplies Flux post-build substitutions down to what the gate can actually
// review: its identity, its key set, which keys are encrypted and with what
// declared type, and every unencrypted value.
//
// The ciphertext and the root `sops` metadata are dropped because they move on
// every re-encryption — a routine rotation or a SOPS version bump — while the
// validator can neither decrypt nor interpret them (#2803). Fingerprinting
// them turned every rotation into a red gate whose only documented remedy was
// re-approving the hash, which trains the re-approval into a rubber stamp.
//
// The deliberate trade: a changed VALUE of an existing encrypted key no longer
// moves this gate. It never could be reviewed here — the render substitutes
// nothing, so authorization documents are fingerprinted with their `${var}`
// literals, and the ciphertext hash only ever said "something changed" without
// saying what. Adding, removing, or renaming a substitution variable, or moving
// a key between encrypted and plaintext, still moves the surface.
func sopsSubstitutionSourceSurfaceDocument(identity resourceIdentity, document map[string]any) map[string]any {
	if identity.apiVersion != "v1" || identity.kind != "Secret" {
		return document
	}
	if _, encrypted := document["sops"].(map[string]any); !encrypted {
		return document
	}
	projected := make(map[string]any, len(document))
	for key, value := range document {
		if key == "sops" {
			continue
		}
		projected[key] = projectSOPSCiphertext(value)
	}
	return projected
}

// projectSOPSCiphertext replaces each whole encrypted scalar with a type-only
// placeholder, copying containers so the decoded document stays untouched.
func projectSOPSCiphertext(value any) any {
	switch typedValue := value.(type) {
	case string:
		if match := sopsEncryptedScalar.FindStringSubmatch(typedValue); match != nil {
			return "<sops-encrypted:" + match[1] + ">"
		}
		return typedValue
	case []any:
		projected := make([]any, len(typedValue))
		for index, item := range typedValue {
			projected[index] = projectSOPSCiphertext(item)
		}
		return projected
	case map[string]any:
		projected := make(map[string]any, len(typedValue))
		for key, item := range typedValue {
			projected[key] = projectSOPSCiphertext(item)
		}
		return projected
	}
	return value
}
