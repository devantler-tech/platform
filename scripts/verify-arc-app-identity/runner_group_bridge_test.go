package main

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerGroupCallbackOnlyAfterVerifiedIdentity(t *testing.T) {
	for _, check := range []struct {
		name, path, from, to string
	}{
		{name: "verified"},
		{"wrong-app", "/app", `"id":123`, `"id":999`},
		{"wrong-installation", "/orgs/devantler-tech/installation", `"id":456`, `"id":999`},
		{"wrong-organization", "/orgs/devantler-tech/installation", `"login":"devantler-tech"`, `"login":"other"`},
		{"suspended-installation", "/orgs/devantler-tech/installation", `"suspended_at":null`, `"suspended_at":"2026-10-06T00:00:00Z"`},
	} {
		t.Run(check.name, func(t *testing.T) {
			valid := check.path == ""
			f := newFixture(t)
			calls := 0
			if !valid {
				f.responses[check.path] = strings.Replace(f.responses[check.path], check.from, check.to, 1)
			}
			f.options.afterIdentity = func(_ context.Context, options verificationOptions, jwt string, id int64) outcome {
				calls++
				if id != 456 || options.expectedClientID != "Iv1.synthetic" {
					t.Fatal("callback received unverified identity")
				}
				verifyFixtureJWT(t, "Bearer "+jwt, fixtureKey())
				return "PASS_EXISTING"
			}
			got := verify(context.Background(), f.options)
			if valid && (calls != 1 || got != "PASS_EXISTING") {
				t.Fatalf("valid identity got %s, calls %d", got, calls)
			}
			if !valid && (calls != 0 || got != failIdentity) {
				t.Fatal("failed identity reached group callback")
			}
			f.mu.Lock()
			revoked := false
			for _, call := range f.requests {
				if strings.Contains(call, "revoke-self") {
					revoked = true
				}
			}
			f.mu.Unlock()
			if !revoked {
				t.Fatal("callback path skipped OpenBao token revocation")
			}
		})
	}
}

func TestRunnerGroupCallbackCannotOverrideOpenBaoCleanupFailure(t *testing.T) {
	for _, result := range []outcome{"PASS_EXISTING", "HOLD_CAPABILITY"} {
		t.Run(string(result), func(t *testing.T) {
			f := newFixture(t)
			f.statuses["/v1/auth/token/revoke-self"] = http.StatusForbidden
			calls := 0
			f.options.afterIdentity = func(context.Context, verificationOptions, string, int64) outcome {
				calls++
				return result
			}
			if got := verify(context.Background(), f.options); got != failCleanup || calls != 1 {
				t.Fatalf("callback result %s bypassed cleanup failure: %s, calls=%d", result, got, calls)
			}
		})
	}
}

func TestRunnerGroupRealCommandPreflight(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "verifier")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	root := configFixture(t)
	t.Setenv("GITHUB_ACTIONS", "false")
	for _, test := range []struct {
		arg, output string
		success     bool
	}{
		{"--preflight-runner-group", "ARC_RUNNER_GROUP_CAPABILITY=TRANSPORT_CONFIG_READY\n", true},
		{"--verify-runner-group", "ARC_RUNNER_GROUP_CAPABILITY=HOLD_INVOCATION\n", false},
		{"--preflight", "ARC_APP_IDENTITY=TRANSPORT_CONFIG_READY\n", true},
		{"--verify", "ARC_APP_IDENTITY=HOLD_INVOCATION\n", false},
	} {
		command := exec.Command(binary, test.arg)
		command.Dir = root
		output, err := command.CombinedOutput()
		if (err == nil) != test.success || string(output) != test.output {
			t.Fatalf("mode %s: %v %q", test.arg, err, output)
		}
	}
}
