// Command check-dex-binary admits a scanned executable only after testing its
// configured HTTPS and gRPC TLS floors. All listeners and credentials are local
// disposable fixtures. Raw scanner output is never printed.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/buildinfo"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
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
	"time"
)

const upstreamRevision = "75eb19238e10fc4750e7efb05a4031b254b04029"

func main() {
	binary := flag.String("binary", "", "exact native executable to scan and exercise")
	scanner := flag.String("scanner", "", "pinned govulncheck executable")
	advisory := flag.String("advisory", "", "fresh primary GO-2024-2476 OSV document")
	web := flag.String("web", "", "pinned upstream web directory")
	flag.Parse()
	if err := run(*binary, *scanner, *advisory, *web); err != nil {
		fmt.Fprintln(os.Stderr, "Dex admission refused:", err)
		os.Exit(1)
	}
	fmt.Println("Dex executable, scanner coverage, primary advisory and native TLS controls passed.")
}

// run binds the executable and scanner identities before exercising the real
// service. An undisposed finding or changed executable refuses admission.
func run(binary, scanner, advisory, web string) error {
	if binary == "" || scanner == "" || advisory == "" || web == "" || flag.NArg() != 0 {
		return errors.New("binary, scanner, advisory and web are required")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return fmt.Errorf("read native build information: %w", err)
	}
	if info.GoVersion != "go1.27.3" || info.Path != "github.com/dexidp/dex/cmd/dex" {
		return errors.New("unexpected executable or compiler")
	}
	revision, modified, netVersion := "", "", ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	for _, dep := range info.Deps {
		if dep.Path == "golang.org/x/net" {
			if dep.Replace != nil {
				return errors.New("unexpected networking module replacement")
			}
			netVersion = dep.Version
		}
	}
	if revision != upstreamRevision || modified != "true" || netVersion != "v0.60.0" {
		return errors.New("executable does not bind the reviewed patched release")
	}
	scanInfo, err := buildinfo.ReadFile(scanner)
	if err != nil {
		return err
	}
	if scanInfo.Path != "golang.org/x/vuln/cmd/govulncheck" || scanInfo.Main.Version != "v1.8.0" || scanInfo.GoVersion != "go1.27.3" {
		return errors.New("unexpected scanner executable")
	}
	before, err := binaryDigest(binary)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "dex-scan-*.json")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, scanner, "-mode", "binary", "-format", "json", binary)
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return errors.New("native scanner failed")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	needsDisposition, err := judgeReport(f, info.GoVersion)
	if err != nil {
		return err
	}
	if needsDisposition {
		body, err := os.ReadFile(advisory)
		if err != nil {
			return err
		}
		if err := checkHistoricalAdvisory(body); err != nil {
			return err
		}
	}
	if err := nativeTLS(binary, web); err != nil {
		return err
	}
	after, err := binaryDigest(binary)
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("executable changed during admission")
	}
	return nil
}

// binaryDigest binds the file scanned to the file exercised by the fixture.
func binaryDigest(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// judgeReport requires actual module/compiler coverage and rejects new reachable
// findings. A true result requires the separate historical TLS disposition.
func judgeReport(r io.Reader, compiler string) (bool, error) {
	decoder := json.NewDecoder(r)
	coverage, disposition := false, false
	for {
		var item struct {
			SBOM *struct {
				GoVersion string `json:"go_version"`
				Modules   []struct {
					Path string `json:"path"`
				} `json:"modules"`
			} `json:"SBOM"`
			Finding *struct {
				OSV   string                              `json:"osv"`
				Trace []struct{ Module, Function string } `json:"trace"`
			} `json:"finding"`
		}
		if err := decoder.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return false, errors.New("invalid or incomplete scanner stream")
		}
		if item.SBOM != nil {
			if coverage || item.SBOM.GoVersion != compiler || compiler == "" {
				return false, errors.New("ambiguous or mismatched scanner coverage")
			}
			for _, module := range item.SBOM.Modules {
				if module.Path == "github.com/dexidp/dex" {
					coverage = true
				}
			}
			if !coverage {
				return false, errors.New("scanner did not examine Dex")
			}
		}
		if item.Finding != nil {
			f := item.Finding
			if f.OSV == "" || len(f.Trace) == 0 {
				return false, errors.New("incomplete finding")
			}
			if f.Trace[0].Function == "" {
				continue
			} // Module-only observations remain in the original report.
			if f.OSV != "GO-2024-2476" || (f.Trace[0].Module != "github.com/dexidp/dex" && f.Trace[0].Module != "./api/v2") {
				return false, errors.New("executable has an undisposed reachable advisory")
			}
			disposition = true
		}
	}
	if !coverage {
		return false, errors.New("scanner coverage is absent")
	}
	return disposition, nil
}

// checkHistoricalAdvisory refuses a changed primary range instead of silently
// carrying a past disposition forward to different advisory metadata.
func checkHistoricalAdvisory(body []byte) error {
	var doc struct {
		ID       string `json:"id"`
		Affected []struct {
			Package struct{ Name, Ecosystem string } `json:"package"`
			Ranges  []struct {
				Type   string                               `json:"type"`
				Events []struct{ Introduced, Fixed string } `json:"events"`
			} `json:"ranges"`
			Specific struct {
				Ranges []struct {
					Type   string                               `json:"type"`
					Events []struct{ Introduced, Fixed string } `json:"events"`
				} `json:"custom_ranges"`
			} `json:"ecosystem_specific"`
		} `json:"affected"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return errors.New("unreadable primary advisory")
	}
	if doc.ID == "GO-2024-2476" && len(doc.Affected) == 1 {
		a := doc.Affected[0]
		if a.Package.Name == "github.com/dexidp/dex" && a.Package.Ecosystem == "Go" && len(a.Specific.Ranges) == 1 && len(a.Ranges) == 1 && a.Ranges[0].Type == "SEMVER" && len(a.Ranges[0].Events) == 1 && a.Ranges[0].Events[0].Introduced == "0" && a.Ranges[0].Events[0].Fixed == "" {
			r := a.Specific.Ranges[0]
			if r.Type == "ECOSYSTEM" && len(r.Events) == 2 && r.Events[0].Introduced == "2.37.0" && r.Events[1].Fixed == "2.38.0" {
				return nil
			}
		}
	}
	return errors.New("historical advisory changed; review the disposition again")
}

func nativeTLS(binary, web string) error {
	return nativeTLSFloor(binary, web, "1.3")
}

// nativeTLSFloor runs disposable SQLite-backed HTTPS/gRPC services. Only the
// negative control supplies a weaker floor; admission always supplies TLS 1.3.
func nativeTLSFloor(binary, web, floor string) (resultErr error) {
	fixture, err := os.MkdirTemp("", "dex-tls-")
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && resultErr.Error() != "Dex discarded its configured TLS floor" {
			// This process only receives the fictional configuration below. Keep
			// bounded failure diagnostics useful without retaining its private key.
			if body, err := os.ReadFile(filepath.Join(fixture, "dex.log")); err == nil {
				if len(body) > 4096 {
					body = body[len(body)-4096:]
				}
				fmt.Fprintf(os.Stderr, "Fictional native fixture diagnostics: %q\n", body)
			}
		}
		_ = os.RemoveAll(fixture)
	}()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local Dex fixture"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(10 * time.Minute),
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Dex watches its certificate directory. Keep transient SQLite journals
	// outside that watch, matching separate certificate and data projections.
	tlsDir, dbDir := filepath.Join(fixture, "tls"), filepath.Join(fixture, "database")
	for _, directory := range []string{tlsDir, dbDir} {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
	}
	certPath, keyPath := filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return err
	}
	listeners, err := reserveEndpoints(2)
	if err != nil {
		return err
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	addresses := make([]string, len(listeners))
	for i, listener := range listeners {
		addresses[i] = listener.Addr().String()
	}
	issuer := "https://" + addresses[0] + "/dex"
	config := fmt.Sprintf(`issuer: %s
storage:
  type: sqlite3
  config:
    file: %s
enablePasswordDB: true
web:
  https: %s
  tlsCert: %s
  tlsKey: %s
  tlsMinVersion: %q
grpc:
  addr: %s
  tlsCert: %s
  tlsKey: %s
  tlsMinVersion: %q
frontend:
  dir: %s
`, issuer, filepath.Join(dbDir, "dex.db"), addresses[0], certPath, keyPath, floor, addresses[1], certPath, keyPath, floor, web)
	configPath := filepath.Join(fixture, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(fixture, "dex.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "serve", configPath)
	cmd.Stdout, cmd.Stderr = log, log
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}
	}()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return errors.New("fixture trust failed")
	}
	good := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	transport := &http.Transport{TLSClientConfig: good}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := client.Get(issuer + "/.well-known/openid-configuration")
		if err != nil {
			continue
		}
		var document struct {
			Issuer string `json:"issuer"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&document)
		resp.Body.Close()
		if err == nil && resp.StatusCode == 200 && document.Issuer == issuer {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("real Dex discovery did not become ready with verified TLS")
	}
	for _, address := range addresses {
		if err := exerciseTLS(address, good); err != nil {
			return err
		}
	}
	// Renew only the certificate, retaining the private key so every old/new
	// read remains a matching keypair. A different issuer name and serial make
	// the old and renewed trust pools distinct despite using the same key.
	template.SerialNumber = big.NewInt(2)
	template.Subject.CommonName = "local Dex renewed fixture"
	renewedDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	renewedPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: renewedDER})
	renewedPath := filepath.Join(fixture, "renewed.pem")
	if err := os.WriteFile(renewedPath, renewedPEM, 0600); err != nil {
		return err
	}
	if err := os.Rename(renewedPath, certPath); err != nil {
		return err
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		return err
	}
	renewedPool := x509.NewCertPool()
	if !renewedPool.AppendCertsFromPEM(renewedPEM) {
		return errors.New("renewed fixture trust failed")
	}
	renewedConfig := good.Clone()
	renewedConfig.RootCAs = renewedPool
	observed := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		count := 0
		for _, address := range addresses {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, renewedConfig.Clone())
			if err != nil {
				continue
			}
			peers := conn.ConnectionState().PeerCertificates
			if len(peers) > 0 && peers[0].SerialNumber.Cmp(big.NewInt(2)) == 0 {
				count++
			}
			_ = conn.Close()
		}
		if count == len(addresses) {
			observed = true
			break
		}
	}
	if !observed {
		return errors.New("renewed certificate was not observed on both endpoints")
	}
	for _, address := range addresses {
		if err := exerciseTLS(address, renewedConfig); err != nil {
			return err
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, good.Clone())
		if err == nil {
			_ = conn.Close()
			return errors.New("retired fixture trust still accepted the renewed certificate")
		}
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			return errors.New("retired fixture trust failed for an unexpected reason")
		}
	}
	return nil
}

// reserveEndpoints keeps every selected port bound until the caller hands all
// endpoints to the native process, preventing self-reuse during selection.
func reserveEndpoints(count int) ([]net.Listener, error) {
	var listeners []net.Listener
	for i := 0; i < count; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, reserved := range listeners {
				_ = reserved.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// exerciseTLS brackets protocol/trust refusals with successful connections, so
// an unavailable service cannot satisfy a negative control.
func exerciseTLS(address string, good *tls.Config) error {
	dial := func(config *tls.Config) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, config)
		if err == nil {
			return conn.Close()
		}
		return err
	}
	if err := dial(good.Clone()); err != nil {
		return errors.New("positive TLS 1.3 handshake failed")
	}
	low := good.Clone()
	low.MinVersion, low.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if err := dial(low); err == nil || !strings.Contains(err.Error(), "protocol version not supported") {
		return errors.New("Dex discarded its configured TLS floor")
	}
	wrongCA := good.Clone()
	wrongCA.RootCAs = x509.NewCertPool()
	var unknown x509.UnknownAuthorityError
	if err := dial(wrongCA); !errors.As(err, &unknown) {
		return errors.New("wrong CA did not fail certificate validation")
	}
	wrongSAN := good.Clone()
	wrongSAN.ServerName = "wrong.invalid"
	var hostname x509.HostnameError
	if err := dial(wrongSAN); !errors.As(err, &hostname) {
		return errors.New("wrong hostname did not fail certificate validation")
	}
	if err := dial(good.Clone()); err != nil {
		return errors.New("positive handshake after negative controls failed")
	}
	return nil
}
