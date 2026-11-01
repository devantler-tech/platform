// Disposable Linux proof. No production inputs, network routes or credentials.
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type assertion string

func check(ok bool, reason string) {
	if !ok {
		panic(assertion(reason))
	}
}

func main() {
	defer func() {
		if failure := recover(); failure != nil {
			reason := "unexpected"
			if known, ok := failure.(assertion); ok {
				reason = string(known)
			}
			fmt.Println("NATIVE_ENVOY_PEER=FAIL reason=" + reason)
			os.Exit(1)
		}
	}()
	check(len(os.Args) == 3 || len(os.Args) == 4 && os.Args[3] == "--logical-peer-mutant", "arguments")
	mutant := len(os.Args) == 4
	interfaces, interfaceErr := net.Interfaces()
	check(interfaceErr == nil, "fixture-network-inventory")
	for _, network := range interfaces {
		check(network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0, "fixture-network-not-isolated")
	}
	version, err := exec.Command(os.Args[1], "--version").CombinedOutput()
	check(err == nil && bytes.Contains(version, []byte("/1.39.5/")), "proxy-version")
	work := "/tmp/email-proxy-native"
	check(os.Mkdir(work, 0700) == nil, "workspace")
	defer os.RemoveAll(work)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err == nil, "certificate-key")
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.example.test"}, DNSNames: []string{"mail.example.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	check(err == nil, "certificate")
	check(os.WriteFile(filepath.Join(work, "certificate.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600) == nil, "certificate-file")
	private, err := x509.MarshalECPrivateKey(key)
	check(err == nil, "private-key")
	check(os.WriteFile(filepath.Join(work, "private-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600) == nil, "private-key-file")
	parsed, err := x509.ParseCertificate(der)
	check(err == nil, "certificate-parse")
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	original, err := os.ReadFile(os.Args[2])
	check(err == nil, "admitted-config")
	var config map[string]any
	check(json.Unmarshal(original, &config) == nil, "config-json")
	listener := config["staticResources"].(map[string]any)["listeners"].([]any)[0].(map[string]any)
	chains := listener["filterChains"].([]any)
	for _, raw := range chains {
		chain := raw.(map[string]any)
		match := chain["filterChainMatch"].(map[string]any)
		if match["transportProtocol"] == "tls" {
			peers, ok := match["directSourcePrefixRanges"].([]any)
			check(ok && len(peers) == 1, "admitted-physical-peer")
			peer := peers[0].(map[string]any)
			check(peer["addressPrefix"] == "127.0.0.1" && peer["prefixLen"] == float64(32), "admitted-peer-value")
			if mutant {
				match["sourcePrefixRanges"] = peers
				delete(match, "directSourcePrefixRanges")
			}
		}
	}
	encode := func() []byte { raw, err := json.Marshal(config); check(err == nil, "encode-config"); return raw }
	fenced := encode()
	for _, raw := range chains {
		match := raw.(map[string]any)["filterChainMatch"].(map[string]any)
		delete(match, "directSourcePrefixRanges")
		delete(match, "sourcePrefixRanges")
	}
	unfenced := encode()
	start := func(raw []byte) func() {
		path := filepath.Join(work, "config.json")
		check(os.WriteFile(path, raw, 0600) == nil, "runtime-config")
		child := exec.Command(os.Args[1], "-c", path, "--concurrency", "1", "--disable-hot-restart", "--drain-time-s", "0", "--parent-shutdown-time-s", "0", "--log-level", "error")
		child.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work}
		var diagnostics bytes.Buffer
		child.Stdout, child.Stderr = &diagnostics, &diagnostics
		check(child.Start() == nil, "proxy-start")
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		stop := func() {
			_ = child.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = child.Process.Kill()
				<-done
				panic(assertion("proxy-shutdown"))
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp4", "127.0.0.1:18443", 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				break
			}
			select {
			case <-done:
				fmt.Print(diagnostics.String())
				panic(assertion("proxy-config"))
			default:
			}
			if time.Now().After(deadline) {
				stop()
				panic(assertion("proxy-readiness"))
			}
			time.Sleep(20 * time.Millisecond)
		}
		return stop
	}
	proxyHeader := func(version int, claim string) []byte {
		if version == 1 {
			return []byte("PROXY TCP4 " + claim + " 127.0.0.1 40000 18443\r\n")
		}
		from := net.ParseIP(claim)
		check(from != nil, "fixture-address")
		header := append([]byte("\r\n\r\n\x00\r\nQUIT\n"), 0x21)
		var data []byte
		if ip := from.To4(); ip != nil {
			header = append(header, 0x11, 0, 12)
			data = append(append([]byte{}, ip...), net.ParseIP("127.0.0.1").To4()...)
		} else {
			header = append(header, 0x21, 0, 36)
			data = append(append([]byte{}, from.To16()...), net.ParseIP("::1").To16()...)
		}
		ports := make([]byte, 4)
		binary.BigEndian.PutUint16(ports, 40000)
		binary.BigEndian.PutUint16(ports[2:], 18443)
		return append(append(header, data...), ports...)
	}
	attempt := func(peer, claim, serverName string, version int) (bool, int, string) {
		dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(peer)}}
		conn, err := dialer.Dial("tcp4", "127.0.0.1:18443")
		check(err == nil, "physical-connect")
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if version != 0 {
			_, err = conn.Write(proxyHeader(version, claim))
			if err != nil {
				return false, 0, ""
			}
		}
		secure := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12})
		if err := secure.Handshake(); err != nil {
			if timed, ok := err.(net.Error); ok {
				check(!timed.Timeout(), "fixture-negative-timeout")
			}
			fmt.Printf("FIXTURE_TLS_REFUSED peer=%s version=%d reason=%s\n", peer, version, err)
			return false, 0, ""
		}
		_, err = io.WriteString(secure, "GET /fixture HTTP/1.1\r\nHost: mail.example.test\r\nX-Forwarded-For: 203.0.113.99\r\nConnection: close\r\n\r\n")
		if err != nil {
			return true, 0, ""
		}
		response, err := http.ReadResponse(bufio.NewReader(secure), nil)
		if err != nil {
			return true, 0, ""
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1000))
		check(err == nil && string(body) == "native-peer-proof", "proxy-response")
		return true, response.StatusCode, response.Header.Get("x-observed-client")
	}
	phase := func(raw []byte, verify func()) { stop := start(raw); defer stop(); verify() }
	phase(unfenced, func() {
		ok, status, seen := attempt("127.0.0.2", "198.51.100.11", "mail.example.test", 2)
		if !ok || status != 200 || seen != "198.51.100.11" {
			fmt.Printf("FIXTURE_BASELINE accepted=%t status=%d seen=%s\n", ok, status, seen)
		}
		check(ok && status == 200 && seen == "198.51.100.11", "initial-positive-control")
	})
	phase(fenced, func() {
		claim := "198.51.100.10"
		if mutant {
			claim = "127.0.0.1"
		}
		for _, tc := range []struct {
			name, claim string
			version     int
		}{{"mail.example.test", claim, 2}, {"127.0.0.1", claim, 1}} {
			ok, status, seen := attempt("127.0.0.1", tc.claim, tc.name, tc.version)
			if !ok || status != 200 || seen != tc.claim {
				fmt.Printf("FIXTURE_ALLOWED accepted=%t status=%d seen=%s\n", ok, status, seen)
			}
			check(ok && status == 200 && seen == tc.claim, "allowed-physical-peer")
		}
		if !mutant {
			ok, status, seen := attempt("127.0.0.1", "2001:db8::10", "mail.example.test", 2)
			check(ok && status == 200 && seen == "2001:db8::10", "allowed-ipv6-client")
		}
		for _, version := range []int{1, 2} {
			for _, name := range []string{"mail.example.test", "127.0.0.1"} {
				ok, _, _ := attempt("127.0.0.2", "127.0.0.1", name, version)
				check(!ok, "forged-physical-peer-accepted")
			}
		}
		ok, _, _ := attempt("127.0.0.1", "", "mail.example.test", 0)
		check(!ok, "missing-provider-header-accepted")
		ok, status, seen := attempt("127.0.0.1", claim, "mail.example.test", 2)
		check(ok && status == 200 && seen == claim, "allowed-peer-after-refusals")
	})
	phase(unfenced, func() {
		for _, name := range []string{"mail.example.test", "127.0.0.1"} {
			ok, status, seen := attempt("127.0.0.2", "198.51.100.11", name, 2)
			check(ok && status == 200 && seen == "198.51.100.11", "denied-peer-positive-control")
		}
	})
	phase(fenced, func() {
		ok, status, seen := attempt("127.0.0.1", "198.51.100.10", "mail.example.test", 2)
		check(ok && status == 200 && seen == "198.51.100.10", "restored-allowed-peer")
		ok, _, _ = attempt("127.0.0.2", "127.0.0.1", "mail.example.test", 2)
		check(!ok, "fence-restoration")
		ok, status, seen = attempt("127.0.0.1", "198.51.100.10", "mail.example.test", 2)
		check(ok && status == 200 && seen == "198.51.100.10", "restored-peer-after-refusal")
	})
	fmt.Println("NATIVE_ENVOY_PEER=PASS image=1.39.5 admitted=TRUE allowed=PASS forged_v1_v2=REFUSED sni_fallback=FENCED ipv6_client=PASS header_required=TRUE unfenced_control=PASS restored=PASS")
}
