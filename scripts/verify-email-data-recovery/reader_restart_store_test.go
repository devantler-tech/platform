package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReaderRestartCustodyRequiresRestoreAndFinalNativeParity exercises the actual staged operator and create-only protocol.
func TestReaderRestartCustodyRequiresRestoreAndFinalNativeParity(t *testing.T) {
	for _, scenario := range []string{"stable", "missing-restore", "wrong-restore", "existing-record", "changed-before-write", "changed-after-write", "uncertain-response", "wrong-version", "changed-readback", "source-refusal", "string-version", "missing-created", "deleted", "destroyed", "changed-created", "duplicate-response", "oversized-response", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			a, env := readerRestartNativeFixture(t)
			store := readerRestartOriginalStore(t, a, env)
			dir := t.TempDir()
			if os.Chmod(dir, 0700) != nil {
				t.Fatal("private fixture")
			}
			env["EMAIL_READER_INPUTS_WORK_DIR"] = dir
			var inspection bytes.Buffer
			load := func(func(string) string) (*readerCustodyStore, error) { return store, nil }
			if runReaderRestartWitness(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error { return nil }, load, &inspection, false) != 0 {
				t.Fatalf("valid initial witness inspection refused: %q", inspection.String())
			}
			if scenario != "missing-restore" {
				marker := "OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3\n"
				if scenario == "wrong-restore" {
					marker = "OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3\n"
				}
				if os.WriteFile(filepath.Join(dir, "restore-result"), []byte(marker), 0600) != nil {
					t.Fatal("private fixture")
				}
			}
			posts, foreign, deletes := 0, 0, 0
			var saved object
			created := "2026-01-02T00:00:00Z"
			metadata := func(version int) object {
				value := object{"version": version, "created_time": created, "deletion_time": "", "destroyed": false}
				switch scenario {
				case "string-version":
					value["version"] = "1"
				case "missing-created":
					delete(value, "created_time")
				case "deleted":
					value["deletion_time"] = created
				case "destroyed":
					value["destroyed"] = true
				}
				return value
			}
			changed := func() { child(a.backend, "spec")["size"] = "3221225472" }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "" || r.Header.Get("X-Vault-Token") != "synthetic-private-archive-token" {
					foreign++
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == retainedReaderDataPath:
					_ = json.NewEncoder(w).Encode(object{"data": store.originalRecord})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/mounts":
					_ = json.NewEncoder(w).Encode(object{"data": object{"secret/": object{"type": "kv", "options": object{"version": "2"}}}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/secret/metadata/infrastructure/email/recovery/retained-reader-restart-witness":
					if scenario == "existing-record" {
						_, _ = w.Write([]byte(`{"data":{"current_version":1}}`))
						return
					}
					w.WriteHeader(http.StatusNotFound)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/secret/data/infrastructure/email/recovery/retained-reader-restart-witness":
					if saved == nil {
						if scenario == "changed-before-write" {
							changed()
						}
						w.WriteHeader(http.StatusNotFound)
						return
					}
					version := 1
					if scenario == "wrong-version" {
						version = 2
					}
					if scenario == "changed-after-write" {
						changed()
					}
					if scenario == "changed-readback" {
						saved["captureRevision"] = strings.Repeat("f", 40)
					}
					m := metadata(version)
					if scenario == "changed-created" {
						m["created_time"] = "2026-01-03T00:00:00Z"
					}
					_ = json.NewEncoder(w).Encode(object{"data": object{"data": saved, "metadata": m}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/secret/data/infrastructure/email/recovery/retained-reader-restart-witness":
					posts++
					var payload object
					if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 2 || child(payload, "options")["cas"] != float64(0) {
						t.Error("restart witness was not create-only")
					}
					saved = child(payload, "data")
					if saved["schema"] != "email-reader-restart-witness/v1" || len(items(saved, "objects")) != 10 {
						t.Error("restart custody omitted the complete fixed witness")
					}
					if scenario == "uncertain-response" {
						_, _ = w.Write([]byte("PRIVATE_UNCERTAIN_RESPONSE"))
						return
					}
					switch scenario {
					case "duplicate-response":
						_, _ = w.Write([]byte(`{"data":{},"data":{}}`))
						return
					case "oversized-response":
						_, _ = w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
						return
					case "redirect":
						w.Header().Set("Location", "/foreign")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					_ = json.NewEncoder(w).Encode(object{"data": metadata(1)})
				default:
					if r.Method == http.MethodDelete {
						deletes++
					} else {
						foreign++
					}
					w.WriteHeader(http.StatusForbidden)
				}
			}))
			defer server.Close()
			store.endpoint = server.URL
			checks := 0
			var out bytes.Buffer
			code := runReaderRestartWitness(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error {
				checks++
				if scenario == "source-refusal" && checks >= 2 {
					return errReaderInputRead
				}
				return nil
			}, load, &out, true)
			if (code == 0) != (scenario == "stable") || strings.Contains(out.String(), "=STORED") != (scenario == "stable") || foreign != 0 || deletes != 0 || a.mutations != 0 || posts > 1 || strings.Contains(out.String(), "PRIVATE_") || strings.Contains(out.String(), "restart=AUTHORIZED") {
				t.Fatalf("witness custody accepted incomplete proof or widened authority: case=%s code=%d posts=%d output=%q", scenario, code, posts, out.String())
			}
			if scenario == "stable" && posts != 1 {
				t.Fatal("valid witness was not durably stored")
			}
			if (scenario == "missing-restore" || scenario == "wrong-restore" || scenario == "existing-record" || scenario == "changed-before-write" || scenario == "source-refusal") && posts != 0 {
				t.Fatal("preflight refusal wrote a record")
			}
			if scenario == "uncertain-response" && (posts != 1 || saved == nil) {
				t.Fatal("uncertain create was retried or rolled back")
			}
		})
	}
}
