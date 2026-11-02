package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const legacyKeyPath = "resources/secrets/namespaces/openbao/openbao-unseal.json"
const preferredKeyPath = "resources/secrets/v1-preferredversion/namespaces/openbao/openbao-unseal.json"

type credentialArchiveEntry struct {
	name string
	body []byte
	kind byte
}

// credentialArchive reproduces Velero's regular resource entries without writing private data.
func credentialArchive(t *testing.T, entries ...credentialArchiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: 0600, Size: int64(len(entry.body))}
		if kind == tar.TypeSymlink {
			header.Size, header.Linkname = 0, "unrelated"
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tw.Close() != nil || gz.Close() != nil {
		t.Fatal("cannot complete synthetic archive")
	}
	return out.Bytes()
}

// credentialSecret supplies the fixed synthetic object, not a live recovery key.
func credentialSecret(t *testing.T, root, unseal string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]string{"name": "openbao-unseal", "namespace": "openbao", "uid": "00000000-0000-0000-0000-000000000001"},
		"data":     map[string]string{"root-token": base64.StdEncoding.EncodeToString([]byte(root)), "unseal-key": base64.StdEncoding.EncodeToString([]byte(unseal))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestArchiveCredentialsAreCompleteAndUnambiguous catches incomplete or conflicting recovery evidence.
func TestArchiveCredentialsAreCompleteAndUnambiguous(t *testing.T) {
	root, unseal := strings.Repeat("r", 40), strings.Repeat("u", 44)
	secret := credentialSecret(t, root, unseal)
	for _, scenario := range []string{"both-versions", "legacy-only", "preferred-only", "missing", "conflicting-versions", "duplicate-entry", "wrong-namespace", "invalid-key", "duplicate-json-field", "symbolic-link", "truncated", "corrupt-checksum", "trailing-stream", "oversized-record"} {
		t.Run(scenario, func(t *testing.T) {
			entries := []credentialArchiveEntry{{name: legacyKeyPath, body: secret}, {name: preferredKeyPath, body: secret}}
			switch scenario {
			case "legacy-only":
				entries = entries[:1]
			case "preferred-only":
				entries = entries[1:]
			case "missing":
				entries = []credentialArchiveEntry{{name: "unrelated", body: []byte("PRIVATE_SYNTHETIC_VALUE")}}
			case "conflicting-versions":
				entries[1].body = credentialSecret(t, strings.Repeat("s", 40), unseal)
			case "duplicate-entry":
				entries = append(entries, entries[0])
			case "wrong-namespace":
				bad := bytes.ReplaceAll(secret, []byte(`"namespace":"openbao"`), []byte(`"namespace":"other"`))
				entries = []credentialArchiveEntry{{name: legacyKeyPath, body: bad}}
			case "invalid-key":
				entries = []credentialArchiveEntry{{name: legacyKeyPath, body: credentialSecret(t, "", unseal)}}
			case "duplicate-json-field":
				entries = []credentialArchiveEntry{{name: legacyKeyPath, body: []byte(strings.TrimSuffix(string(secret), "}") + `,"data":{}}`)}}
			case "symbolic-link":
				entries = []credentialArchiveEntry{{name: legacyKeyPath, kind: tar.TypeSymlink}}
			case "oversized-record":
				entries = []credentialArchiveEntry{{name: legacyKeyPath, body: bytes.Repeat([]byte("a"), 16385)}}
			}
			archive := credentialArchive(t, entries...)
			switch scenario {
			case "truncated":
				archive = archive[:len(archive)-3]
			case "corrupt-checksum":
				archive[len(archive)-8] ^= 1
			case "trailing-stream":
				archive = append(archive, credentialArchive(t, entries...)...)
			}
			got, err := readCredentialArchive(archive)
			want := scenario == "both-versions" || scenario == "legacy-only" || scenario == "preferred-only"
			if want {
				if err != nil || got.Root != root || got.Unseal != unseal {
					t.Fatal("independent recovery credentials were not recovered")
				}
			} else if err == nil || got.Root != "" || got.Unseal != "" {
				t.Fatal("invalid archive released recovery credentials")
			}
		})
	}
}
