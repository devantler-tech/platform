package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStorageHoldReasonNeverPrintsTransportDetails rejects unrecognized or wrapped diagnostics.
func TestStorageHoldReasonNeverPrintsTransportDetails(t *testing.T) {
	for _, err := range []error{nil, errors.New("private-fixture-token"), errors.New("artifact provenance unverified: private-fixture-token"), fmt.Errorf("private-fixture-token: %w", errors.New("current main unverified"))} {
		if holdReason(err) != "UNKNOWN" {
			t.Fatal("private transport detail became a public diagnostic")
		}
	}
}

// TestStorageCheckDistinguishesDeploymentAndProvenanceHolds prevents an opaque accepted-deployment verdict.
func TestStorageCheckDistinguishesDeploymentAndProvenanceHolds(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*storageAPI)
	}{
		{"source", "CURRENT_SOURCE_UNVERIFIED", func(a *storageAPI) { a.staleSource = true }},
		{"run read", "DEPLOYMENT_RUN_UNAVAILABLE", func(a *storageAPI) { a.readFailure = "actions/runs/456" }},
		{"run identity", "DEPLOYMENT_RUN_UNVERIFIED", func(a *storageAPI) { a.acceptedRun["head_sha"] = "foreign" }},
		{"deployment", "DEPLOYMENT_READ_UNAVAILABLE", func(a *storageAPI) { a.missingDeployment = true }},
		{"provenance transport", "ARTIFACT_VERIFIER_UNAVAILABLE", func(a *storageAPI) { a.provenanceFailure = true }},
		{"provenance response", "ARTIFACT_RESPONSE_UNVERIFIED", func(a *storageAPI) { a.provenanceBody = []byte("private-fixture-token") }},
		{"provenance identity", "ARTIFACT_IDENTITY_UNVERIFIED", func(a *storageAPI) {
			a.provenanceBody = []byte(strings.ReplaceAll(testProvenance, testSHA, strings.Repeat("c", 40)))
		}},
		{"claim ownership", "STORAGE_CENSUS_UNVERIFIED", func(a *storageAPI) { mchild(mchild(mitem(a.volumes, 0), "spec"), "claimRef")["uid"] = "foreign" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			tc.change(a)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"] = "check"
			env["PORTAL_STORAGE_CONFIRM"] = "CHECK BACKSTAGE STORAGE ONLY"
			_, err := checkStorage(t, a, env)
			if err == nil || holdReason(err) != tc.want || a.patches != 0 {
				t.Fatalf("wrong guard category or a diagnostic write: reason=%s patches=%d", holdReason(err), a.patches)
			}
		})
	}
}

// TestStorageMainReportsBoundedInvocationHold exercises the actual public failure output.
func TestStorageMainReportsBoundedInvocationHold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--check")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOCACHE=" + os.Getenv("GOCACHE"), "GOMODCACHE=" + os.Getenv("GOMODCACHE"), "GOTOOLCHAIN=local", "PORTAL_STORAGE_CONFIRM=private-fixture-token"}
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "PORTAL_STORAGE_PROTECTION=HOLD\nPORTAL_STORAGE_REASON=INVOCATION_UNVERIFIED\n") || strings.Contains(string(b), "private-fixture-token") {
		t.Fatalf("main failure lacks a bounded non-sensitive diagnostic: error=%v output=%s", err, b)
	}
}
