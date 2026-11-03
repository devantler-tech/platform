package namespaceinventory

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
