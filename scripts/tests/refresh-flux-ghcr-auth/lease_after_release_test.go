package refreshfluxghcrauth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDelayedRenewalIsRefusedAfterRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_SYNC_LEASE_ORPHANED_HEARTBEAT_WRITE_AFTER_RELEASE": "true",
	})
	requireSuccessResult(t, result)
	if mustRead(filepath.Join(f.syncStateDir, "sync-lease-delayed-renewal-before-control")) != "accepted" {
		t.Fatal("the replay was not a valid renewal before release")
	}
	for _, mode := range []string{"actual", "holder-only", "version-only"} {
		path := filepath.Join(f.syncStateDir, "sync-lease-delayed-renewal-"+mode)
		if !pathExists(path) || mustRead(path) != "rejected" {
			t.Fatalf("the captured %s renewal was not refused after release", mode)
		}
	}
	if holder := mustRead(filepath.Join(f.syncStateDir, "sync-lease-holder")); holder != "" {
		t.Fatalf("delayed renewal left the released Lease held by %q", holder)
	}
}

func TestAfterReleaseRegressionDetectsEachAblatedProductionFence(t *testing.T) {
	t.Parallel()
	source := mustReadFile(helperPath)
	start := strings.Index(source, "renew_sync_lease() {\n")
	if start < 0 {
		t.Fatal("missing production renew helper")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("missing production renew helper end")
	}
	section := source[start : start+end+3]
	for _, test := range []struct{ name, line, acceptedMode string }{
		{"holder", "      {op: \"test\", path: \"/spec/holderIdentity\", value: $holder},\n", "holder-only"},
		{"resource version", "      {op: \"test\", path: \"/metadata/resourceVersion\", value: $resource_version},\n", "version-only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if strings.Count(section, test.line) != 1 {
				t.Fatal("mutation must replace exactly one production fence")
			}
			mutantSection := strings.Replace(section, test.line, "", 1)
			mutant := source[:start] + mutantSection + source[start+len(section):]
			if mutant == source {
				t.Fatal("mutation did not change production source")
			}
			f := newFixture(t)
			file, err := os.CreateTemp(filepath.Join(rootPath, "scripts"), ".refresh-lease-after-release-*")
			if err != nil {
				t.Fatal(err)
			}
			path := file.Name()
			t.Cleanup(func() { _ = os.Remove(path) })
			if _, err := file.WriteString(mutant); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
			result := f.runHelperExecutable(validConfig(), nil, map[string]string{
				"FAKE_SYNC_LEASE_ORPHANED_HEARTBEAT_WRITE_AFTER_RELEASE": "true",
			}, false, path)
			// The mutated helper completes normally. The rejection assertion,
			// rather than a syntax error or the fake's old mandatory-test policy,
			// must be what distinguishes it from the real helper.
			requireSuccessResult(t, result)
			if outcome := mustRead(filepath.Join(f.syncStateDir, "sync-lease-delayed-renewal-"+test.acceptedMode)); outcome != "accepted" {
				t.Fatalf("the applied %s mutation did not break its isolated fence: %s", test.name, outcome)
			}
		})
	}
}

func TestLeasePatchModelHonorsOnlySuppliedTestsAndIsAtomic(t *testing.T) {
	t.Parallel()
	state := map[string]any{"/spec/holderIdentity": "", "/spec/renewTime": "released"}
	write := jsonPatchOperation{Operation: "replace", Path: "/spec/renewTime", Value: "renewed"}
	updated, err := evaluateLeasePatch([]jsonPatchOperation{write}, state)
	if err != nil || updated[write.Path] != "renewed" {
		t.Fatalf("an unfenced write must apply: %v %v", updated, err)
	}
	if !reflect.DeepEqual(state, map[string]any{"/spec/holderIdentity": "", "/spec/renewTime": "released"}) {
		t.Fatal("evaluation modified its input snapshot")
	}
	_, err = evaluateLeasePatch([]jsonPatchOperation{write, {Operation: "test", Path: "/spec/holderIdentity", Value: "old-holder"}}, state)
	if err == nil || state[write.Path] != "released" {
		t.Fatal("a failed test must reject the whole patch without committing an earlier replace")
	}
}

func TestDeletedLeaseIsReportedDuringRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_SYNC_LEASE_DELETED_BEFORE_RELEASE": "true",
	})
	requireFailureResult(t, result)
	if !pathExists(filepath.Join(f.syncStateDir, "sync-lease-deleted-before-release")) {
		t.Fatal("the fixture did not delete the acquired Lease")
	}
	requireContains(t, result.stdout+result.stderr, "The GHCR synchronization lease no longer exists")
	if pathExists(filepath.Join(f.syncStateDir, "sync-lease-holder")) {
		t.Fatal("cleanup recreated the deleted Lease")
	}
}

func TestDeletedLeaseDiagnosticRegressionRejectsAnAppliedMutation(t *testing.T) {
	t.Parallel()
	source := mustReadFile(helperPath)
	branch := "    if [[ ! -s \"${lease_file}\" ]]; then\n" +
		"      echo \"::error::The GHCR synchronization lease no longer exists; this run cannot prove it held the lease while it was mutating.\"\n" +
		"      return 1\n" +
		"    fi\n"
	if strings.Count(source, branch) != 1 {
		t.Fatal("mutation must remove exactly one production deleted-Lease branch")
	}
	mutant := strings.Replace(source, branch, "", 1)
	file, err := os.CreateTemp(filepath.Join(rootPath, "scripts"), ".refresh-deleted-lease-*")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, err := file.WriteString(mutant); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	result := f.runHelperExecutable(validConfig(), nil, map[string]string{
		"FAKE_SYNC_LEASE_DELETED_BEFORE_RELEASE": "true",
	}, false, path)
	// The applied mutation reaches release and exhausts the invalid-state path.
	// It still fails overall; only the missing diagnostic distinguishes it, so
	// asserting a non-zero exit alone would not protect the deleted-Lease branch.
	requireFailureResult(t, result)
	if !pathExists(filepath.Join(f.syncStateDir, "sync-lease-deleted-before-release")) {
		t.Fatal("the mutated helper did not reach the acquired-Lease deletion")
	}
	output := result.stdout + result.stderr
	requireContains(t, output, "Could not clear the GHCR synchronization lease after")
	requireContains(t, output, "(invalid-lease-state)")
	requireNotContains(t, output, "The GHCR synchronization lease no longer exists")
}
