package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// custodyBundle freezes independently witnessed fixture inputs for storage protocol tests.
func custodyBundle(t *testing.T) retainedReaderInputs {
	t.Helper()
	graph, reader, claim := readerInputFixture(t)
	bundle, err := captureRetainedReaderInputs(retainedRecovery, fixtureImage, reader, claim, "ffffffff-1111-2222-3333-444444444444", graph)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// custodyResponse models KV2 data with explicit original-version and deletion metadata.
func custodyResponse(t *testing.T, w http.ResponseWriter, data any, version int) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(object{"data": object{"data": data, "metadata": object{"version": version, "destroyed": false, "deletion_time": "", "created_time": "2026-01-01T00:00:00Z"}}}); err != nil {
		t.Error(err)
	}
}

// TestReaderCustodyCreatesOnlyOnceAndRebindsImmediatelyBeforeWrite checks the final native rebind, readback and zero-overwrite repeat.
func TestReaderCustodyCreatesOnlyOnceAndRebindsImmediatelyBeforeWrite(t *testing.T) {
	bundle := custodyBundle(t)
	var stored object
	posts, preflight, rebound := 0, false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "fixture-reader-custody-token" {
			t.Error("request lost its private authentication header")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/mounts":
			_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"2"}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == retainedReaderMetadataPath:
			preflight = true
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == retainedReaderDataPath:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			custodyResponse(t, w, stored, 1)
		case r.Method == http.MethodPost && r.URL.Path == retainedReaderDataPath:
			posts++
			if !preflight || rebound != 1 {
				t.Error("custody mutation preceded final native-input rebinding")
			}
			var payload object
			if json.NewDecoder(r.Body).Decode(&payload) != nil || child(payload, "options")["cas"] != float64(0) || len(child(payload, "options")) != 1 || len(payload) != 2 {
				t.Error("write lost its create-only boundary")
			}
			stored = child(payload, "data")
			_, _ = w.Write([]byte(`{"data":{"version":1}}`))
		default:
			t.Errorf("unexpected credential operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
	if err != nil {
		t.Fatal(err)
	}
	defer store.client.CloseIdleConnections()
	verify := func(context.Context) error {
		if !preflight {
			t.Error("native verification happened before the final non-destructive preflight")
		}
		rebound++
		return nil
	}
	if err = store.ensure(context.Background(), bundle, verify); err != nil {
		t.Fatalf("valid create-only custody failed: %v", err)
	}
	if posts != 1 || rebound != 2 || stored["schema"] != "email-reader-inputs/v1" {
		t.Fatal("custody did not read back and rebind the exact stored inputs")
	}
	expected, err := readerCustodyDocument(bundle)
	if err != nil || !reflect.DeepEqual(stored, expected) {
		t.Fatal("stored credentials differ from the independently captured copy")
	}
	// A new invocation may verify the same immutable record, never overwrite it.
	if err = store.ensure(context.Background(), bundle, verify); err != nil || posts != 1 {
		t.Fatal("idempotent verification rewrote the existing custody record")
	}
}

// TestReaderCustodyRejectsChangedInputsAfterFinalPreflight models drift in the last interval before a credential write.
func TestReaderCustodyRejectsChangedInputsAfterFinalPreflight(t *testing.T) {
	posts, preflight := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		if r.URL.Path == "/v1/sys/mounts" {
			_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"2"}}}}`))
			return
		}
		if r.URL.Path == retainedReaderMetadataPath {
			preflight = true
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
	if err != nil {
		t.Fatal(err)
	}
	defer store.client.CloseIdleConnections()
	err = store.ensure(context.Background(), custodyBundle(t), func(context.Context) error {
		if preflight {
			return errors.New("native inputs changed")
		}
		return nil
	})
	if err == nil || posts != 0 {
		t.Fatal("changed native inputs authorized a credential write")
	}
}

// TestReaderCustodyRefusesAmbiguousExistingOrUnreadableRecords preserves existing history when API evidence cannot establish safe creation.
func TestReaderCustodyRefusesAmbiguousExistingOrUnreadableRecords(t *testing.T) {
	for _, failure := range []string{"different", "newer-version", "deleted", "destroyed", "absent-metadata", "unreadable", "duplicate-json", "oversized", "wrong-mount"} {
		t.Run(failure, func(t *testing.T) {
			posts := 0
			bundle := custodyBundle(t)
			document, err := readerCustodyDocument(bundle)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
				}
				if r.URL.Path == "/v1/sys/mounts" {
					version := "2"
					if failure == "wrong-mount" {
						version = "1"
					}
					_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"` + version + `"}}}}`))
					return
				}
				if r.URL.Path == retainedReaderMetadataPath {
					_, _ = w.Write([]byte(`{"data":{"current_version":1}}`))
					return
				}
				switch failure {
				case "deleted":
					w.WriteHeader(http.StatusNotFound)
				case "destroyed":
					_, _ = w.Write([]byte(`{"data":{"data":{},"metadata":{"version":1,"destroyed":true,"deletion_time":""}}}`))
				case "absent-metadata":
					_ = json.NewEncoder(w).Encode(object{"data": object{"data": document}})
				case "unreadable":
					w.WriteHeader(http.StatusForbidden)
				case "duplicate-json":
					_, _ = w.Write([]byte(`{"data":{},"data":{}}`))
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
				case "newer-version":
					custodyResponse(t, w, document, 2)
				default:
					custodyResponse(t, w, object{"schema": "foreign"}, 1)
				}
			}))
			defer server.Close()
			store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
			if err != nil {
				t.Fatal(err)
			}
			defer store.client.CloseIdleConnections()
			err = store.ensure(context.Background(), bundle, func(context.Context) error { return nil })
			if err == nil || posts != 0 || strings.Contains(err.Error(), "fixture-reader-custody-token") {
				t.Fatal("ambiguous custody state caused a write or exposed authentication")
			}
		})
	}
}

// TestReaderCustodyNeverFollowsCredentialRedirects uses a second server to detect any forwarded authentication request.
func TestReaderCustodyNeverFollowsCredentialRedirects(t *testing.T) {
	forwarded := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded++ }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
	if err != nil {
		t.Fatal(err)
	}
	defer store.client.CloseIdleConnections()
	if store.ensure(context.Background(), custodyBundle(t), func(context.Context) error { return nil }) == nil || forwarded != 0 {
		t.Fatal("a redirect could relay the custody authentication")
	}
}

// TestReaderCustodyCASConflictCannotOverwriteAnotherRecord distinguishes an identical concurrent winner from foreign custody.
func TestReaderCustodyCASConflictCannotOverwriteAnotherRecord(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign-winner", true: "identical-winner"}[identical], func(t *testing.T) {
			bundle := custodyBundle(t)
			winner, err := readerCustodyDocument(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if !identical {
				winner = object{"schema": "another-recovery-point"}
			}
			posts := 0
			var stored object
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sys/mounts" {
					_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"2"}}}}`))
					return
				}
				if r.Method == http.MethodPost {
					posts++
					stored = winner
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"errors":["check-and-set conflict"]}`))
					return
				}
				if r.URL.Path == retainedReaderDataPath && stored != nil {
					custodyResponse(t, w, stored, 1)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
			if err != nil {
				t.Fatal(err)
			}
			defer store.client.CloseIdleConnections()
			err = store.ensure(context.Background(), bundle, func(context.Context) error { return nil })
			if (err == nil) != identical || posts != 1 || !reflect.DeepEqual(stored, winner) {
				t.Fatal("CAS conflict rewrote a winner or accepted different recovery inputs")
			}
		})
	}
}

// TestReaderCustodyLostWriteResponseIsNotRetriedOrReportedStored models a committed request whose response connection is lost.
func TestReaderCustodyLostWriteResponseIsNotRetriedOrReportedStored(t *testing.T) {
	posts, postWriteReads := 0, 0
	committed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/mounts" {
			_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"2"}}}}`))
			return
		}
		if r.Method == http.MethodPost {
			posts++
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("HTTP fixture cannot model a lost committed response")
				w.WriteHeader(http.StatusInternalServerError)
				close(committed)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
			} else {
				_ = connection.Close()
			}
			close(committed)
			return
		}
		if posts != 0 {
			postWriteReads++
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	store, err := newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
	if err != nil {
		t.Fatal(err)
	}
	defer store.client.CloseIdleConnections()
	err = store.ensure(context.Background(), custodyBundle(t), func(context.Context) error { return nil })
	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture write never reached its committed-response boundary")
	}
	if err == nil || posts != 1 || postWriteReads != 0 {
		t.Fatal("uncertain write was retried or reclassified as verified")
	}
}
