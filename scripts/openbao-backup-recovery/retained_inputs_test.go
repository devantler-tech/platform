package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const retainedInputTestPath = "infrastructure/email/recovery/retained-reader-inputs"

// retainedInputDocumentFixture builds independent synthetic copy inputs, never production credentials.
func retainedInputDocumentFixture(t *testing.T) map[string]any {
	t.Helper()
	namespace := "email-data-recovery-41561215415"
	refs := [][3]string{{"Pod", "reader", namespace}, {"PersistentVolumeClaim", "data", namespace}, {"Namespace", namespace, ""}, {"StorageClass", namespace, ""}, {"CiliumNetworkPolicy", "isolation", namespace}, {"ConfigMap", "datastore", namespace}, {"Secret", "storage-key", namespace}, {"Secret", "test-authentication", namespace}}
	var inputs []any
	for i, ref := range refs {
		values := map[string]any{"data": map[string]any{"synthetic-input": fmt.Sprintf("independent-copy-value-%d", i)}}
		raw, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		inputs = append(inputs, map[string]any{"Kind": ref[0], "Name": ref[1], "Namespace": ref[2], "UID": fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), "Digest": hex.EncodeToString(digest[:]), "Values": values})
	}
	return map[string]any{"schema": "email-reader-inputs/v1", "trial_run": "41561215415", "trial_revision": "7e4486cad02c326004ac9a1a637bb5205dbfe76a", "bundle": map[string]any{"ReaderUID": "00000000-0000-4000-8000-000000000001", "ClaimUID": "00000000-0000-4000-8000-000000000002", "Inputs": inputs}}
}

// TestRetainedInputEqualityIncludesNestedValuesAndOriginalMetadata catches passphrase-only or field-count proofs.
func TestRetainedInputEqualityIncludesNestedValuesAndOriginalMetadata(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{retainedInputTestPath}
	encode := func(response map[string]any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(response["data"])
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	expected := map[string]json.RawMessage{retainedInputTestPath: encode(retainedInputResponseFixture(t))}
	if !recordsEqual(expected, expected) {
		t.Fatal("valid complete record was refused")
	}
	for _, field := range []string{"created_time", "nested-key", "nested-config"} {
		response := retainedInputResponseFixture(t)
		data := response["data"].(map[string]any)
		if field == "created_time" {
			data["metadata"].(map[string]any)[field] = "2026-11-12T02:28:48Z"
		} else {
			index := 6
			if field == "nested-config" {
				index = 5
			}
			entry := data["data"].(map[string]any)["bundle"].(map[string]any)["Inputs"].([]any)[index].(map[string]any)
			entry["Values"].(map[string]any)["data"].(map[string]any)["synthetic-input"] = "different-but-valid-copy-input"
			raw, err := json.Marshal(entry["Values"])
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			entry["Digest"] = hex.EncodeToString(digest[:])
		}
		actual := map[string]json.RawMessage{retainedInputTestPath: encode(response)}
		if !recordsEqual(actual, actual) {
			t.Fatal("altered valid fixture did not reach full equality comparison")
		}
		if recordsEqual(expected, actual) {
			t.Fatal("changed nested input or original creation identity was ignored")
		}
	}
}

// TestRetainedInputFixtureEmitsARecoverableOriginalRecord exercises the actual sandbox fixture producer.
func TestRetainedInputFixtureEmitsARecoverableOriginalRecord(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{retainedInputTestPath}
	var saved json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sys/mounts/secret":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/secret/data/"+retainedInputTestPath:
			var payload struct {
				Data json.RawMessage `json:"data"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("invalid fixture write")
			}
			saved = payload.Data
			_, _ = w.Write([]byte(`{"data":{"version":1}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/secret/data/"+retainedInputTestPath:
			_, _ = fmt.Fprintf(w, `{"data":{"data":%s,"metadata":{"version":1,"created_time":"2026-11-12T02:28:47Z","deletion_time":"","destroyed":false}}}`, saved)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/storage/raft/snapshot":
			_, _ = w.Write([]byte("synthetic-snapshot-only"))
		default:
			t.Error("fixture escaped its fixed synthetic API boundary")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	api, err := newAPI(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := api.createFixture(context.Background(), "synthetic-root", "synthetic-unseal", dir); err != nil {
		t.Fatal(err)
	}
	raw, err := privateInput(filepath.Join(dir, "expected.json"), 1<<20)
	var expected map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &expected) != nil || !recordsEqual(expected, expected) {
		t.Fatal("fixture failed to preserve the full original-version retained input record")
	}
}

// retainedInputResponseFixture includes every security-relevant original-version metadata field.
func retainedInputResponseFixture(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"data": map[string]any{"data": retainedInputDocumentFixture(t), "metadata": map[string]any{"created_time": "2026-11-12T02:28:47Z", "deletion_time": "", "destroyed": false, "version": 1, "custom_metadata": nil}}}
}

// TestRetainedInputModeSelectsOnlyTheFixedRecord catches accidental account/storage aliases or arbitrary paths.
func TestRetainedInputModeSelectsOnlyTheFixedRecord(t *testing.T) {
	for _, mode := range []string{"collect", "verify", "fixture"} {
		base, paths, ok := credentialMode(mode + "-email-retained-inputs")
		if !ok || base != mode || !slices.Equal(paths, []string{retainedInputTestPath}) {
			t.Fatal("retained input recovery did not select exactly its fixed original record")
		}
	}
	for _, mode := range []string{"collect-email-retained-inputs/elsewhere", "verify-email-retained-inputs\n", "image-email-retained-inputs", "seed-email-retained-inputs"} {
		if _, _, ok := credentialMode(mode); ok {
			t.Fatal("undeclared retained input mode admitted")
		}
	}
}

// TestRetainedInputReadRequiresCompleteOriginalVersion catches generic version>=1 or defaulted metadata acceptance.
func TestRetainedInputReadRequiresCompleteOriginalVersion(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{retainedInputTestPath}
	for _, name := range []string{"valid", "version-two", "string-version", "missing-version", "deleted", "missing-deleted", "destroyed", "missing-destroyed", "missing-created", "invalid-created", "future-created", "wrong-trial", "changed-digest", "foreign-input", "duplicate-uid", "incomplete-inputs", "duplicate-json", "oversized"} {
		t.Run(name, func(t *testing.T) {
			response := retainedInputResponseFixture(t)
			data := response["data"].(map[string]any)
			metadata := data["metadata"].(map[string]any)
			document := data["data"].(map[string]any)
			inputs := document["bundle"].(map[string]any)["Inputs"].([]any)
			switch name {
			case "version-two":
				metadata["version"] = 2
			case "string-version":
				metadata["version"] = "1"
			case "missing-version":
				delete(metadata, "version")
			case "deleted":
				metadata["deletion_time"] = "2026-11-12T02:28:48Z"
			case "missing-deleted":
				delete(metadata, "deletion_time")
			case "destroyed":
				metadata["destroyed"] = true
			case "missing-destroyed":
				delete(metadata, "destroyed")
			case "missing-created":
				delete(metadata, "created_time")
			case "invalid-created":
				metadata["created_time"] = "not-a-time"
			case "future-created":
				metadata["created_time"] = "2099-01-01T00:00:00Z"
			case "wrong-trial":
				document["trial_run"] = "42818236850"
			case "changed-digest":
				inputs[6].(map[string]any)["Digest"] = strings.Repeat("0", 64)
			case "foreign-input":
				inputs[6].(map[string]any)["Namespace"] = "private-mail"
			case "duplicate-uid":
				inputs[6].(map[string]any)["UID"] = inputs[5].(map[string]any)["UID"]
			case "incomplete-inputs":
				document["bundle"].(map[string]any)["Inputs"] = inputs[:7]
			case "oversized":
				document["padding"] = strings.Repeat("x", 513<<10)
			}
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if name == "duplicate-json" {
				raw = []byte(strings.Replace(string(raw), `"version":1`, `"version":2,"version":1`, 1))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/secret/data/"+retainedInputTestPath || r.URL.RawQuery != "" {
					t.Error("recovery attempted a write or foreign credential path")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			api, err := newAPI(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			records, err := api.credentialRecords(context.Background(), "synthetic-token")
			if name == "valid" {
				if err != nil || !recordsEqual(records, records) {
					t.Fatal("complete original retained inputs were refused")
				}
				var preserved map[string]any
				if json.Unmarshal(records[retainedInputTestPath], &preserved) != nil || preserved["metadata"] == nil || preserved["data"] == nil {
					t.Fatal("original metadata or complete input record was dropped")
				}
			} else if err == nil {
				t.Fatal("invalid original input custody was accepted")
			}
		})
	}
}

// TestRetainedInputRestoreRejectsDuplicateExpectations catches map decoding that hides conflicting original records.
func TestRetainedInputRestoreRejectsDuplicateExpectations(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{retainedInputTestPath}
	raw, err := json.Marshal(retainedInputResponseFixture(t)["data"])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string][]byte{"root-token": []byte(strings.Repeat("a", 40)), "unseal-key": []byte(strings.Repeat("b", 44)), "expected.json": []byte(`{"` + retainedInputTestPath + `":` + string(raw) + `,"` + retainedInputTestPath + `":` + string(raw) + `}`)}
	for name, data := range files {
		if os.WriteFile(filepath.Join(dir, name), data, 0600) != nil {
			t.Fatal("private fixture")
		}
	}
	api := &baoAPI{}
	if err := api.restore(context.Background(), "different-temporary-root", dir); err != stageError("inputs") {
		t.Fatalf("duplicate expected records reached snapshot handling instead of refusal: %v", err)
	}
}
