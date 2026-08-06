// Command validate-flux-verify checks that the platform's Flux signature
// verification is actually IN EFFECT, rather than merely present somewhere in
// the cluster config.
//
// # WHY THIS IS NOT A GREP
//
// The flux-system OCIRepository is the root of the whole platform: every
// controller, tenant binding and policy arrives through it. Verification on
// that source ran with spec.verify unset for roughly three weeks (#2627) while
// CI stayed green the entire time. The config was not missing the block — it
// carried a complete, correct, well-commented cosign block the whole time, at
// spec.cluster.verify. ClusterSpec has no verify field, so KSail discarded the
// key in silence and rendered an OCIRepository that pulled a mutable tag
// unverified.
//
// A text scan for `verify:` would have passed on every single day of that
// outage. So would a scan for the cosign matcher subjects, which were also
// present and also inert. The only question that separates a configured
// platform from an unprotected one is whether the block resolves at the path
// KSail READS, so that is the only question this validator asks.
//
// 🔴 THE ENABLED PREDICATE IS KSAIL'S, NOT AN APPROXIMATION OF IT.
//
// KSail decides whether to render spec.verify onto the OCIRepository with
// FluxVerifySpec.Enabled(), which is `strings.TrimSpace(Provider) != ""`
// (ksail pkg/apis/cluster/v1alpha1/flux_types.go). A check that asked the
// obvious `provider != ""` instead would accept `provider: "  "` and report a
// verified platform while KSail rendered no spec.verify at all — the identical
// silent-inertness defect this validator exists to catch, one layer deeper and
// considerably harder to see. Mirroring the consumer's own predicate is the
// point, not an implementation detail; if KSail's predicate changes, this must
// follow it rather than drift into a second, weaker opinion.
//
// A stray block is reported too, and deliberately fails closed. A verify key at
// any other path is either the outage shape itself or a leftover copy that
// invites someone to "tidy up" the live one. If a future KSail genuinely reads
// a verify key elsewhere, this fails loudly on the pull request that
// introduces it, which is the direction a security check should fail in.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// readPath is the only path KSail reads a Flux verify block from. Kept as a
// slice so the walk below can compare a full path rather than a leaf name.
var readPath = []string{"spec", "workload", "flux", "verify"}

// verifyKey is the leaf name a misplaced block keeps when it is written at the
// wrong level, which is how every observed instance of this defect has looked.
const verifyKey = "verify"

// enabled mirrors ksail's FluxVerifySpec.Enabled(). See the package comment:
// this must track KSail, not approximate it.
func enabled(provider string) bool {
	return strings.TrimSpace(provider) != ""
}

// constrainsSigner reports whether the verify block says WHO it trusts.
//
// A block that is present, at the read path, and enabled by KSail's predicate
// can still verify nothing useful: Flux treats a missing secretRef as KEYLESS
// verification, and matchOIDCIdentity is what pins the certificate issuer and
// subject. So `verify: {provider: cosign}` accepts any keylessly-signed
// artifact from any signer — configured, in effect, and guaranteeing nothing.
// That is the same class as the misplaced-block outage this validator was
// written for, one level further in.
//
// TWO shapes constrain a signer, and either is sufficient: an OIDC matcher
// naming both an issuer and a subject, or a secretRef pinning a public key.
// The keyed branch is accepted deliberately — a future keyed setup is a
// complete configuration, and failing it would only teach someone to bypass
// the gate.
//
// 🔴 THE FIELD SET IS KSAIL'S, FOR THE SAME REASON THE ENABLED PREDICATE IS.
//
// KSail's FluxVerifySpec carries exactly three fields — Provider, SecretRef
// and MatchOIDCIdentity (ksail pkg/apis/cluster/v1alpha1/flux_types.go, v7.178.14,
// the version this repo pins). It has NO trustedRootSecretRef, so a block
// written with one has that key dropped in silence and renders as a bare
// provider: present, enabled, and constraining nobody. Honouring a field the
// consumer does not model would make this check pass the exact configuration
// it exists to reject. Only the two fields KSail actually reads count here; if
// KSail gains a field, this follows it rather than anticipating it.
func constrainsSigner(block map[string]any) bool {
	return namesSecret(block["secretRef"]) ||
		hasUsableOIDCMatcher(block["matchOIDCIdentity"])
}

// namesSecret reports whether a LocalObjectReference actually names something.
// A `secretRef:` whose name is absent or blank pins no key, so presence of the
// key is not the assertion — a non-blank name is.
func namesSecret(value any) bool {
	reference, ok := asMapping(value)
	if !ok {
		return false
	}

	name, _ := reference["name"].(string)

	return strings.TrimSpace(name) != ""
}

// hasUsableOIDCMatcher reports whether at least ONE matcher entry pins both an
// issuer and a subject.
//
// Requiring one usable entry rather than every entry is deliberate: a
// half-written or commented-out sibling must not veto a block that does
// constrain a signer, or the gate starts failing correct configs and gets
// switched off. Requiring BOTH fields within that entry is equally deliberate —
// a blank issuer lets the subject alone decide trust, and a blank subject
// trusts every workflow the issuer will sign for, which on a public OIDC
// issuer is everyone.
func hasUsableOIDCMatcher(value any) bool {
	entries, ok := value.([]any)
	if !ok {
		return false
	}

	for _, entry := range entries {
		matcher, ok := asMapping(entry)
		if !ok {
			continue
		}

		issuer, _ := matcher["issuer"].(string)
		subject, _ := matcher["subject"].(string)

		if strings.TrimSpace(issuer) != "" && strings.TrimSpace(subject) != "" {
			return true
		}
	}

	return false
}

// asMapping normalises the two shapes yaml.v3 decodes a mapping into.
//
// A mapping whose keys are all strings decodes as map[string]any, but ONE
// non-string key anywhere in it — `1:`, `true:`, `~:` — switches the ENTIRE
// mapping to map[any]any. A walk matching only map[string]any would then skip
// that whole level, so a stray verify block sitting beside an integer key would
// go unreported while the read-path check still passed. That is a fail-open in
// a check whose whole purpose is to fail closed, so both shapes are handled
// here rather than at each call site.
func asMapping(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		normalised := make(map[string]any, len(typed))
		for key, item := range typed {
			normalised[fmt.Sprintf("%v", key)] = item
		}

		return normalised, true
	default:
		return nil, false
	}
}

// lookup walks a decoded YAML tree along path, returning the value at that path.
func lookup(tree any, path []string) (any, bool) {
	current := tree

	for _, key := range path {
		mapping, ok := asMapping(current)
		if !ok {
			return nil, false
		}

		current, ok = mapping[key]
		if !ok {
			return nil, false
		}
	}

	return current, true
}

// strayVerifyPaths returns every path holding a `verify` key other than the one
// KSail reads. Sorted, because Go randomises map iteration and an error message
// that lists the same paths in a different order on every run is both harder to
// read and impossible to assert on.
func strayVerifyPaths(tree any, path []string) []string {
	found := collectStrayVerifyPaths(tree, path)
	sort.Strings(found)

	return found
}

func collectStrayVerifyPaths(tree any, path []string) []string {
	var found []string

	if node, ok := asMapping(tree); ok {
		for key, value := range node {
			child := append(append([]string{}, path...), key)
			if key == verifyKey && !samePath(child, readPath) {
				found = append(found, strings.Join(child, "."))
			}

			found = append(found, collectStrayVerifyPaths(value, child)...)
		}

		return found
	}

	if node, ok := tree.([]any); ok {
		for index, value := range node {
			child := append(append([]string{}, path...), fmt.Sprintf("[%d]", index))
			found = append(found, collectStrayVerifyPaths(value, child)...)
		}
	}

	return found
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// decodeAll returns every YAML document in the input. yaml.Unmarshal reads only
// the first and discards the rest silently, which is the behaviour this
// validator exists to make loud, so it must not rely on it itself.
func decodeAll(config []byte) ([]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(config))

	var documents []any

	for {
		var document any

		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}

		if err != nil {
			return nil, err
		}

		// A trailing `---` with nothing after it decodes as a nil document and is
		// not a second resource, so it must not trip the one-document rule.
		if document == nil {
			continue
		}

		documents = append(documents, document)
	}
}

// validate reports why the config's signature verification is not in effect, or
// nil when it is.
func validate(config []byte) error {
	_, err := configVerifyBlock(config)

	return err
}

// configVerifyBlock is validate() plus the block it validated, so the drift
// check can compare the two halves without re-walking the tree.
func configVerifyBlock(config []byte) (map[string]any, error) {
	documents, err := decodeAll(config)
	if err != nil {
		return nil, fmt.Errorf("cluster config does not parse, so verification cannot be established: %w", err)
	}

	// A cluster config is ONE `kind: Cluster` resource, so everything after the
	// first document is not read as the cluster spec. yaml.Unmarshal drops those
	// documents without a word, which makes a second document exactly the kind of
	// place a verify block can sit while looking configured — measured: a valid
	// document one plus a stray block in document two validated clean. Refusing
	// the shape is simpler than guessing which document was meant to win.
	if len(documents) > 1 {
		return nil, fmt.Errorf(
			"cluster config has %d YAML documents; only the first is read as the cluster spec, "+
				"so anything configured in the rest verifies nothing: keep it to one document",
			len(documents),
		)
	}

	var tree any
	if len(documents) == 1 {
		tree = documents[0]
	}

	// Reported before the read-path check, because a stray block is the reason
	// the read path is usually empty and naming it is the actionable half.
	if stray := strayVerifyPaths(tree, nil); len(stray) > 0 {
		return nil, fmt.Errorf(
			"verify block at %s, which KSail does not read: move it to %s or delete it "+
				"(a block at any other path is discarded in silence and verifies nothing)",
			strings.Join(stray, ", "), strings.Join(readPath, "."),
		)
	}

	value, ok := lookup(tree, readPath)
	if !ok {
		return nil, fmt.Errorf(
			"no verify block at %s, so the generated flux-system OCIRepository pulls unverified: "+
				"add a cosign block there",
			strings.Join(readPath, "."),
		)
	}

	block, ok := asMapping(value)
	if !ok {
		return nil, fmt.Errorf("%s is not a mapping, so KSail cannot decode it into a verify spec",
			strings.Join(readPath, "."))
	}

	// A non-string provider (`provider: 123`) fails the assertion and lands here
	// as "", which is the right VERDICT — KSail cannot decode it into its string
	// field either — but reporting it as "" would misname what the file says and
	// send the reader looking for an empty value that is not there. Report the
	// raw node so the message names the edit to make.
	provider, isString := block["provider"].(string)
	if !enabled(provider) {
		reported := any(provider)
		if !isString {
			reported = block["provider"]
		}

		return nil, fmt.Errorf(
			"%s.provider is %#v, which KSail treats as verification DISABLED "+
				"(it renders spec.verify only when the provider is a non-blank string): set it to cosign",
			strings.Join(readPath, "."), reported,
		)
	}

	// Reported last, because it is the only check here that assumes the block is
	// otherwise well-formed and switched on.
	if !constrainsSigner(block) {
		return nil, fmt.Errorf(
			"%s is enabled but constrains no signer, so it accepts any keylessly-signed artifact "+
				"from any signer (Flux reads a missing secretRef as keyless): add a matchOIDCIdentity "+
				"entry with a non-blank issuer and subject, or a secretRef naming a key Secret "+
				"(those are the only two KSail reads — a trustedRootSecretRef is dropped in silence)",
			strings.Join(readPath, "."),
		)
	}

	return block, nil
}

// run checks BOTH halves of the contract, and requires both paths rather than
// making the second optional. The two configure different lifecycle stages —
// the cluster config covers bootstrap, the FluxInstance patch covers the
// running cluster — and either alone leaves a real window unverified. An
// optional second argument would let a caller silently drop the half that #2922
// was actually about, which is the failure this command exists to make loud.
func run(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-flux-verify <ksail-config.yaml> <flux-instance.yaml>")

		return 1
	}

	config, err := os.ReadFile(args[0]) //nolint:gosec // Explicit path from the caller.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: read cluster config: %v\n", err)

		return 1
	}

	configBlock, err := configVerifyBlock(config)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: %v\n", err)

		return 1
	}

	manifest, err := os.ReadFile(args[1]) //nolint:gosec // Explicit path from the caller.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: read FluxInstance manifest: %v\n", err)

		return 1
	}

	instanceBlock, err := instanceVerifyBlock(manifest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: %v\n", err)

		return 1
	}

	// Last, because it is the only check that assumes BOTH halves are already
	// well-formed: comparing a block that failed its own checks would report
	// drift where the real fault is the block itself.
	if err := checkNoDrift(configBlock, instanceBlock); err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: %v\n", err)

		return 1
	}

	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
