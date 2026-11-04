// Command govulncheck-negative-control calls standard-library functions that
// have published advisories in the Go release its go.mod pins. The Go
// vulnerability scan must report them as reachable. It is a scan fixture only.
package main

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
)

func main() {
	if path, err := exec.LookPath("true"); err == nil {
		fmt.Println(path)
	}
	if parsed, err := url.Parse("https://example.invalid/"); err == nil {
		fmt.Println(parsed.Host)
	}
	if _, err := x509.ParseCertificate(nil); err != nil {
		fmt.Println("no certificate")
	}
	if response, err := http.Get("https://example.invalid/"); err == nil {
		_ = response.Body.Close()
	}
}
