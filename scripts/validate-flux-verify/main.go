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

// validate reports why the config's signature verification is not in effect, or
// nil when it is.
func validate(config []byte) error {
	var tree any
	if err := yaml.Unmarshal(config, &tree); err != nil {
		return fmt.Errorf("cluster config does not parse, so verification cannot be established: %w", err)
	}

	// Reported before the read-path check, because a stray block is the reason
	// the read path is usually empty and naming it is the actionable half.
	if stray := strayVerifyPaths(tree, nil); len(stray) > 0 {
		return fmt.Errorf(
			"verify block at %s, which KSail does not read: move it to %s or delete it "+
				"(a block at any other path is discarded in silence and verifies nothing)",
			strings.Join(stray, ", "), strings.Join(readPath, "."),
		)
	}

	value, ok := lookup(tree, readPath)
	if !ok {
		return fmt.Errorf(
			"no verify block at %s, so the generated flux-system OCIRepository pulls unverified: "+
				"add a cosign block there",
			strings.Join(readPath, "."),
		)
	}

	block, ok := asMapping(value)
	if !ok {
		return fmt.Errorf("%s is not a mapping, so KSail cannot decode it into a verify spec",
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

		return fmt.Errorf(
			"%s.provider is %#v, which KSail treats as verification DISABLED "+
				"(it renders spec.verify only when the provider is a non-blank string): set it to cosign",
			strings.Join(readPath, "."), reported,
		)
	}

	return nil
}

func run(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-flux-verify <ksail-config.yaml>")

		return 1
	}

	config, err := os.ReadFile(args[0]) //nolint:gosec // Explicit path from the caller.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: read cluster config: %v\n", err)

		return 1
	}

	if err := validate(config); err != nil {
		_, _ = fmt.Fprintf(stderr, "flux verify contract: %v\n", err)

		return 1
	}

	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
