package main

import (
	"bytes"
	"strings"
	"testing"
)

// provenanceFixture selects the signed certificate and statement from the native CLI-shaped synthetic response.
func provenanceFixture(t *testing.T) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	r := unmarshal(t, `{"entries":`+testProvenance+`}`)
	result := mchild(firstEntry(r, "entries"), "verificationResult")
	return r, mchild(mchild(result, "signature"), "certificate"), mchild(result, "statement")
}

// firstEntry selects a known synthetic attestation or subject.
func firstEntry(r map[string]any, key string) map[string]any {
	return r[key].([]any)[0].(map[string]any)
}

// TestStorageProtectionAcceptsNativeVerifierCertificateShape catches a decoder
// that invents an object SAN instead of consuming gh's Sigstore string field.
func TestStorageProtectionAcceptsNativeVerifierCertificateShape(t *testing.T) {
	a := storageFixture(t)
	out, err := executeStorage(t, a, testEnvironment())
	if err != nil || a.patches != 2 || !strings.Contains(out, `"result":"PROTECTED"`) {
		t.Fatalf("native CLI-shaped proof rejected: writes=%d error=%v receipt=%q", a.patches, err, out)
	}
}

// TestStorageProtectionRejectsNonNativeOrForeignCertificateIdentity ensures
// accepting the native string shape does not broaden the signed identity join.
func TestStorageProtectionRejectsNonNativeOrForeignCertificateIdentity(t *testing.T) {
	for _, value := range []any{nil, "", 42, true, []string{"https://example.test"},
		map[string]any{"type": "URI", "value": "https://github.com/devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/gh-readonly-queue/main/pr-12-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"https://github.com/other/platform/.github/workflows/ci.yaml@refs/heads/main"} {
		t.Run(string(mustJSON(t, value)), func(t *testing.T) {
			a := storageFixture(t)
			r, cert, _ := provenanceFixture(t)
			cert["subjectAlternativeName"] = value
			a.provenanceBody = mustJSON(t, r["entries"])
			if out, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 0 || out != "" {
				t.Fatal("unverified certificate identity authorized storage changes")
			}
		})
	}
}

// TestStorageProtectionRejectsLiveDigestUnrelatedToSuccessfulRun checks each signed identity join independently of untrusted predicate claims.
func TestStorageProtectionRejectsLiveDigestUnrelatedToSuccessfulRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"speculative source certificate with old successful main run", func(cert, statement map[string]any) {
			cert["sourceRepositoryDigest"] = "cccccccccccccccccccccccccccccccccccccccc"
		}},
		{"certificate belongs to another accepted-looking run", func(cert, statement map[string]any) {
			cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/999/attempts/1"
		}},
		{"different successful-run attempt", func(cert, statement map[string]any) {
			cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/456/attempts/2"
		}},
		{"wrong repository certificate", func(cert, statement map[string]any) {
			cert["sourceRepositoryURI"] = "https://github.com/other/platform"
		}},
		{"missing signed run identity", func(cert, statement map[string]any) { delete(cert, "runInvocationURI") }},
		{"forged predicate run cannot replace signed identity", func(cert, statement map[string]any) {
			cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/999/attempts/1"
			mchild(statement, "predicate")["run"] = "https://github.com/devantler-tech/platform/actions/runs/456/attempts/1"
		}},
		{"wrong certificate workflow", func(cert, statement map[string]any) {
			cert["subjectAlternativeName"] = "https://github.com/devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main"
		}},
		{"different workflow ref", func(cert, statement map[string]any) { cert["sourceRepositoryRef"] = "refs/heads/main" }},
		{"wrong certificate issuer", func(cert, statement map[string]any) { cert["issuer"] = "https://other.example" }},
		{"unrelated immutable subject digest", func(cert, statement map[string]any) {
			mchild(firstEntry(statement, "subject"), "digest")["sha256"] = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		}},
		{"wrong subject name", func(cert, statement map[string]any) {
			firstEntry(statement, "subject")["name"] = "ghcr.io/other/manifests"
		}},
		{"wrong predicate type", func(cert, statement map[string]any) { statement["predicateType"] = "https://example.test/other" }},
		{"empty subject census", func(cert, statement map[string]any) { statement["subject"] = []any{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			r, cert, statement := provenanceFixture(t)
			tc.mutate(cert, statement)
			a.provenanceBody = mustJSON(t, r["entries"])
			out, err := executeStorage(t, a, testEnvironment())
			if err == nil || a.patches != 0 || out != "" {
				t.Fatalf("unjoined registry provenance authorized writes=%d error=%v", a.patches, err)
			}
		})
	}
}

// TestStorageProtectionRejectsMissingOrUnverifiedRegistryOutput denies incomplete JSON and plausible output accompanying verifier failure.
func TestStorageProtectionRejectsMissingOrUnverifiedRegistryOutput(t *testing.T) {
	for _, body := range [][]byte{[]byte(""), []byte("[]"), []byte("null"), []byte("{}"), []byte("[malformed]"), bytes.Repeat([]byte(" "), responseLimit+1)} {
		t.Run(string(body[:min(len(body), 20)]), func(t *testing.T) {
			a := storageFixture(t)
			a.provenanceBody = body
			if out, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 0 || out != "" {
				t.Fatal("missing registry proof accepted")
			}
		})
	}
	t.Run("failed verification with plausible successful JSON", func(t *testing.T) {
		a := storageFixture(t)
		a.provenanceFailure = true
		if _, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 0 {
			t.Fatal("failed cryptographic verifier authorized mutation")
		}
	})
}

// TestStorageProtectionRequiresAcceptedRunAttemptAndRef rejects successful-looking runs without exact signed-joinable publication identities.
func TestStorageProtectionRequiresAcceptedRunAttemptAndRef(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing run attempt", func(r map[string]any) { delete(r, "run_attempt") }},
		{"zero run attempt", func(r map[string]any) { r["run_attempt"] = float64(0) }},
		{"invalid queue ref", func(r map[string]any) { r["head_branch"] = "feature" }},
		{"CI main ref is not an accepted merge group", func(r map[string]any) { r["head_branch"] = "main" }},
		{"missing branch", func(r map[string]any) { delete(r, "head_branch") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			tc.mutate(a.acceptedRun)
			if _, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 0 {
				t.Fatal("run identity is not signed-joinable")
			}
		})
	}
}

// TestStorageProtectionRechecksArtifactProvenanceBeforeEveryWrite requires fresh signed witnesses throughout the production entrypoint.
func TestStorageProtectionRechecksArtifactProvenanceBeforeEveryWrite(t *testing.T) {
	a := storageFixture(t)
	out, err := executeStorage(t, a, testEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if a.verifications < 12 || a.patches != 2 || !strings.Contains(out, `"result":"PROTECTED"`) {
		t.Fatalf("protection omitted fresh registry verification: proofs=%d writes=%d error=%v", a.verifications, a.patches, err)
	}
}

// TestStorageProtectionAcceptsSignedCDAndExactLaterAttempt characterizes both normal publication paths with exact attempt binding.
func TestStorageProtectionAcceptsSignedCDAndExactLaterAttempt(t *testing.T) {
	for _, cd := range []bool{false, true} {
		t.Run(map[bool]string{false: "CI second attempt", true: "CD main"}[cd], func(t *testing.T) {
			a := storageFixture(t)
			r, cert, _ := provenanceFixture(t)
			if cd {
				a.acceptedRun["path"] = ".github/workflows/cd.yaml"
				a.acceptedRun["event"] = "workflow_dispatch"
				a.acceptedRun["head_branch"] = "main"
				cert["sourceRepositoryRef"] = "refs/heads/main"
				cert["subjectAlternativeName"] = "https://github.com/devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main"
			} else {
				a.acceptedRun["run_attempt"] = float64(2)
				cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/456/attempts/2"
			}
			a.provenanceBody = mustJSON(t, r["entries"])
			if _, err := executeStorage(t, a, testEnvironment()); err != nil || a.patches != 2 {
				t.Fatalf("valid accepted publication refused: writes=%d error=%v", a.patches, err)
			}
		})
	}
}

// TestStorageProtectionNeedsExactWitnessAmongVerifiedBundles preserves an exact accepted witness alongside a valid historical bundle.
func TestStorageProtectionNeedsExactWitnessAmongVerifiedBundles(t *testing.T) {
	a := storageFixture(t)
	r, cert, _ := provenanceFixture(t)
	cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/999/attempts/1"
	good, _, _ := provenanceFixture(t)
	r["entries"] = append(r["entries"].([]any), firstEntry(good, "entries"))
	a.provenanceBody = mustJSON(t, r["entries"])
	if _, err := executeStorage(t, a, testEnvironment()); err != nil || a.patches != 2 {
		t.Fatalf("exact accepted witness lost beside a valid historical bundle: writes=%d error=%v", a.patches, err)
	}
}

// TestStorageProtectionStopsWhenSignedWitnessChangesAfterFirstWrite retains completed protection while refusing stale evidence for the next write.
func TestStorageProtectionStopsWhenSignedWitnessChangesAfterFirstWrite(t *testing.T) {
	a := storageFixture(t)
	r, cert, _ := provenanceFixture(t)
	cert["runInvocationURI"] = "https://github.com/devantler-tech/platform/actions/runs/999/attempts/1"
	bad := mustJSON(t, r["entries"])
	a.onRead = func(a *storageAPI, _ string) {
		if a.patches == 1 {
			a.provenanceBody = bad
		}
	}
	if out, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 1 || out != "" {
		t.Fatal("stale signed evidence allowed another write or emitted success")
	}
	if mchild(mitem(a.volumes, 0), "spec")["persistentVolumeReclaimPolicy"] != "Retain" || mchild(mitem(a.volumes, 1), "spec")["persistentVolumeReclaimPolicy"] != "Delete" {
		t.Fatal("late proof failure reversed protection or changed next volume")
	}
}
