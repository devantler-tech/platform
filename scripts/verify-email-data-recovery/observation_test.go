package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type observationAPI struct{ calls int }

func (a *observationAPI) call(_ context.Context, _ []byte, _ ...string) ([]byte, error) {
	a.calls++
	return nil, errors.New("private-observation-canary")
}

func observationEnv() map[string]string {
	e := confirmedInvocation()
	e["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/observe-email-data-recovery.yaml@refs/heads/main"
	e["EMAIL_DATA_RECOVERY_CONFIRM"] = "observe-email-data-recovery"
	return e
}

// A confirmed observer must reach the fixed read, and every changed dispatch
// field must stop before access. A refusal must never print a private API error.
func TestObservationDispatchBindsFixedReaderAndHidesAPIErrors(t *testing.T) {
	e := observationEnv()
	api := &observationAPI{}
	var out bytes.Buffer
	if runRecoveryObservation(context.Background(), func(k string) string { return e[k] }, api, &out) != 2 || api.calls != 1 || out.String() != "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=reader-identity\n" {
		t.Fatalf("confirmed observation did not reach its fixed read: %q calls=%d", out.String(), api.calls)
	}
	for key := range e {
		t.Run(key, func(t *testing.T) {
			e := observationEnv()
			e[key] = "foreign"
			api := &observationAPI{}
			var out bytes.Buffer
			if runRecoveryObservation(context.Background(), func(k string) string { return e[k] }, api, &out) != 2 || api.calls != 0 || out.String() != "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=dispatch\n" {
				t.Fatal("foreign context reached access or exposed details")
			}
		})
	}
}

// The observer's Kubernetes capability cannot forward a write, an unbounded
// inventory, a serving workload read or a credential read from another scope.
func TestObservationClientHasOnlyThreeFixedReads(t *testing.T) {
	for _, args := range [][]string{{"delete", "Namespace", "email-data-recovery-41561215415"}, {"get", "Secret", "storage-key", "--ignore-not-found=true", "-o=json", "--namespace=email-data-recovery-41561215415"}, {"get", "Pod", "private-mail-0", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"}, {"get", "Pod", "reader", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"}, {"get", "Pod", "reader", "--ignore-not-found=true", "-o=json", "--namespace=email-data-recovery-41561215415", "--raw=/api/v1/secrets"}, {"get", "pods", "--all-namespaces"}} {
		api := &observationAPI{}
		k := observationReadOnly{base: api}
		if _, err := k.call(context.Background(), nil, args...); err == nil || api.calls != 0 {
			t.Fatal("observer forwarded a foreign read or mutation")
		}
	}
	for _, args := range [][]string{{"get", "Namespace", "email-data-recovery-41561215415", "--ignore-not-found=true", "-o=json"}, {"get", "Pod", "reader", "--ignore-not-found=true", "-o=json", "--namespace=email-data-recovery-41561215415"}, {"get", "Secret", "private-mail-maintainer", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"}} {
		api := &observationAPI{}
		k := observationReadOnly{base: api}
		_, _ = k.call(context.Background(), nil, args...)
		if api.calls != 1 {
			t.Fatal("fixed diagnostic read was refused")
		}
		_, _ = k.call(context.Background(), []byte("private-canary"), args...)
		if api.calls != 1 {
			t.Fatal("observer forwarded input bytes")
		}
	}
}

// A real local HTTP server distinguishes readiness from successful transport.
// This probe must send no credential and never follow a server-selected URL.
func TestObservationHealthReportsStatusWithoutAuthenticationOrRedirects(t *testing.T) {
	for _, status := range []int{200, 401, 403, 503, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			targetReads := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetReads++ }))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/healthz/ready" || r.Header.Get("Authorization") != "" {
					t.Error("probe sent an unexpected request or credential")
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private-health-canary"))
			}))
			defer source.Close()
			c, e := newMailboxClient(source.URL, "private-unused-credential")
			if e != nil {
				t.Fatal("local fixture refused")
			}
			got, e := observationHealth(context.Background(), c)
			if e != nil || got != status || targetReads != 0 {
				t.Fatal("health status was lost or redirect followed")
			}
		})
	}
}

// The actual command must distinguish a refused read-only observation from a
// refused recovery write, before any cluster client or credential is used.
func TestObservationCommandRefusesForeignDispatchBeforeAccess(t *testing.T) {
	if os.Getenv("EMAIL_OBSERVATION_CHILD") == "1" {
		os.Args = []string{"verify-email-data-recovery", "--observe-retained"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestObservationCommandRefusesForeignDispatchBeforeAccess$")
	cmd.Env = []string{"EMAIL_OBSERVATION_CHILD=1", "PATH=/nonexistent"}
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || string(output) != "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=dispatch\n" {
		t.Fatalf("observation did not fail closed at dispatch: %q", output)
	}
	if strings.Contains(string(output), "credential") {
		t.Fatal("private error escaped")
	}
}
