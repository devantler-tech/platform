package transport_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestNativeTLSAndCertificateReload exercises the declared supplemental listener
// with the actual pinned server, synthetic certificates and an empty local store.
// It never initializes the store or accesses a production credential.
func TestNativeTLSAndCertificateReload(t *testing.T) {
	binary := os.Getenv("OPENBAO_TLS_TEST_BINARY")
	if binary == "" {
		t.Skip("native binary is supplied by the mandatory chart regression")
	}
	version, err := exec.Command(binary, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "OpenBao v2.6.3 ") {
		t.Fatal("expected the verified OpenBao 2.6.3 binary")
	}
	root := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-transport-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	const hostname = "openbao-arc.openbao.svc.cluster.local"
	writeLeaf := func(serial int64) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{hostname},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		for name, block := range map[string]*pem.Block{"tls.crt": {Type: "CERTIFICATE", Bytes: der}, "tls.key": {Type: "PRIVATE KEY", Bytes: private}} {
			if err := os.WriteFile(filepath.Join(root, name), pem.EncodeToMemory(block), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeLeaf(2)
	freePort := func() int {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return port
	}
	plainPort, tlsPort, clusterPort, tlsClusterPort := freePort(), freePort(), freePort(), freePort()
	base := fmt.Sprintf("disable_mlock = true\napi_addr = \"http://127.0.0.1:%d\"\ncluster_addr = \"https://127.0.0.1:%d\"\nlistener \"tcp\" {\n address = \"127.0.0.1:%d\"\n cluster_address = \"127.0.0.1:%d\"\n tls_disable = true\n}\nstorage \"file\" {\n path = %q\n}\n", plainPort, clusterPort, plainPort, clusterPort, filepath.Join(root, "data"))
	if err := os.WriteFile(filepath.Join(root, "base.hcl"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../k8s/providers/hetzner/infrastructure/controllers/openbao/transport/listener-config-map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var declaration struct{ Data map[string]string }
	if err := yaml.Unmarshal(data, &declaration); err != nil {
		t.Fatal(err)
	}
	listenerConfig := declaration.Data["listener.hcl"]
	for old, next := range map[string]string{
		"0.0.0.0:8204":             fmt.Sprintf("127.0.0.1:%d", tlsPort),
		"127.0.0.1:8205":           fmt.Sprintf("127.0.0.1:%d", tlsClusterPort),
		"/openbao/arc-tls/tls.crt": filepath.Join(root, "tls.crt"),
		"/openbao/arc-tls/tls.key": filepath.Join(root, "tls.key"),
	} {
		listenerConfig = strings.ReplaceAll(listenerConfig, old, next)
	}
	if err := os.WriteFile(filepath.Join(root, "listener.hcl"), []byte(listenerConfig), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "server", "-config="+filepath.Join(root, "base.hcl"), "-config="+filepath.Join(root, "listener.hcl"))
	var privateLog bytes.Buffer
	command.Stdout, command.Stderr = &privateLog, &privateLog
	if err := command.Start(); err != nil {
		t.Fatal("native server did not start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	endpoint := fmt.Sprintf("https://127.0.0.1:%d/v1/sys/health", tlsPort)
	client := func(servername string, roots *x509.CertPool, min, max uint16) *http.Client {
		return &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots, ServerName: servername, MinVersion: min, MaxVersion: max,
		}, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	readHealth := func(c *http.Client) (int64, error) {
		response, err := c.Get(endpoint)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		if response.StatusCode != 501 || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
			return 0, fmt.Errorf("unexpected native empty-store health response")
		}
		return response.TLS.PeerCertificates[0].SerialNumber.Int64(), nil
	}
	valid := client(hostname, pool, tls.VersionTLS12, tls.VersionTLS13)
	deadline := time.Now().Add(15 * time.Second)
	for {
		serial, err := readHealth(valid)
		if err == nil && serial == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native declared TLS listener did not become available")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for name, rejected := range map[string]*http.Client{
		"wrong hostname": client("wrong.invalid", pool, tls.VersionTLS12, tls.VersionTLS13),
		"untrusted CA":   client(hostname, x509.NewCertPool(), tls.VersionTLS12, tls.VersionTLS13),
		"obsolete TLS":   client(hostname, pool, tls.VersionTLS10, tls.VersionTLS11),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readHealth(rejected); err == nil {
				t.Fatal("native listener accepted invalid TLS")
			}
		})
	}
	writeLeaf(3)
	if err := command.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal("native certificate reload signal failed")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		serial, err := readHealth(valid)
		if err == nil && serial == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native listener did not reload its renewed certificate")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
