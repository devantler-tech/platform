package main

import (
	"errors"
	"strings"
	"testing"
)

// The real signer identity currently on the artifact, as read from the
// transparency log when #3001 settled the question by hand. Kept verbatim so
// the positive case is a real observation rather than a construction that
// happens to satisfy whatever regex the manifest carries today.
const (
	realIssuer  = "https://token.actions.githubusercontent.com"
	realSubject = "https://github.com/devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/gh-readonly-queue/main/pr-3285-1a2b3c4d"
)

const (
	configManifest = `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity:
          - issuer: '^https://token\.actions\.githubusercontent\.com$'
            subject: '^https://github\.com/devantler-tech/platform/\.github/workflows/(ci\.yaml@refs/heads/gh-readonly-queue/main/.+|(cd|dr-rebuild)\.yaml@refs/heads/main)$'
`
	instanceManifest = `
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/verify
            value:
              provider: cosign
              matchOIDCIdentity:
                - issuer: '^https://token\.actions\.githubusercontent\.com$'
                  subject: '^https://github\.com/devantler-tech/platform/\.github/workflows/(ci\.yaml@refs/heads/gh-readonly-queue/main/.+|(cd|dr-rebuild)\.yaml@refs/heads/main)$'
`
)

func TestExtractMatcherReadsBothHalvesFromTheManifest(t *testing.T) {
	t.Parallel()

	matcher, err := extractMatcher([]byte(configManifest), []byte(instanceManifest))
	if err != nil {
		t.Fatalf("extractMatcher: %v", err)
	}

	if !strings.Contains(matcher.Subject, "dr-rebuild") {
		t.Fatalf("subject not read from the manifest: %q", matcher.Subject)
	}
	if !strings.Contains(matcher.Issuer, "token") {
		t.Fatalf("issuer not read from the manifest: %q", matcher.Issuer)
	}
}

// The check must refuse when the two halves disagree: the bootstrap path and
// the steady-state path would then enforce different things, and whichever is
// weaker is the one that actually decides.
func TestExtractMatcherRefusesDriftBetweenHalves(t *testing.T) {
	t.Parallel()

	drifted := strings.Replace(instanceManifest, "dr-rebuild", "dr-rebuilt", 1)

	_, err := extractMatcher([]byte(configManifest), []byte(drifted))
	if !errors.Is(err, errMatcherDrift) {
		t.Fatalf("want errMatcherDrift, got %v", err)
	}
}

func TestAcceptsTheRealSignerIdentity(t *testing.T) {
	t.Parallel()

	matcher, err := extractMatcher([]byte(configManifest), []byte(instanceManifest))
	if err != nil {
		t.Fatalf("extractMatcher: %v", err)
	}

	err = assertVerifies(matcher, []signerIdentity{{Issuer: realIssuer, Subject: realSubject}})
	if err != nil {
		t.Fatalf("configured matcher rejected the real signer: %v", err)
	}
}

// THE NEGATIVE CONTROL. Without this the check is indistinguishable from one
// that always passes — which is precisely the failure class #3007 closes. A
// deliberately wrong subject must be refused.
func TestNegativeControlWrongSubjectIsRefused(t *testing.T) {
	t.Parallel()

	matcher, err := extractMatcher([]byte(configManifest), []byte(instanceManifest))
	if err != nil {
		t.Fatalf("extractMatcher: %v", err)
	}

	wrong := strings.Replace(realSubject, "devantler-tech/platform", "evil-org/platform", 1)

	err = assertVerifies(matcher, []signerIdentity{{Issuer: realIssuer, Subject: wrong}})
	if !errors.Is(err, errNoAcceptedSigner) {
		t.Fatalf("want errNoAcceptedSigner for a wrong subject, got %v", err)
	}
}

// A matcher that is present but matches a DIFFERENT org's workflow is the
// general form of the incident: green, present, inert.
func TestNegativeControlWrongIssuerIsRefused(t *testing.T) {
	t.Parallel()

	matcher, err := extractMatcher([]byte(configManifest), []byte(instanceManifest))
	if err != nil {
		t.Fatalf("extractMatcher: %v", err)
	}

	err = assertVerifies(matcher, []signerIdentity{{Issuer: "https://accounts.google.com", Subject: realSubject}})
	if !errors.Is(err, errNoAcceptedSigner) {
		t.Fatalf("want errNoAcceptedSigner for a wrong issuer, got %v", err)
	}
}

// Observing NO signatures must FAIL, not pass. A check that treats "nothing
// found" as success is exactly the vacuous control this exists to prevent.
func TestNoObservedSignaturesFailsClosed(t *testing.T) {
	t.Parallel()

	matcher, err := extractMatcher([]byte(configManifest), []byte(instanceManifest))
	if err != nil {
		t.Fatalf("extractMatcher: %v", err)
	}

	err = assertVerifies(matcher, nil)
	if !errors.Is(err, errNoSignaturesObserved) {
		t.Fatalf("want errNoSignaturesObserved, got %v", err)
	}
}

// A subject regex that does not compile must fail loudly rather than be
// silently treated as matching nothing.
func TestUncompilableSubjectFailsLoudly(t *testing.T) {
	t.Parallel()

	err := assertVerifies(
		matcher{Issuer: `^https://token\.actions\.githubusercontent\.com$`, Subject: `^(unclosed`},
		[]signerIdentity{{Issuer: realIssuer, Subject: realSubject}},
	)
	if !errors.Is(err, errBadPattern) {
		t.Fatalf("want errBadPattern, got %v", err)
	}
}
