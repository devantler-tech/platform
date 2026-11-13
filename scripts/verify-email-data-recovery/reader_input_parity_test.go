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

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
)

// TestReaderInputParityRefusesChangedOrMissingCustody catches create-on-read and stale native/source acceptance.
func TestReaderInputParityRefusesChangedOrMissingCustody(t *testing.T) {
	for _, scenario := range []string{"stable", "missing", "version-two", "recreated-record", "changed-before-read", "changed-after-read", "foreign-dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			a, reader, claim := readerCustodyNativeFixture(t)
			before, err := json.Marshal(a.objects)
			if err != nil {
				t.Fatal(err)
			}
			d := &recoveryDriver{i: retainedRecovery, k: readerInputKubernetes{inner: a}}
			d.source, err = d.readSource(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := d.captureCustodyInputs(context.Background(), reader, claim)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := readerCustodyDocument(bundle)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			expectedRecord := object{"data": stored, "metadata": object{"version": 1, "created_time": "2026-01-01T00:00:00Z", "deletion_time": "", "destroyed": false}}
			expectedRaw, err := json.Marshal(object{"infrastructure/email/recovery/retained-reader-inputs": expectedRecord})
			if err != nil || os.WriteFile(filepath.Join(directory, "expected.json"), expectedRaw, 0600) != nil {
				t.Fatal("private original record witness")
			}
			reads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != retainedReaderDataPath || r.URL.RawQuery != "" {
					writes++
					t.Error("read-only verification attempted a write or foreign request")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				reads++
				if scenario == "recreated-record" {
					_ = json.NewEncoder(w).Encode(object{"data": object{"data": stored, "metadata": object{"version": 1, "created_time": "2026-01-02T00:00:00Z", "deletion_time": "", "destroyed": false}}})
					return
				}
				if scenario == "missing" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				version := 1
				if scenario == "version-two" {
					version = 2
				}
				custodyResponse(t, w, stored, version)
			}))
			defer server.Close()
			env := readerCustodyDispatch()
			env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/verify-retained-email-reader-inputs.yaml@refs/heads/main"
			env["EMAIL_READER_INPUTS_CONFIRM"] = "verify-retained-email-reader-inputs"
			env["EMAIL_RETAINED_READER_UID"], env["EMAIL_RETAINED_CLAIM_UID"] = reader, claim
			env["EMAIL_READER_INPUTS_WORK_DIR"] = directory
			if scenario == "foreign-dispatch" {
				env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/prepare-retained-email-reader-inputs.yaml@refs/heads/main"
			}
			checks := 0
			var out bytes.Buffer
			code := runReaderInputParity(context.Background(), func(k string) string { return env[k] }, a,
				func(context.Context, string) error {
					checks++
					if (scenario == "changed-before-read" && checks == 2) || (scenario == "changed-after-read" && checks == 3) {
						child(a.objects[cleanupKey("ConfigMap", retainedRecovery.namespace, "datastore")], "data")["config.json"] = `{}`
					}
					return nil
				}, func(func(string) string) (*readerCustodyStore, error) {
					store, err := newReaderCustodyStore(server.URL, "synthetic-parity-custody-token")
					if err != nil {
						return nil, err
					}
					store.originalRecord, err = emailreaderinputs.Expectation(expectedRaw)
					return store, err
				}, &out)
			if writes != 0 || (code == 0) != (scenario == "stable") || (strings.Contains(out.String(), "PARITY=VERIFIED")) != (scenario == "stable") {
				t.Fatal("read-only custody parity accepted invalid state or used write authority")
			}
			if scenario == "stable" {
				after, err := json.Marshal(a.objects)
				if err != nil || !bytes.Equal(before, after) || checks != 3 || reads != 1 {
					t.Fatal("parity failed to rebind source/native state around a single original-version read")
				}
			}
			if scenario == "foreign-dispatch" && (checks != 0 || reads != 0) {
				t.Fatal("foreign dispatch reached native inputs or custody")
			}
		})
	}
}
