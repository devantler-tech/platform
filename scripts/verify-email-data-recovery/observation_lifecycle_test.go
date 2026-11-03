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

func TestObservationForwarderChild(t *testing.T) {
	port := os.Getenv("EMAIL_OBSERVATION_FORWARD_PORT")
	if port == "" {
		return
	}
	got := os.Args[len(os.Args)-8:]
	want := []string{"--context=admin@prod", "--namespace=email-data-recovery-41561215415", "port-forward", "pod/reader", "0:8080", "--address=127.0.0.1", "--pod-running-timeout=15s"}
	if len(got) != 8 || got[0] != "--" || !reflect.DeepEqual(got[1:], want) {
		os.Exit(3)
	}
	fmt.Printf("Forwarding from 127.0.0.1:%s -> 8080\n", port)
	time.Sleep(30 * time.Second)
}

type observedReaderAPI struct {
	ns, pod              object
	secretReads, nsReads int
	replace              bool
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
		return json.Marshal(a.pod)
	case "Secret":
		a.secretReads++
		return json.Marshal(object{"metadata": object{"name": "private-mail-maintainer"}, "data": object{"password": base64.StdEncoding.EncodeToString([]byte("synthetic-account-passphrase"))}})
	default:
		return nil, fmt.Errorf("private-scope-canary")
	}
}

// This exercises the actual command protocol, forwarding process, HTTP client,
// account-session decoder and final UID fence. Only the external Kubernetes API
// and port-forward transport are controlled by local fixtures.
func TestObservationRetainedReaderLifecycle(t *testing.T) {
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
