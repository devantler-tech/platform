package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const clusterAPIPath = "/apis/postgresql.cnpg.io/v1/namespaces/wedding-app/clusters/wedding-db"

func clusterGetArgs() []string {
	return []string{"get", "cluster.postgresql.cnpg.io", clusterName, "-n", namespace, "--show-managed-fields=true", "-o", "json"}
}

// transportFixture uses only synthetic, in-memory certificates. The TLS server
// offers HTTP/2, so the caller must positively restrict itself to HTTP/1.
func transportFixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, object) {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	cert := s.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal("synthetic certificate fixture failed")
	}
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	privateKey := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	config := object{"apiVersion": "v1", "kind": "Config", "current-context": "admin@prod", "preferences": object{},
		"contexts": []object{{"name": "admin@prod", "context": object{"cluster": "synthetic", "user": "synthetic"}}},
		"clusters": []object{{"name": "synthetic", "cluster": object{"server": s.URL, "certificate-authority-data": certificate}}},
		"users":    []object{{"name": "synthetic", "user": object{"client-certificate-data": certificate, "client-key-data": privateKey}}}}
	return s, config
}

func clusterResponse() object {
	s, _ := completedFixture()
	s.cluster["apiVersion"], s.cluster["kind"] = "postgresql.cnpg.io/v1", "Cluster"
	return s.cluster
}

func configuredCommand(t *testing.T, config object, fallback *atomic.Int32) recoveryCommand {
	t.Helper()
	configReads := 0
	command, closeTransport, err := protectedClusterCommand(context.Background(), func(_ context.Context, args []string, body []byte) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"config", "view", "--minify", "--flatten", "--raw", "-o", "json"}) && body == nil {
			configReads++
			return json.Marshal(config)
		}
		fallback.Add(1)
		return nil, errors.New("fixture-private-CLI-error")
	})
	if err != nil || configReads != 1 {
		t.Fatal("protected transport did not resolve exactly one selected context")
	}
	t.Cleanup(closeTransport)
	return command
}

// TestClusterTransportReusesTheFinalReadConnection removes discovery and the
// implicit target GET that kubectl patch otherwise adds to the CAS window.
func TestClusterTransportReusesTheFinalReadConnection(t *testing.T) {
	var trace []string
	var connections []string
	var fallback atomic.Int32
	snapshot := clusterResponse()
	patch, _ := json.Marshal(pausePatch(snapshot, str(snapshot, "status", "currentPrimary")))
	_, config := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, r.Method)
		connections = append(connections, r.RemoteAddr)
		if r.URL.Path != clusterAPIPath || r.ProtoMajor != 1 || len(r.TLS.PeerCertificates) != 1 {
			t.Error("request changed its scope, authentication or no-replay protocol")
		}
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(patch) || r.URL.RawQuery != "fieldManager="+fieldManager || r.Header.Get("Content-Type") != "application/json-patch+json" {
				t.Error("conditional patch bytes or ownership changed")
			}
		} else if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Error("unexpected discovery or metadata request")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
	command := configuredCommand(t, config, &fallback)
	if _, err := command(context.Background(), clusterGetArgs(), nil); err != nil {
		t.Fatal("fresh cluster read failed")
	}
	if _, err := command(context.Background(), patchArgs("cluster", snapshot), patch); err != nil {
		t.Fatal("guarded patch failed")
	}
	if !reflect.DeepEqual(trace, []string{"GET", "PATCH"}) || connections[0] != connections[1] || fallback.Load() != 0 {
		t.Fatalf("unexpected trace=%v fallback=%d", trace, fallback.Load())
	}
}

// TestRejectedClusterPatchDoesNotRetry covers refusal, throttling, redirects,
// server errors and an ambiguous lost response on an already reused connection.
func TestRejectedClusterPatchDoesNotRetry(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 422, 429, 500, 503, 307, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var patches, redirects, fallback atomic.Int32
			snapshot := clusterResponse()
			_, config := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != clusterAPIPath {
					redirects.Add(1)
				}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(snapshot)
					return
				}
				patches.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error("lost-response fixture failed")
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Retry-After", "0")
				w.Header().Set("Location", "/redirect-private-target")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "fixture-private-response")
			})
			command := configuredCommand(t, config, &fallback)
			if _, err := command(context.Background(), clusterGetArgs(), nil); err != nil {
				t.Fatal("warm-up read failed")
			}
			c := client{command: command}
			err := c.patch(context.Background(), "cluster", snapshot, pausePatch(snapshot, str(snapshot, "status", "currentPrimary")))
			if err == nil || strings.Contains(err.Error(), "private") || patches.Load() != 1 || redirects.Load() != 0 || fallback.Load() != 0 {
				t.Fatalf("refusal not single-shot: patches=%d redirects=%d fallback=%d error=%v", patches.Load(), redirects.Load(), fallback.Load(), err)
			}
		})
	}
}

// TestProtectedConfigRefusesChangedIdentity prevents incomplete credentials or
// ignored auth/impersonation fields from changing the effective protected user.
func TestProtectedConfigRefusesChangedIdentity(t *testing.T) {
	for _, failure := range []string{"context", "context name", "cluster reference", "user reference", "extra context", "extra cluster", "extra user", "http", "URL user", "query", "fragment", "base path", "missing CA", "bad certificate", "bad key", "insecure-skip-tls-verify", "proxy-url", "token", "tokenFile", "username", "password", "exec", "auth-provider", "as", "as-uid", "as-groups", "as-user-extra", "client-certificate", "client-key"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int32
			_, config := transportFixture(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
			selected := config["contexts"].([]object)
			clusters := config["clusters"].([]object)
			users := config["users"].([]object)
			cluster, user := at(clusters[0], "cluster"), at(users[0], "user")
			switch failure {
			case "context":
				config["current-context"] = "oidc@prod"
			case "context name":
				selected[0]["name"] = "other"
			case "cluster reference":
				at(selected[0], "context")["cluster"] = "other"
			case "user reference":
				at(selected[0], "context")["user"] = "other"
			case "extra context":
				config["contexts"] = append(selected, selected[0])
			case "extra cluster":
				config["clusters"] = append(clusters, clusters[0])
			case "extra user":
				config["users"] = append(users, users[0])
			case "http":
				cluster["server"] = "http://example.invalid"
			case "URL user":
				cluster["server"] = "https://private:private@example.invalid"
			case "query":
				cluster["server"] = "https://example.invalid?private=true"
			case "fragment":
				cluster["server"] = "https://example.invalid#private"
			case "base path":
				cluster["server"] = "https://example.invalid/private"
			case "missing CA":
				delete(cluster, "certificate-authority-data")
			case "bad certificate":
				user["client-certificate-data"] = "fixture-private-certificate"
			case "bad key":
				user["client-key-data"] = "fixture-private-key"
			case "insecure-skip-tls-verify", "proxy-url":
				cluster[failure] = "fixture-private-config"
			default:
				user[failure] = "fixture-private-config"
			}
			command, closeTransport, err := protectedClusterCommand(context.Background(), func(context.Context, []string, []byte) ([]byte, error) { return json.Marshal(config) })
			if err == nil || command != nil || closeTransport != nil || requests.Load() != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("unsupported credential shape did not fail closed before API access")
			}
		})
	}
}

func TestProtectedConfigRefusesUnboundedOrMalformedExport(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("null"), []byte("{"), []byte(`{"current-context":"admin@prod"}`), []byte(strings.Repeat("x", transportByteLimit+1))} {
		command, closeTransport, err := protectedClusterCommand(context.Background(), func(context.Context, []string, []byte) ([]byte, error) { return data, nil })
		if err == nil || command != nil || closeTransport != nil {
			t.Fatal("incomplete or oversized export accepted")
		}
	}
}

func TestClusterTransportDryRunAndScope(t *testing.T) {
	var requests, fallback atomic.Int32
	snapshot := clusterResponse()
	patch, _ := json.Marshal(pausePatch(snapshot, str(snapshot, "status", "currentPrimary")))
	_, config := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "PATCH" || r.URL.Path != clusterAPIPath || r.URL.RawQuery != "dryRun=All&fieldManager="+fieldManager || string(body) != string(patch) {
			t.Error("dry-run request lost its exact non-persisting patch contract")
		}
		_ = json.NewEncoder(w).Encode(snapshot)
	})
	command := configuredCommand(t, config, &fallback)
	if _, err := command(context.Background(), append(patchArgs("cluster", snapshot), "--dry-run=server"), patch); err != nil {
		t.Fatal("fixed server-side dry run failed")
	}
	for _, args := range [][]string{{"delete", "cluster.postgresql.cnpg.io", clusterName}, {"patch", "cluster", clusterName}, {"get", "clusters.postgresql.cnpg.io"}, {"get", "cluster.postgresql.cnpg.io", "other"}, append(patchArgs("cluster", snapshot), "--dry-run=client")} {
		if _, err := command(context.Background(), args, patch); err == nil {
			t.Fatal("unsupported Cluster operation accepted")
		}
	}
	if requests.Load() != 1 || fallback.Load() != 0 {
		t.Fatal("unsupported operation reached the API or CLI")
	}
	if _, err := command(context.Background(), []string{"get", "pods", "-n", namespace, "-o", "json"}, nil); err == nil || fallback.Load() != 1 {
		t.Fatal("other resources did not retain their original transport")
	}
}

func TestClusterTransportRefusesUnverifiableResponse(t *testing.T) {
	for _, failure := range []string{"TLS server name", "wrong CA", "oversized", "truncated", "wrong resource", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			var requests, fallback atomic.Int32
			server, config := transportFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				switch failure {
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", transportByteLimit+1))
				case "truncated":
					_, _ = io.WriteString(w, "{")
				default:
					_, _ = io.WriteString(w, `{"kind":"Status"}`)
				}
			})
			if failure == "TLS server name" {
				at(config["clusters"].([]object)[0], "cluster")["tls-server-name"] = "wrong.invalid"
			}
			if failure == "wrong CA" {
				certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
				if err != nil {
					t.Fatal("synthetic certificate parse failed")
				}
				certificate.RawSubject = nil
				certificate.Subject.CommonName = "unrelated synthetic CA"
				certificate.IsCA, certificate.BasicConstraintsValid = true, true
				certificate.KeyUsage = x509.KeyUsageCertSign
				der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, certificate.PublicKey, server.TLS.Certificates[0].PrivateKey)
				if err != nil {
					t.Fatal("synthetic CA fixture failed")
				}
				at(config["clusters"].([]object)[0], "cluster")["certificate-authority-data"] = base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			}
			command := configuredCommand(t, config, &fallback)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			if _, err := command(ctx, clusterGetArgs(), nil); err == nil || strings.Contains(err.Error(), "private") || fallback.Load() != 0 {
				t.Fatal("unverifiable response was accepted or leaked")
			}
			if (failure == "TLS server name" || failure == "wrong CA" || failure == "cancelled") && requests.Load() != 0 {
				t.Fatal("request crossed the TLS or cancellation gate")
			}
		})
	}
}

// TestKubeconfigExportHelper is a subprocess fixture, never a real kubectl call.
func TestKubeconfigExportHelper(t *testing.T) {
	switch os.Getenv("WEDDING_CONFIG_TEST_HELPER") {
	case "stdout":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", transportByteLimit+1))
	case "stderr":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", transportByteLimit+1))
	case "blocked":
		time.Sleep(time.Minute)
	default:
		return
	}
	os.Exit(0)
}

func TestKubeconfigCaptureBoundAndDeadline(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			deadline := 5 * time.Second
			if mode == "blocked" {
				deadline = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKubeconfigExportHelper$")
			cmd.Env = append(os.Environ(), "WEDDING_CONFIG_TEST_HELPER="+mode)
			if data, err := captureConfig(cmd); err == nil || data != nil || strings.Contains(err.Error(), "xxx") {
				t.Fatal("credential export exceeded its capture/deadline gate")
			}
			if mode != "blocked" && ctx.Err() != nil {
				t.Fatal("deadline masked a broken byte bound")
			}
		})
	}
}

func TestCredentialCaptureBoundsIOCopy(t *testing.T) {
	var capture limitedCapture
	reader := io.LimitReader(strings.NewReader(strings.Repeat("x", transportByteLimit+1)), transportByteLimit+1)
	if _, err := io.Copy(&capture, reader); err == nil || capture.Len() > transportByteLimit {
		t.Fatal("io.Copy bypassed the credential byte bound")
	}
}

// TestProtectedTransportWiring guards the production call site, not just the
// injectable request helper. Default OIDC planning and legacy repairs stay CLI.
func TestProtectedTransportWiring(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	gate := strings.Index(source, "contextName := \"oidc@prod\"")
	transport := strings.Index(source, "if *quarantine && (*execute || *diagnose) {")
	proof := strings.Index(source, "c.source = func(ctx context.Context) error {")
	if gate < 0 || transport < gate || proof < transport || !strings.Contains(source[transport:proof], "protectedClusterCommand(ctx, c.command)") || !strings.Contains(source[transport:proof], "defer closeTransport()") {
		t.Fatal("persistent transport escaped protected completed-join dispatch or initialization order")
	}
}
