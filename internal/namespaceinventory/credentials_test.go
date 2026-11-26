package namespaceinventory

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func fixtureConfig(server *httptest.Server) *clientcmdapi.Config {
	raw := clientcmdapi.NewConfig()
	raw.Clusters["prod"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	raw.AuthInfos["fixture-admin"] = &clientcmdapi.AuthInfo{Token: "private-fixture-token"}
	raw.Contexts["admin@prod"] = &clientcmdapi.Context{Cluster: "prod", AuthInfo: "fixture-admin"}
	// The current context must never choose the credential or destination.
	raw.CurrentContext = "unselected"
	return raw
}

func TestNativeClientUsesInlineTokenOrCertificateWithVerifiedTLS(t *testing.T) {
	for _, mode := range []string{"token", "certificate"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "token" && r.Header.Get("Authorization") != "Bearer private-fixture-token" {
					t.Error("inline token did not reach the reviewed TLS peer")
				}
				if mode == "certificate" && (r.TLS == nil || len(r.TLS.PeerCertificates) == 0) {
					t.Error("inline client certificate was not used")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
			server.StartTLS()
			t.Cleanup(server.Close)
			raw := fixtureConfig(server)
			if mode == "certificate" {
				key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				auth := raw.AuthInfos["fixture-admin"]
				auth.Token = ""
				auth.ClientCertificateData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
				auth.ClientKeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
			}
			client, endpoint, err := newProductionClient(raw)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Get(endpoint.String() + "/api")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusNoContent {
				t.Fatal("native TLS request failed")
			}
		})
	}
}

func TestNativeClientRefusesDynamicExternalOrAmbiguousCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("credential validation issued an HTTP request") }))
	t.Cleanup(server.Close)
	for _, failure := range []string{"missing context", "external CA", "insecure TLS", "proxy override", "exec plugin", "auth provider", "token file", "certificate file", "key file", "impersonation", "basic auth", "no inline auth", "partial certificate", "mixed auth", "opaque API root"} {
		t.Run(failure, func(t *testing.T) {
			raw := fixtureConfig(server)
			cluster, auth := raw.Clusters["prod"], raw.AuthInfos["fixture-admin"]
			switch failure {
			case "missing context":
				delete(raw.Contexts, "admin@prod")
			case "external CA":
				cluster.CertificateAuthority = "/never/read"
			case "insecure TLS":
				cluster.InsecureSkipTLSVerify = true
			case "proxy override":
				cluster.ProxyURL = "http://unreviewed.invalid"
			case "exec plugin":
				auth.Exec = &clientcmdapi.ExecConfig{Command: "must-not-execute"}
			case "auth provider":
				auth.AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "must-not-refresh"}
			case "token file":
				auth.TokenFile = "/never/read"
			case "certificate file":
				auth.ClientCertificate = "/never/read"
			case "key file":
				auth.ClientKey = "/never/read"
			case "impersonation":
				auth.Impersonate = "other-user"
			case "basic auth":
				auth.Username, auth.Password = "other-user", "private-fixture-password"
			case "no inline auth":
				auth.Token = ""
			case "partial certificate":
				auth.ClientCertificateData = []byte("partial")
			case "mixed auth":
				auth.ClientCertificateData, auth.ClientKeyData = []byte("certificate"), []byte("key")
			case "opaque API root":
				cluster.Server = "https:opaque-destination"
			}
			if _, _, err := newProductionClient(raw); err == nil {
				t.Fatal("unsupported credential profile accepted")
			} else if strings.Contains(err.Error(), "private-fixture") || strings.Contains(err.Error(), "/never/read") {
				t.Fatal("credential detail escaped through error")
			}
		})
	}
}

func TestCredentialLoaderRefusesOutsideProtectedDispatch(t *testing.T) {
	if _, _, err := LoadProtectedClient(func(string) string { return "" }); err == nil {
		t.Fatal("credential loader admitted an unverified dispatch")
	}
}

func TestBenchmarkDirectClientPinsAddressAndNeverFollowsRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the old endpoint must not receive direct-client credentials")
	}))
	t.Cleanup(server.Close)
	raw := fixtureConfig(server)
	raw.Clusters["prod"].TLSServerName = "old-dns-name.invalid"
	direct := netip.MustParseAddr("203.0.113.10")
	client, endpoint, err := newProductionClientAt(raw, direct)
	if err != nil || endpoint.String() != "https://203.0.113.10:6443" {
		t.Fatalf("direct endpoint is not pinned: %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("direct client accepted a request for a different peer")
	}
	if !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("direct client permits a redirect")
	}
	for _, test := range []struct{ method, query string }{
		{http.MethodPatch, ""},
		{http.MethodPatch, "?dryRun=All&dryRun="},
		{http.MethodDelete, "?dryRun=All"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), test.method, endpoint.String()+"/api/v1/persistentvolumes/scratch"+test.query, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "only observations and server dry-runs") {
			t.Fatal("direct API client did not reject a write-capable request locally")
		}
	}
}

func TestBenchmarkCredentialLoaderRefusesOutsideProtectedDispatch(t *testing.T) {
	if _, _, err := LoadBenchmarkAdmissionClient(func(string) string { return "" }, netip.MustParseAddr("203.0.113.10"), ""); err == nil {
		t.Fatal("direct credential loader admitted an unverified dispatch")
	}
}

func nativeSeedAdmissionDispatch() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"GITHUB_SHA":          strings.Repeat("a", 40), "GITHUB_RUN_ID": "1234",
		"LONGHORN_V2_OPERATION": "prepare-native-v1-seed", "LONGHORN_V2_CONFIRM": "prepare-longhorn-native-v1-seed",
		// Deliberately invalid: no credential file may be opened by this test.
		"KUBECONFIG": "private-credential-path-canary",
	}
}

func TestNativeSeedReachesTheExistingAdmissionCredentialBoundary(t *testing.T) {
	valid := nativeSeedAdmissionDispatch()
	getenv := func(key string) string { return valid[key] }
	client, endpoint, err := LoadBenchmarkAdmissionClient(getenv, netip.MustParseAddr("203.0.113.10"), "")
	if client != nil || endpoint != nil || err == nil || err.Error() != "fixed benchmark admission credentials and direct IPv4 peer are required" {
		t.Fatal("the confirmed seed did not reach the unchanged credential boundary:", err)
	}
	benchmark := func(key string) string {
		if key == "LONGHORN_V2_OPERATION" {
			return "benchmark-v1"
		}
		if key == "LONGHORN_V2_CONFIRM" {
			return "benchmark-longhorn-storage"
		}
		return valid[key]
	}
	if client, endpoint, err := LoadBenchmarkAdmissionClient(benchmark, netip.MustParseAddr("203.0.113.10"), ""); client != nil || endpoint != nil || err == nil || err.Error() != "fixed benchmark admission credentials and direct IPv4 peer are required" {
		t.Fatal("the original confirmed benchmark lost its credential boundary:", err)
	}
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "GITHUB_WORKFLOW_REF", "LONGHORN_V2_OPERATION", "LONGHORN_V2_CONFIRM", "LONGHORN_V2_NODE", "LONGHORN_V2_DATABASE"} {
		t.Run(key, func(t *testing.T) {
			changed := func(name string) string {
				if name == key {
					return "foreign"
				}
				return valid[name]
			}
			client, endpoint, err := LoadBenchmarkAdmissionClient(changed, netip.MustParseAddr("203.0.113.10"), "")
			if client != nil || endpoint != nil || err == nil || err.Error() != "protected benchmark admission dispatch is unverified" {
				t.Fatal("a foreign or targeted seed reached credential loading:", err)
			}
		})
	}
	for _, pair := range [][2]string{{"benchmark-v1", "prepare-longhorn-native-v1-seed"}, {"prepare-native-v1-seed", "benchmark-longhorn-storage"}, {"restore-native-v2-seed", "prepare-longhorn-native-v1-seed"}} {
		changed := func(key string) string {
			if key == "LONGHORN_V2_OPERATION" {
				return pair[0]
			}
			if key == "LONGHORN_V2_CONFIRM" {
				return pair[1]
			}
			return valid[key]
		}
		if _, _, err := LoadBenchmarkAdmissionClient(changed, netip.MustParseAddr("203.0.113.10"), ""); err == nil || err.Error() != "protected benchmark admission dispatch is unverified" {
			t.Fatal("mismatched operation/confirmation pair reached credentials:", err)
		}
	}
}

func TestAdmissionLoaderBuildsConfirmedClientsFromPrivateMemoryCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the protected credential directory is Linux memory-backed storage")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("loader contacted an API") }))
	t.Cleanup(server.Close)
	directory, err := os.MkdirTemp("/dev/shm", "longhorn-v2.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	raw, err := clientcmd.Write(*fixtureConfig(server))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "kubeconfig")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dispatch := range []struct {
		operation    string
		confirmation string
		originalRun  string
	}{
		{operation: "benchmark-v1", confirmation: "benchmark-longhorn-storage"},
		{operation: "prepare-native-v1-seed", confirmation: "prepare-longhorn-native-v1-seed"},
		{operation: "recover-bound-v1-benchmark", confirmation: "recover-bound-longhorn-v1-benchmark:99", originalRun: "99"},
		{operation: "reclaim-native-v2-seed", confirmation: "reclaim-longhorn-native-v2-seed:99", originalRun: "99"},
	} {
		valid := nativeSeedAdmissionDispatch()
		valid["LONGHORN_V2_OPERATION"], valid["LONGHORN_V2_CONFIRM"] = dispatch.operation, dispatch.confirmation
		valid["KUBECONFIG"], valid["LONGHORN_V2_CREDENTIAL_DIR"] = path, directory
		client, endpoint, err := LoadBenchmarkAdmissionClient(func(key string) string { return valid[key] }, netip.MustParseAddr("203.0.113.10"), dispatch.originalRun)
		if err != nil || client == nil || endpoint == nil || endpoint.String() != "https://203.0.113.10:6443" || client.Timeout != 20*time.Second {
			t.Fatal("confirmed operation could not construct its fixed client:", dispatch.operation, err)
		}
		response, err := client.Get(server.URL + "/version")
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			t.Fatal("loaded client addressed a different peer")
		}
		client.CloseIdleConnections()
	}
}
