// Command validate-matcher-efficacy checks that the cosign matcher guarding the
// root Flux source actually VERIFIES the artifact it guards, rather than merely
// being well-formed.
//
// # WHY THE EXISTING CHECKS ARE NOT ENOUGH
//
// scripts/validate-flux-verify already proves the block resolves at the path
// KSail reads, that it carries exactly one matcher, and that both halves agree.
// Every one of those passed on a matcher that verified ZERO artifacts and
// halted all GitOps delivery on prod for roughly 5.5 hours (#3005). The config
// was schema-valid, docs-valid, correctly targeted, enabled, and constrained a
// signer. It was also inert, and the only place that was visible was
// status.conditions on the live cluster — after the deploy.
//
// A matcher goes inert without changing shape: a renamed workflow, a changed
// trigger ref, a moved org, or a publisher relocated behind a reusable workflow
// (which moves the OIDC subject to the CALLED workflow's path). Each of those
// leaves a control that is present, green, and verifies nothing.
//
// So this asks the one question the static checks cannot: does the matcher, as
// written in the manifest, accept a signer that actually signed this artifact?
//
// 🔴 THE PATTERNS ARE READ FROM THE MANIFEST, NEVER RESTATED HERE.
//
// A constant copy of the issuer or subject would test the copy, not the
// deployment — and would keep passing after the manifest drifted away from it,
// which is the failure mode rather than a defence against it. Both halves are
// read and required to agree, so neither the bootstrap path nor the
// steady-state path can quietly enforce something weaker than the other.
//
// 🔴 IT FAILS CLOSED ON AN EMPTY OBSERVATION.
//
// Observing no signatures is reported as a failure, not a pass. "Nothing found"
// resolving to success is precisely the vacuous control this exists to close:
// it is what a broken fetch, an unauthenticated registry read, and a genuinely
// unsigned artifact all look like.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var (
	errMatcherDrift         = errors.New("the two matcher halves disagree")
	errMatcherMissing       = errors.New("no cosign matcher found")
	errNoAcceptedSigner     = errors.New("the configured matcher accepts none of the observed signers")
	errNoSignaturesObserved = errors.New("no signatures were observed for the artifact")
	errBadPattern           = errors.New("matcher pattern does not compile")
)

// matcher is one matchOIDCIdentity entry: the two regexes Flux hands to cosign.
type matcher struct {
	Issuer  string
	Subject string
}

// signerIdentity is one identity observed to have actually signed the artifact,
// as read from the certificate on a real signature.
type signerIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// extractMatcher reads the matcher from BOTH halves and requires them to agree.
// configManifest carries the bootstrap path (spec.workload.flux.verify);
// instanceManifest carries the steady-state path (the FluxInstance kustomize
// patch that has the operator itself write spec.verify onto the root source).
func extractMatcher(configManifest, instanceManifest []byte) (matcher, error) {
	fromConfig, err := matcherFromConfig(configManifest)
	if err != nil {
		return matcher{}, fmt.Errorf("bootstrap half: %w", err)
	}

	fromInstance, err := matcherFromInstance(instanceManifest)
	if err != nil {
		return matcher{}, fmt.Errorf("steady-state half: %w", err)
	}

	if fromConfig != fromInstance {
		return matcher{}, fmt.Errorf(
			"%w: bootstrap has issuer=%q subject=%q; steady-state has issuer=%q subject=%q",
			errMatcherDrift,
			fromConfig.Issuer, fromConfig.Subject,
			fromInstance.Issuer, fromInstance.Subject,
		)
	}

	return fromConfig, nil
}

// assertVerifies is the whole question, in one place: does the configured
// matcher accept at least one identity that really signed the artifact?
func assertVerifies(m matcher, observed []signerIdentity) error {
	issuer, err := regexp.Compile(m.Issuer)
	if err != nil {
		return fmt.Errorf("%w: issuer %q: %w", errBadPattern, m.Issuer, err)
	}

	subject, err := regexp.Compile(m.Subject)
	if err != nil {
		return fmt.Errorf("%w: subject %q: %w", errBadPattern, m.Subject, err)
	}

	if len(observed) == 0 {
		return fmt.Errorf(
			"%w: cannot prove the matcher verifies anything. A matcher that guards an "+
				"artifact carrying no readable signature is inert in exactly the way #3005 was",
			errNoSignaturesObserved,
		)
	}

	for _, identity := range observed {
		if issuer.MatchString(identity.Issuer) && subject.MatchString(identity.Subject) {
			return nil
		}
	}

	return fmt.Errorf(
		"%w: matcher issuer=%q subject=%q; %s",
		errNoAcceptedSigner, m.Issuer, m.Subject, describeObserved(observed),
	)
}

// describeObserved names what DID sign, which is the difference between a
// failure someone can act on and one they have to reproduce by hand.
func describeObserved(observed []signerIdentity) string {
	described := fmt.Sprintf("%d signer(s) actually signed this artifact:", len(observed))
	for _, identity := range observed {
		described += fmt.Sprintf("\n  issuer=%q subject=%q", identity.Issuer, identity.Subject)
	}

	return described
}

// matcherFromConfig reads spec.workload.flux.verify.matchOIDCIdentity — the
// path KSail actually reads, mirrored from scripts/validate-flux-verify.
func matcherFromConfig(manifest []byte) (matcher, error) {
	var document map[string]any
	if err := yaml.Unmarshal(manifest, &document); err != nil {
		return matcher{}, fmt.Errorf("parse: %w", err)
	}

	verify, err := descend(document, "spec", "workload", "flux", "verify")
	if err != nil {
		return matcher{}, err
	}

	return soleMatcher(verify)
}

// matcherFromInstance digs the matcher out of the FluxInstance kustomize patch.
// The patch body is a STRING holding its own YAML document, so this parses a
// second time rather than walking through it.
func matcherFromInstance(manifest []byte) (matcher, error) {
	var document map[string]any
	if err := yaml.Unmarshal(manifest, &document); err != nil {
		return matcher{}, fmt.Errorf("parse: %w", err)
	}

	patches, err := descend(document, "spec", "kustomize", "patches")
	if err != nil {
		return matcher{}, err
	}

	entries, ok := patches.([]any)
	if !ok {
		return matcher{}, fmt.Errorf("%w: spec.kustomize.patches is not a list", errMatcherMissing)
	}

	for _, entry := range entries {
		body, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		if !targetsRootSource(body["target"]) {
			continue
		}

		patch, ok := body["patch"].(string)
		if !ok {
			return matcher{}, fmt.Errorf("%w: root-source patch body is not a string", errMatcherMissing)
		}

		return matcherFromPatchBody(patch)
	}

	return matcher{}, fmt.Errorf("%w: no kustomize patch targets the root OCIRepository", errMatcherMissing)
}

// matcherFromPatchBody parses the JSON-patch operation list the patch string
// holds and reads the matcher out of the added spec.verify value.
func matcherFromPatchBody(patch string) (matcher, error) {
	var operations []any
	if err := yaml.Unmarshal([]byte(patch), &operations); err != nil {
		return matcher{}, fmt.Errorf("parse patch body: %w", err)
	}

	for _, operation := range operations {
		body, ok := operation.(map[string]any)
		if !ok {
			continue
		}

		if body["path"] != "/spec/verify" {
			continue
		}

		return soleMatcher(body["value"])
	}

	return matcher{}, fmt.Errorf("%w: patch adds nothing at /spec/verify", errMatcherMissing)
}

// targetsRootSource reports whether a patch target names the flux-system
// OCIRepository. A mistargeted patch renders nothing, exits 0 and warns nobody,
// so the target is checked rather than assumed.
func targetsRootSource(value any) bool {
	target, ok := value.(map[string]any)
	if !ok {
		return false
	}

	return target["kind"] == "OCIRepository" &&
		target["name"] == "flux-system" &&
		target["namespace"] == "flux-system"
}

// soleMatcher reads the single matchOIDCIdentity entry. More than one is
// refused: cosign rejects a multi-entry list outright and fails closed for the
// whole set, so a list verifies nothing while looking stricter.
func soleMatcher(verify any) (matcher, error) {
	block, ok := verify.(map[string]any)
	if !ok {
		return matcher{}, fmt.Errorf("%w: verify block is not a mapping", errMatcherMissing)
	}

	identities, ok := block["matchOIDCIdentity"].([]any)
	if !ok {
		return matcher{}, fmt.Errorf("%w: matchOIDCIdentity is absent or not a list", errMatcherMissing)
	}

	if len(identities) != 1 {
		return matcher{}, fmt.Errorf(
			"%w: matchOIDCIdentity has %d entries, want exactly 1", errMatcherMissing, len(identities),
		)
	}

	entry, ok := identities[0].(map[string]any)
	if !ok {
		return matcher{}, fmt.Errorf("%w: matchOIDCIdentity entry is not a mapping", errMatcherMissing)
	}

	issuer, _ := entry["issuer"].(string)
	subject, _ := entry["subject"].(string)

	if issuer == "" || subject == "" {
		return matcher{}, fmt.Errorf("%w: matcher has a blank issuer or subject", errMatcherMissing)
	}

	return matcher{Issuer: issuer, Subject: subject}, nil
}

// descend walks a path, reporting the first missing step rather than a nil.
func descend(document map[string]any, path ...string) (any, error) {
	var current any = document

	for index, step := range path {
		node, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %v is not a mapping", errMatcherMissing, path[:index])
		}

		next, present := node[step]
		if !present {
			return nil, fmt.Errorf("%w: no key at %v", errMatcherMissing, path[:index+1])
		}

		current = next
	}

	return current, nil
}

// readObserved loads the identities a CI step observed on the real artifact.
// The fetch lives in the workflow so this stays deterministic and testable; the
// fetch never restates the patterns, it only reports who signed.
func readObserved(path string) ([]signerIdentity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read observed signers: %w", err)
	}

	var observed []signerIdentity
	if err := json.Unmarshal(raw, &observed); err != nil {
		return nil, fmt.Errorf("parse observed signers: %w", err)
	}

	return observed, nil
}

// printMatcher writes the manifest's own patterns, issuer first, one per line,
// so a caller can hand them straight to cosign. This is what keeps the thing
// tested identical to the thing deployed: the caller never spells a pattern.
func printMatcher(configPath, instancePath string) error {
	configManifest, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config manifest: %w", err)
	}

	instanceManifest, err := os.ReadFile(instancePath)
	if err != nil {
		return fmt.Errorf("read instance manifest: %w", err)
	}

	configured, err := extractMatcher(configManifest, instanceManifest)
	if err != nil {
		return err
	}

	fmt.Println(configured.Issuer)
	fmt.Println(configured.Subject)

	return nil
}

// checkObserved applies the manifest's matcher to identities some other tool
// already read off the artifact.
func checkObserved(configPath, instancePath, observedPath string) error {
	configManifest, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config manifest: %w", err)
	}

	instanceManifest, err := os.ReadFile(instancePath)
	if err != nil {
		return fmt.Errorf("read instance manifest: %w", err)
	}

	configured, err := extractMatcher(configManifest, instanceManifest)
	if err != nil {
		return err
	}

	observed, err := readObserved(observedPath)
	if err != nil {
		return err
	}

	if err := assertVerifies(configured, observed); err != nil {
		return err
	}

	fmt.Printf("matcher verifies the artifact: issuer=%s subject=%s\n",
		configured.Issuer, configured.Subject)

	return nil
}

func main() {
	var err error

	switch {
	case len(os.Args) == 4 && os.Args[1] == "--print-matcher":
		err = printMatcher(os.Args[2], os.Args[3])
	case len(os.Args) == 4:
		err = checkObserved(os.Args[1], os.Args[2], os.Args[3])
	default:
		fmt.Fprintf(os.Stderr,
			"usage: %s --print-matcher <config-manifest> <flux-instance-manifest>\n"+
				"       %s <config-manifest> <flux-instance-manifest> <observed-signers.json>\n",
			os.Args[0], os.Args[0])
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-matcher-efficacy: %v\n", err)
		os.Exit(1)
	}
}
