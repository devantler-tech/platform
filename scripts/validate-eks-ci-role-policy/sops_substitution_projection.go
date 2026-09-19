package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

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
// The ciphertext and the volatile `sops` metadata (mac, timestamps, version and
// each key group's wrapped data key) are dropped because they move on every
// re-encryption — a routine rotation or a SOPS version bump — while the
// validator can neither decrypt nor interpret them (#2803). Fingerprinting
// them turned every rotation into a red gate whose only documented remedy was
// re-approving the hash, which trains the re-approval into a rubber stamp.
//
// Encrypted values are nulled in the document and recorded out of band as a
// sorted list of paths with their declared types. An in-band placeholder
// string could be reproduced by a plaintext value, which would hide an
// encrypted-to-plaintext transition; a separate list cannot be.
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
	sopsMetadata, encrypted := document["sops"].(map[string]any)
	if !encrypted {
		return document
	}
	encryptedScalars := make([]string, 0)
	projected := make(map[string]any, len(document))
	for key, value := range document {
		if key == "sops" {
			projected[key] = stableSOPSMetadata(sopsMetadata)

			continue
		}
		projected[key] = projectSOPSCiphertext(value, "/"+escapeJSONPointerToken(key), &encryptedScalars)
	}
	sort.Strings(encryptedScalars)
	return map[string]any{
		"sopsProjectedDocument": projected,
		"sopsEncryptedScalars":  encryptedScalars,
	}
}

// volatileSOPSFields move on every re-encryption without any change in who can
// decrypt the Secret or which of its keys are encrypted.
var volatileSOPSFields = map[string]bool{"mac": true, "lastmodified": true, "version": true}

// volatileSOPSKeyGroupFields are the per-recipient fields that change when the
// data key is re-wrapped for the same recipient.
var volatileSOPSKeyGroupFields = map[string]bool{"enc": true, "created_at": true}

// stableSOPSMetadata keeps every `sops` field that says who can decrypt the
// Secret and how it is encrypted — each key group's recipient, encrypted_regex
// and the like — and drops only the fields listed as volatile. Losing or
// replacing a recipient leaves Flux unable to decrypt the substitution source,
// so it must still move the surface. An unrecognised field is kept, so it moves
// the surface rather than being silently ignored.
func stableSOPSMetadata(metadata map[string]any) map[string]any {
	stable := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if volatileSOPSFields[key] {
			continue
		}
		groups, isList := value.([]any)
		if !isList {
			stable[key] = value

			continue
		}
		stableGroups := make([]any, len(groups))
		for index, group := range groups {
			fields, isMap := group.(map[string]any)
			if !isMap {
				stableGroups[index] = group

				continue
			}
			stableFields := make(map[string]any, len(fields))
			for field, fieldValue := range fields {
				if !volatileSOPSKeyGroupFields[field] {
					stableFields[field] = fieldValue
				}
			}
			stableGroups[index] = stableFields
		}
		stable[key] = stableGroups
	}

	return stable
}

// projectSOPSCiphertext nulls each whole encrypted scalar and records its JSON
// pointer and declared type, copying containers so the decoded document stays
// untouched.
func projectSOPSCiphertext(value any, pointer string, encryptedScalars *[]string) any {
	switch typedValue := value.(type) {
	case string:
		if match := sopsEncryptedScalar.FindStringSubmatch(typedValue); match != nil {
			*encryptedScalars = append(*encryptedScalars, pointer+" "+match[1])
			return nil
		}
		return typedValue
	case []any:
		projected := make([]any, len(typedValue))
		for index, item := range typedValue {
			projected[index] = projectSOPSCiphertext(item, pointer+"/"+strconv.Itoa(index), encryptedScalars)
		}
		return projected
	case map[string]any:
		projected := make(map[string]any, len(typedValue))
		for key, item := range typedValue {
			projected[key] = projectSOPSCiphertext(item, pointer+"/"+escapeJSONPointerToken(key), encryptedScalars)
		}
		return projected
	}
	return value
}

// escapeJSONPointerToken applies RFC 6901 escaping so distinct keys can never
// share a recorded path.
func escapeJSONPointerToken(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}
