package main

import (
	"strings"
	"testing"
)

// TestPublishMatcherProjectionRetainsThePublisherFamily proves that a manifest matcher
// accepting the canonical publisher keeps that family and its commit in the approval
// fingerprint, while the generated legacy set inside it may still rotate (#4502).
func TestPublishMatcherProjectionRetainsThePublisherFamily(t *testing.T) {
	const legacySubject = `^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@LEGACY$`
	const familySubject = `^https://github\.com/devantler-tech/(actions/\.github/workflows/publish-manifests\.yaml@LEGACY|\.github/\.github/workflows/publish-manifests\.yaml@CANONICAL)$`
	const manifest = `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: consumer
  namespace: consumer
spec:
  url: oci://ghcr.io/devantler-tech/github-config/manifests
  ref:
    semver: ">=1.0.0"
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: 'SUBJECT'
`
	entry := func(subject string) string {
		t.Helper()
		documents, err := decodeDocuments([]byte(strings.Replace(manifest, "SUBJECT", subject, 1)))
		if err != nil || len(documents) != 1 {
			t.Fatalf("decode OCIRepository: documents=%d error=%v", len(documents), err)
		}
		actual, err := authorizationSurfaceEntry(identityOf(documents[0]), documents[0])
		if err != nil {
			t.Fatal(err)
		}
		return actual
	}
	a, b, c := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	k, x := strings.Repeat("6", 40), strings.Repeat("7", 40)
	const pattern = "[0-9a-f]{40}"
	legacy := func(ref string) string { return strings.Replace(legacySubject, "LEGACY", ref, 1) }
	family := func(ref, commit string) string {
		return strings.Replace(strings.Replace(familySubject, "LEGACY", ref, 1), "CANONICAL", commit, 1)
	}

	legacyBaseline := entry(legacy(pattern))
	familyBaseline := entry(family(pattern, k))
	if familyBaseline == legacyBaseline {
		t.Fatal("accepting the canonical publisher did not move the approval fingerprint")
	}
	if entry(family("("+a+"|"+b+")", k)) == legacyBaseline {
		t.Fatal("a narrowed two-family matcher projected onto the legacy-only approval")
	}
	for _, ref := range []string{a, "(" + a + ")", "(" + a + "|" + b + ")", "(" + a + "|" + b + "|" + c + ")"} {
		if entry(family(ref, k)) != familyBaseline {
			t.Errorf("legacy signer set %q inside a two-family matcher moved the approval fingerprint", ref)
		}
	}

	narrowed := family("("+a+"|"+b+")", k)
	mutations := []struct{ name, subject string }{
		{"different canonical commit", family("("+a+"|"+b+")", x)},
		{"canonical pattern", family("("+a+"|"+b+")", pattern)},
		{"canonical set", family("("+a+"|"+b+")", "("+k+"|"+x+")")},
		{"canonical branch", family("("+a+"|"+b+")", "refs/heads/main")},
		{"second canonical commit", strings.Replace(narrowed, k+")$", k+`|\.github/\.github/workflows/publish-manifests\.yaml@`+x+")$", 1)},
		{"second legacy alternative", strings.Replace(narrowed, k+")$", k+`|actions/\.github/workflows/publish-manifests\.yaml@`+x+")$", 1)},
		{"bare extra alternative", strings.Replace(narrowed, k+")$", k+"|.*)$", 1)},
		{"different canonical repository", strings.Replace(narrowed, `|\.github/\.github/`, `|elsewhere/\.github/`, 1)},
		{"different canonical organization", strings.Replace(narrowed, `|\.github/\.github/`, `|.*/\.github/`, 1)},
		{"different canonical workflow", strings.Replace(narrowed, `publish-manifests\.yaml@`+k, `publish-app\.yaml@`+k, 1)},
		{"legacy regex injection", family("("+a+"|.*)", k)},
		{"too many legacy signers", family("("+a+"|"+b+"|"+c+"|"+x+")", k)},
		{"unanchored suffix", strings.TrimSuffix(narrowed, "$")},
		{"suffix after the group", strings.TrimSuffix(narrowed, "$") + ".*$"},
		{"unanchored prefix", strings.TrimPrefix(narrowed, "^")},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if mutation.subject == narrowed {
				t.Fatal("mutation did not change the fixture")
			}
			actual := entry(mutation.subject)
			if actual == familyBaseline || actual == legacyBaseline {
				t.Fatal("a change beyond the legacy signer set escaped the approval fingerprint")
			}
		})
	}

	t.Run("canonical-first order", func(t *testing.T) {
		reversed := `^https://github\.com/devantler-tech/(\.github/\.github/workflows/publish-manifests\.yaml@` + k +
			`|actions/\.github/workflows/publish-manifests\.yaml@` + a + `)$`
		if actual := entry(reversed); actual == familyBaseline || actual == legacyBaseline {
			t.Fatal("a canonical-first matcher escaped the approval fingerprint")
		}
	})

	// Application-image publishers stay legacy-only: a two-family application matcher is
	// never normalised, so even its legacy set stays in the fingerprint.
	t.Run("application publisher stays legacy-only", func(t *testing.T) {
		app := func(ref string) string {
			subject := strings.ReplaceAll(family(ref, k), "publish-manifests", "publish-app")
			documents, err := decodeDocuments([]byte(strings.Replace(
				strings.Replace(manifest, "github-config", "wedding-app", 1), "SUBJECT", subject, 1)))
			if err != nil || len(documents) != 1 {
				t.Fatalf("decode OCIRepository: documents=%d error=%v", len(documents), err)
			}
			actual, err := authorizationSurfaceEntry(identityOf(documents[0]), documents[0])
			if err != nil {
				t.Fatal(err)
			}
			return actual
		}
		if app(a) == app(b) {
			t.Fatal("a two-family application matcher was normalised")
		}
	})
}

// TestProjectPublishSubjectRefusesWhatItCannotPlace pins the refusals directly: a subject
// left unprojected still reaches the fingerprint whole, so only the return value shows
// that the projection declined it rather than normalising part of it.
func TestProjectPublishSubjectRefusesWhatItCannotPlace(t *testing.T) {
	a, k := strings.Repeat("1", 40), strings.Repeat("6", 40)
	const host = `^https://github\.com/devantler-tech/`
	legacy := `actions/\.github/workflows/publish-manifests\.yaml@`
	canonical := `|\.github/\.github/workflows/publish-manifests\.yaml@`
	if _, ok := projectPublishSubject(host+"("+legacy+a+canonical+k+")$", "publish-manifests"); !ok {
		t.Fatal("the rendered two-family subject was not recognised")
	}
	refused := map[string]string{
		"canonical pattern":       host + "(" + legacy + a + canonical + "[0-9a-f]{40})$",
		"canonical branch":        host + "(" + legacy + a + canonical + "refs/heads/main)$",
		"canonical short commit":  host + "(" + legacy + a + canonical + k[:39] + ")$",
		"second canonical commit": host + "(" + legacy + a + canonical + k + canonical + k + ")$",
		"legacy pattern":          host + "(" + legacy + "[0-9a-f]{40}" + canonical + k + ")$",
		"no group suffix":         host + "(" + legacy + a + canonical + k + "$",
	}
	for name, subject := range refused {
		if projected, ok := projectPublishSubject(subject, "publish-manifests"); ok {
			t.Errorf("%s was projected to %q", name, projected)
		}
	}
	appSubject := strings.ReplaceAll(host+"("+legacy+a+canonical+k+")$", "publish-manifests", "publish-app")
	if projected, ok := projectPublishSubject(appSubject, "publish-app"); ok {
		t.Errorf("a two-family application subject was projected to %q", projected)
	}
}
