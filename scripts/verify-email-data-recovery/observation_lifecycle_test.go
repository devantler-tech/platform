package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type observationFailingWriter struct {
	base      io.Writer
	remaining int
}

func (w *observationFailingWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	w.remaining--
	return w.base.Write(p)
}

func TestObservationForwarderChild(_ *testing.T) {
	port := os.Getenv("EMAIL_OBSERVATION_FORWARD_PORT")
	if port == "" {
		return
	}
	got := os.Args[len(os.Args)-8:]
	want := []string{"--context=admin@prod", "--namespace=email-data-recovery-41561215415", "port-forward", "pod/reader", "0:8080", "--address=127.0.0.1", "--pod-running-timeout=15s"}
	serving := append([]string(nil), want...)
	serving[1], serving[3] = "--namespace=private-mail", "pod/private-mail-0"
	switch {
	case got[0] == "--" && reflect.DeepEqual(got[1:], want):
	case got[0] == "--" && reflect.DeepEqual(got[1:], serving) && os.Getenv("EMAIL_OBSERVATION_SOURCE_PORT") != "":
		port = os.Getenv("EMAIL_OBSERVATION_SOURCE_PORT")
	default:
		os.Exit(3)
	}
	fmt.Printf("Forwarding from 127.0.0.1:%s -> 8080\n", port)
	time.Sleep(30 * time.Second)
}

type observedReaderAPI struct {
	ns, pod                                              object
	servingPod, servingClaim, servingVolume, readerClaim object
	password                                             string
	secretReads, nsReads                                 int
	replace                                              bool
}

func (a *observedReaderAPI) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	if len(input) != 0 || len(args) < 3 || args[0] != "get" {
		return nil, fmt.Errorf("private-mutation-canary")
	}
	switch args[1] {
	case "Namespace":
		a.nsReads++
		if a.replace && a.nsReads > 1 {
			child(a.ns, "metadata")["uid"] = "replacement"
		}
		return json.Marshal(a.ns)
	case "Pod":
		if args[2] == "private-mail-0" && a.servingPod != nil {
			return json.Marshal(a.servingPod)
		}
		return json.Marshal(a.pod)
	case "PersistentVolumeClaim":
		if args[2] == "private-mail-data" && a.servingClaim != nil {
			return json.Marshal(a.servingClaim)
		}
		if args[2] == "data" && a.readerClaim != nil {
			return json.Marshal(a.readerClaim)
		}
	case "PersistentVolume":
		if args[2] == "serving-volume" && a.servingVolume != nil {
			return json.Marshal(a.servingVolume)
		}
	case "Secret":
		a.secretReads++
		password := a.password
		if password == "" {
			password = "synthetic-account-passphrase"
		}
		return json.Marshal(object{"metadata": object{"name": "private-mail-maintainer"}, "data": object{"password": base64.StdEncoding.EncodeToString([]byte(password))}})
	}
	return nil, fmt.Errorf("private-scope-canary")
}

// This exercises the actual command protocol, forwarding process, HTTP client,
// account-session decoder and final UID fence. Only the external Kubernetes API
// and port-forward transport are controlled by local fixtures.
func observationForwarderFixture(t *testing.T) string {
	t.Helper()
	t.Chdir("../..")
	image, err := loadRecoveryImage()
	if err != nil {
		t.Fatal("fixture declaration missing")
	}
	testbin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(testbin, "'", "'\\''") + "' -test.run='^TestObservationForwarderChild$' -- \"$@\"\n"
	if os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700) != nil {
		t.Fatal("fixture forwarder unavailable")
	}
	t.Setenv("PATH", bin)
	return image
}

func TestObservationRetainedReaderLifecycle(t *testing.T) {
	image := observationForwarderFixture(t)
	for _, tc := range []struct {
		name            string
		health, session int
		replace, badPod bool
		want            string
		secretReads     int
	}{
		{"readable", 200, 200, false, false, "EMAIL_RECOVERY_OBSERVATION=PASS readiness=VERIFIED session=VERIFIED writes=0", 1},
		{"not ready", 503, 200, false, false, "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-health", 0},
		{"authentication refused", 200, 403, false, false, "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-session-http", 1},
		{"replaced", 200, 200, true, false, "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-replaced", 1},
		{"changed executable", 200, 200, false, true, "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-identity", 0},
		{"output closed", 200, 200, false, false, "", 0},
		{"final output closed", 200, 200, false, false, "EMAIL_RECOVERY_OBSERVATION_HEALTH status=200 writes=0", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("observer attempted a mailbox write")
				}
				switch r.URL.Path {
				case "/healthz/ready":
					if r.Header.Get("Authorization") != "" {
						t.Error("health probe carried a credential")
					}
					w.WriteHeader(tc.health)
				case "/jmap/session":
					user, password, ok := r.BasicAuth()
					if !ok || user != "ned@devantler.tech" || password != "synthetic-account-passphrase" {
						t.Error("account credential was misbound")
					}
					w.WriteHeader(tc.session)
					_, _ = w.Write([]byte(`{"primaryAccounts":{"urn:ietf:params:jmap:mail":"test-account"},"accounts":{"test-account":{"isReadOnly":false}}}`))
				default:
					t.Error("observer read an unexpected path")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			t.Setenv("EMAIL_OBSERVATION_FORWARD_PORT", strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
			ns := recoveryManifest(retainedRecovery, "Namespace", retainedRecovery.namespace, "", "v1")
			child(ns, "metadata")["uid"] = "namespace-original"
			pod := recoveryReader(retainedRecovery, image)
			child(pod, "metadata")["uid"] = "reader-original"
			pod["status"] = object{"phase": "Running"}
			if tc.badPod {
				items(child(pod, "spec"), "containers")[0]["command"] = []any{"/foreign"}
			}
			api := &observedReaderAPI{ns: ns, pod: pod, replace: tc.replace}
			e := observationEnv()
			var out bytes.Buffer
			var writer io.Writer = &out
			if tc.name == "output closed" {
				writer = &observationFailingWriter{base: &out}
			}
			if tc.name == "final output closed" {
				writer = &observationFailingWriter{base: &out, remaining: 1}
			}
			rc := runRecoveryObservation(context.Background(), func(k string) string { return e[k] }, api, writer)
			if (rc == 0) != (tc.name == "readable") || !strings.Contains(out.String(), tc.want) || api.secretReads != tc.secretReads {
				t.Fatalf("unexpected observation result: %q reads=%d exit=%d", out.String(), api.secretReads, rc)
			}
			if strings.Contains(out.String(), "synthetic-account") || strings.Contains(out.String(), "private-") {
				t.Fatal("observation exposed private details")
			}
		})
	}
}

// TestObservationVerifiesRetainedMessageWithoutBackupOrCleanup exercises the
// read-only comparison and checks that failure stages reveal no mailbox data.
func TestObservationVerifiesRetainedMessageWithoutBackupOrCleanup(t *testing.T) {
	image := observationForwarderFixture(t)
	for _, tc := range []struct {
		name, readerUID, want string
	}{
		{"matching", "reader-original", "EMAIL_RETAINED_PARITY=VERIFIED source_writes=0 original_trial=UNVERIFIED"},
		{"missing diagnostic", "reader-original", "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=retained-reader-mailbox-missing"},
		{"different diagnostic", "reader-original", "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=retained-source-mismatch"},
		{"invalid source query", "reader-original", "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=retained-source-email-query"},
		{"replaced reader", "foreign-reader", "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &retainedMessageServer{mutate: func(method string, _ int, result object) {
				if tc.name == "missing diagnostic" && method == "Mailbox/get" {
					result["list"] = items(result, "list")[:1]
				}
			}}
			s := &retainedMessageServer{mutate: func(method string, _ int, result object) {
				if tc.name == "different diagnostic" && method == "Email/get" {
					items(result, "list")[0]["blobId"] = "different-blob"
				}
				if tc.name == "invalid source query" && method == "Email/query" {
					result["total"] = "private-provider-canary"
				}
			}}
			t.Setenv("EMAIL_OBSERVATION_FORWARD_PORT", strings.TrimPrefix(retainedMessageEndpoint(t, r), "http://127.0.0.1:"))
			t.Setenv("EMAIL_OBSERVATION_SOURCE_PORT", strings.TrimPrefix(retainedMessageEndpoint(t, s), "http://127.0.0.1:"))
			ns := recoveryManifest(retainedRecovery, "Namespace", retainedRecovery.namespace, "", "v1")
			child(ns, "metadata")["uid"] = "namespace-original"
			pod := recoveryReader(retainedRecovery, image)
			child(pod, "metadata")["uid"] = "reader-original"
			pod["status"] = object{"phase": "Running"}
			p, c, v := servingFixture()
			items(child(p, "spec"), "containers")[0]["image"] = image
			claim := recoveryManifest(retainedRecovery, "PersistentVolumeClaim", "data", retainedRecovery.namespace, "v1")
			child(claim, "metadata")["uid"] = "claim-original"
			claim["spec"] = object{"volumeName": "reader-volume"}
			claim["status"] = object{"phase": "Bound"}
			api := &observedReaderAPI{ns: ns, pod: pod, servingPod: p, servingClaim: c, servingVolume: v, readerClaim: claim, password: "private-retained-fixture"}
			e := observationEnv()
			e["EMAIL_DATA_RECOVERY_CONFIRM"] = "verify-retained-email-parity"
			e["EMAIL_RETAINED_READER_UID"] = tc.readerUID
			e["EMAIL_RETAINED_CLAIM_UID"] = "claim-original"
			var out bytes.Buffer
			rc := runRecoveryObservation(context.Background(), func(k string) string { return e[k] }, api, &out)
			if (rc == 0) != (tc.name == "matching") || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("wrong parity result: %q exit=%d", out.String(), rc)
			}
			if strings.Contains(out.String(), "diagnostic-blob") || strings.Contains(out.String(), retainedFixtureNonce) || strings.Contains(out.String(), api.password) || strings.Contains(out.String(), "private-provider-canary") {
				t.Fatal("mailbox evidence escaped the observer")
			}
		})
	}
}
