package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestTransportInvocationRejectsDeclaredHTTPBeforeReaderAccess(t *testing.T) {
	root := configFixture(t)
	mutateFixture(t, root, storePath, "https://", "http://")
	for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": "verify-arc-app-transport"} {
		t.Setenv(name, value)
	}
	var output bytes.Buffer
	reject := func(configuration) outcome {
		t.Fatal("transport admission requested identity access")
		return pass
	}
	code := run([]string{"--transport"}, root, &output, runtimeModes{identity: reject, transport: reject})
	if code == 0 || output.String() != "ARC_APP_TRANSPORT=HOLD_TRANSPORT\n" {
		t.Fatalf("transport admission got exit=%d, output=%q", code, output.String())
	}
}

func transportTLSFixture(t *testing.T, hostname string, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTransportHealthRequiresExplicitHealthyResponseWithoutAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       outcome
	}{
		{"active", `{"initialized":true,"sealed":false,"standby":false,"version":"2.6.3","server_time_utc":1}`, 200, pass},
		{"standby", `{"initialized":true,"sealed":false,"standby":true}`, 200, pass},
		{"sealed despite HTTP success", `{"initialized":true,"sealed":true}`, 200, "FAIL_HEALTH"},
		{"uninitialized despite HTTP success", `{"initialized":false,"sealed":false}`, 200, "FAIL_HEALTH"},
		{"missing sealed", `{"initialized":true}`, 200, "FAIL_HEALTH"},
		{"missing initialized", `{"sealed":false}`, 200, "FAIL_HEALTH"},
		{"null sealed", `{"initialized":true,"sealed":null}`, 200, "FAIL_HEALTH"},
		{"null initialized", `{"initialized":null,"sealed":false}`, 200, "FAIL_HEALTH"},
		{"wrong type", `{"initialized":true,"sealed":"false"}`, 200, failAPI},
		{"duplicate field", `{"initialized":true,"sealed":true,"sealed":false}`, 200, failAPI},
		{"folded duplicate", `{"initialized":true,"sealed":true,"SEALED":false}`, 200, failAPI},
		{"trailing document", `{"initialized":true,"sealed":false}{}`, 200, failAPI},
		{"malformed", `{"initialized":true,"sealed":`, 200, failAPI},
		{"array", `[true,false]`, 200, failAPI},
		{"empty", `{}`, 200, "FAIL_HEALTH"},
		{"null document", `null`, 200, "FAIL_HEALTH"},
		{"oversized", strings.Repeat(" ", maxResponse) + `{"initialized":true,"sealed":false}`, 200, failAPI},
		{"sealed status", `{"initialized":true,"sealed":false}`, 503, failAPI},
		{"uninitialized status", `{"initialized":true,"sealed":false}`, 501, failAPI},
		{"standby parameter ignored", `{"initialized":true,"sealed":false}`, 429, failAPI},
		{"API unavailable", `sensitive-health-canary`, 500, failAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server, ca := transportTLSFixture(t, "openbao-arc.openbao.svc.cluster.local", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.RequestURI() != "/v1/sys/health?standbyok=true" || r.Header.Get("X-Vault-Token") != "" || r.Header.Get("Authorization") != "" || r.ContentLength != 0 {
					t.Error("health operation escaped its unauthenticated, fixed endpoint")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			if got := verifyTransportHealth(context.Background(), server.URL, ca); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if calls.Load() != 1 {
				t.Fatal("health mode performed additional API operations")
			}
		})
	}
}

func TestTransportHealthRejectsTLSBypassesRedirectsAndPartialReads(t *testing.T) {
	var sinkCalls atomic.Int32
	sink, sinkCA := transportTLSFixture(t, "openbao-arc.openbao.svc.cluster.local", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sinkCalls.Add(1)
		_, _ = io.WriteString(w, `{"initialized":true,"sealed":false}`)
	}))
	for _, tc := range []struct {
		name, hostname                                  string
		wrongCA, plaintext, redirect, partial, canceled bool
		want                                            outcome
	}{
		{"wrong hostname", "other.example", false, false, false, false, false, failTransport},
		{"wrong CA", "openbao-arc.openbao.svc.cluster.local", true, false, false, false, false, failTransport},
		{"plaintext", "openbao-arc.openbao.svc.cluster.local", false, true, false, false, false, holdTransport},
		{"redirect", "openbao-arc.openbao.svc.cluster.local", false, false, true, false, false, failAPI},
		{"partial read", "openbao-arc.openbao.svc.cluster.local", false, false, false, true, false, failAPI},
		{"canceled", "openbao-arc.openbao.svc.cluster.local", false, false, false, false, true, failTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, ca := transportTLSFixture(t, tc.hostname, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.redirect {
					w.Header().Set("Location", sink.URL+"/v1/sys/health?standbyok=true")
					w.WriteHeader(307)
					return
				}
				if tc.partial {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer connection.Close()
					_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{\"initialized\":true,\"sealed\":false}")
					return
				}
				_, _ = io.WriteString(w, `{"initialized":true,"sealed":false}`)
			}))
			if tc.wrongCA {
				ca = sinkCA
			}
			endpoint := server.URL
			if tc.plaintext {
				endpoint = strings.Replace(endpoint, "https:", "http:", 1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			if got := verifyTransportHealth(ctx, endpoint, ca); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	if sinkCalls.Load() != 0 {
		t.Fatal("health check followed a redirect")
	}
}

func TestTransportRuntimeNeverRequestsReaderAccessAndJoinsTunnel(t *testing.T) {
	for _, tc := range []struct {
		name, body, hostname string
		drift                bool
		want                 outcome
	}{
		{"healthy", `{"initialized":true,"sealed":false}`, "openbao-arc.openbao.svc.cluster.local", false, pass},
		{"sealed", `{"initialized":true,"sealed":true}`, "openbao-arc.openbao.svc.cluster.local", false, "FAIL_HEALTH"},
		{"TLS rejection", `{"initialized":true,"sealed":false}`, "other.example", false, failTransport},
		{"live HTTP drift", `{"initialized":true,"sealed":false}`, "openbao-arc.openbao.svc.cluster.local", true, holdTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, responses := liveFixture(t)
			server, ca := transportTLSFixture(t, tc.hostname, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/v1/sys/health?standbyok=true" || r.Header.Get("X-Vault-Token") != "" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected authentication operation")
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			// The reviewed and live store share this synthetic listener's real CA.
			originalCA := config.store.caBundle
			config.store.caBundle = base64.StdEncoding.EncodeToString(ca)
			responses["--namespace=arc-runners get secretstore openbao --output=json"] = []byte(strings.Replace(string(responses["--namespace=arc-runners get secretstore openbao --output=json"]), originalCA, config.store.caBundle, 1))
			forwarded, joined := false, false
			operations := runtimeOperations{
				execute: func(_ context.Context, args ...string) ([]byte, error) {
					body, ok := responses[strings.Join(args, " ")]
					if !ok {
						t.Fatal("transport mode requested a token or credential resource")
					}
					if tc.drift {
						body = []byte(strings.ReplaceAll(string(body), "https://", "http://"))
					}
					return body, nil
				},
				forward: func(_ context.Context, port int) (string, func(), error) {
					if port != 8204 {
						t.Fatal("wrong listener")
					}
					forwarded = true
					return server.URL, func() { joined = true }, nil
				},
				handshake: listenerTLS,
				health:    verifyTransportHealth,
				identity: func(context.Context, verificationOptions) outcome {
					t.Fatal("transport mode reached App identity")
					return pass
				},
			}
			if got := verifyTransportRuntime(context.Background(), config, operations); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if tc.drift {
				if forwarded {
					t.Fatal("live transport drift reached a tunnel")
				}
			} else if !forwarded || !joined {
				t.Fatal("transport tunnel was not joined before returning")
			}
		})
	}
}

func TestTransportCLIRequiresItsOwnProtectedConfirmation(t *testing.T) {
	root := configFixture(t)
	for _, tc := range []struct {
		name, field, value string
		admitted           bool
	}{
		{"protected", "", "", true},
		{"local", "GITHUB_ACTIONS", "false", false},
		{"pull request", "GITHUB_EVENT_NAME", "pull_request", false},
		{"branch", "GITHUB_REF", "refs/heads/codex/test", false},
		{"retry", "GITHUB_RUN_ATTEMPT", "2", false},
		{"other repo", "GITHUB_REPOSITORY", "devantler-tech/other", false},
		{"identity confirmation", "ARC_IDENTITY_CONFIRM", "verify-arc-app-identity", false},
		{"missing confirmation", "ARC_IDENTITY_CONFIRM", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": "verify-arc-app-transport"} {
				t.Setenv(name, value)
			}
			if tc.field != "" {
				t.Setenv(tc.field, tc.value)
			}
			var output bytes.Buffer
			called := false
			code := run([]string{"--transport"}, root, &output, runtimeModes{
				identity:  func(configuration) outcome { t.Fatal("transport mode dispatched identity verification"); return pass },
				transport: func(configuration) outcome { called = true; return pass },
			})
			want, exit := "ARC_APP_TRANSPORT=HOLD_INVOCATION\n", 1
			if tc.admitted {
				want, exit = "ARC_APP_TRANSPORT=PASS\n", 0
			}
			if called != tc.admitted || code != exit || output.String() != want {
				t.Fatalf("wrong mode admission: called=%v, exit=%d, output=%q", called, code, output.String())
			}
		})
	}
}

func TestTransportTunnelCancellationJoinsTheOwnedProcess(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "owned-forward.pid")
	script := "#!/bin/bash\nprintf '%s' \"$$\" >\"${ARC_FORWARD_PID}\"\nprintf 'Forwarding from 127.0.0.1:12345 -> 8204\\n'\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(root, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARC_FORWARD_PID", pidFile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, stop, err := forwardBao(ctx, 8204)
	if err != nil || endpoint != "https://127.0.0.1:12345" {
		t.Fatalf("owned tunnel did not start: %v", err)
	}
	t.Cleanup(stop)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid < 1 {
		t.Fatal("owned process not observed")
	}
	joined := make(chan struct{})
	go func() { stop(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("tunnel stop did not join the process")
	}
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		t.Fatal("owned port-forward process survived cleanup")
	}
}

func TestTransportCLIRejectsCredentialOutcomesAndSuppressesErrorMaterial(t *testing.T) {
	root := configFixture(t)
	for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": "verify-arc-app-transport"} {
		t.Setenv(name, value)
	}
	for _, status := range []outcome{holdEntry, failIdentity, failCleanup, "HOLD_READER", "sensitive-health-error-canary"} {
		var output bytes.Buffer
		code := run([]string{"--transport"}, root, &output, runtimeModes{transport: func(configuration) outcome { return status }})
		if code != 1 || output.String() != "ARC_APP_TRANSPORT=FAIL_API\n" {
			t.Fatalf("credential or unbounded outcome escaped transport: exit=%d, output=%q", code, output.String())
		}
	}
}

func TestTransportHealthRejectsMalformedTrustBeforeAPIRequest(t *testing.T) {
	if got := verifyTransportHealth(context.Background(), "https://127.0.0.1:1", []byte("sensitive-invalid-trust-canary")); got != holdTransport {
		t.Fatalf("malformed trust got %s", got)
	}
}
