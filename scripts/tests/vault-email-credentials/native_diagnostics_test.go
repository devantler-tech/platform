package emailcredentials_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// nativeStartFailure emits only fixed classes; raw Docker diagnostics stay out
// of logs even when credential helpers or provider messages include private data.
func nativeStartFailure(err error) string {
	if err == nil {
		return "invalid-container-id"
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "docker-not-found"
	}
	var failure *exec.ExitError
	if !errors.As(err, &failure) {
		return "process-failed"
	}
	message := strings.ToLower(string(failure.Stderr))
	if strings.Contains(message, "no space left on device") {
		return "disk-capacity"
	}
	if strings.Contains(message, "cannot connect to the docker daemon") || strings.Contains(message, "is the docker daemon running") {
		return "daemon-unavailable"
	}
	if strings.Contains(message, "oci runtime create failed") || strings.Contains(message, "runc create failed") {
		return "container-runtime"
	}
	if strings.Contains(message, "quay.io") {
		for _, marker := range []string{"toomanyrequests", "too many requests", "429"} {
			if strings.Contains(message, marker) {
				return "registry-throttled"
			}
		}
		for _, marker := range []string{"unauthorized", "denied"} {
			if strings.Contains(message, marker) {
				return "registry-denied"
			}
		}
		for _, marker := range []string{"502", "503", "504", "service unavailable", "bad gateway", "i/o timeout", "tls handshake timeout", "connection reset", "no such host"} {
			if strings.Contains(message, marker) {
				return "registry-unavailable"
			}
		}
	}
	return "process-failed"
}

// Native stderr can contain credential-helper details. Diagnostics must reveal
// an actionable class without copying any provider-controlled bytes to the log.
func TestNativeContainerFailureClassifiesWithoutDisclosingStderr(t *testing.T) {
	const private = "private-fixture-material-must-never-be-logged"
	for _, test := range []struct{ name, stderr, want string }{
		{"registry-throttle", "quay.io: toomanyrequests: " + private, "registry-throttled"},
		{"registry-outage", "quay.io: 503 Service Unavailable " + private, "registry-unavailable"},
		{"registry-network", "Get https://quay.io/v2/: TLS handshake timeout " + private, "registry-unavailable"},
		{"registry-access", "quay.io: unauthorized: " + private, "registry-denied"},
		{"daemon", "Cannot connect to the Docker daemon: " + private, "daemon-unavailable"},
		{"capacity", "no space left on device: " + private, "disk-capacity"},
		{"runtime", "OCI runtime create failed: " + private, "container-runtime"},
		{"unknown", private, "process-failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := nativeStartFailure(&exec.ExitError{Stderr: []byte(test.stderr)})
			if got != test.want {
				t.Fatalf("expected safe class %s, got %s", test.want, got)
			}
		})
	}
	if got := nativeStartFailure(exec.ErrNotFound); got != "docker-not-found" {
		t.Fatalf("missing Docker was not distinguished: %s", got)
	}
	if got := nativeStartFailure(errors.New(private)); got != "process-failed" {
		t.Fatalf("unknown process error was not safely classified: %s", got)
	}
	if got := nativeStartFailure(nil); got != "invalid-container-id" {
		t.Fatalf("invalid container output was not distinguished: %s", got)
	}
}
