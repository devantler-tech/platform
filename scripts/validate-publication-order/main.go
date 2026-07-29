// Command validate-publication-order checks that production is never pointed at
// an artifact whose supply-chain evidence is still being assembled.
//
// The prod deploy publishes the platform manifests to a mutable tag, signs them
// with keyless cosign, attests an SBOM and SLSA provenance, and only then tells
// Flux to reconcile. That order is the whole guarantee: reordering any of it
// leaves a window in which the cluster's root source — which delivers every
// controller, tenant binding and policy — resolves bytes that carry no
// signature or no attestation.
//
// Nothing about that window is visible in a green deploy. The steps all
// succeed, the cluster converges, and the artifact ends up fully signed a
// minute later; the defect is only that Flux could have looked in between. So
// the ordering has no natural regression signal, and a later edit that moves
// the reconcile trigger up to "fail faster" would be indistinguishable from an
// improvement. This turns that into a pull-request failure.
//
// Scope: this pins the order of the steps that already exist. It does NOT
// establish that publication is atomic — the mutable tag is still moved by the
// push step before signing begins, which is the substance of
// devantler-tech/platform#2627 and needs a staging reference to fix. Do not
// read a green run here as that issue being closed.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// step is one entry of runs.steps, reduced to what identifies it here.
type step struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
	// If is read because ordering is positional: a step keeps its position while carrying a
	// condition that never fires, so the sequence can be satisfied by steps that do not run.
	If string `yaml:"if"`
}

type action struct {
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

// marker locates one step of the publication sequence. Matching on what a step
// DOES — the command it runs or the action it uses — rather than on its name
// keeps the check working when a name is reworded, and failing when the step it
// describes actually disappears.
type marker struct {
	// label is how the step is described in an error message.
	label string
	// match reports whether a step is this one.
	match func(step) bool
}

// executable drops comment-only lines from a shell script before it is matched.
//
// Every matcher here works on raw script text, so without this a `#` in front of the invocation
// satisfies them all: the comment still contains the command AND the reference. The deploy would
// publish and reconcile an artifact nothing signed while this check stayed green — a guard that a
// single character disables is not a guard.
//
// Only whole-line comments are removed. A trailing `#` is left alone deliberately: deciding whether
// one opens a comment or sits inside a string literal needs a shell parser, and guessing wrong would
// silently drop a real invocation.
func executable(run string) string {
	lines := strings.Split(run, "\n")
	kept := make([]string, 0, len(lines))

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		kept = append(kept, line)
	}

	return strings.Join(kept, "\n")
}

func runContains(substr string) func(step) bool {
	return func(s step) bool { return strings.Contains(executable(s.Run), substr) }
}

func usesPrefix(prefix string) func(step) bool {
	return func(s step) bool { return strings.HasPrefix(s.Uses, prefix) }
}

// The contract is a partial order, not a chain: publish first, then the
// evidence in any order among itself, then the release. Modelling it as a
// straight sequence would additionally forbid swapping the two attestations,
// which is a free choice — failing CI on a legitimate reordering trains people
// to treat this check as noise.
//
// The SBOM generator is deliberately absent: it produces a file rather than
// publishing anything, so its position carries no guarantee.
var (
	// publish makes new bytes reachable and must come first — signing or
	// attesting before it would cover the previous artifact.
	publish = marker{"publish the manifests (`workload push`)", runContains("workload push")}

	// sign is named separately because its step is inspected again below, for
	// the digest reference. Locating it by value rather than by matching its
	// label keeps that second lookup correct when the wording changes.
	sign = marker{"sign the published digest (`cosign sign`)", runContains("cosign sign ")}

	// evidence is what must exist before production may look. Order among
	// these is unconstrained.
	evidence = []marker{
		sign,
		{"attest the SBOM (`actions/attest`)", usesPrefix("actions/attest@")},
		{"attest build provenance (`actions/attest-build-provenance`)", usesPrefix("actions/attest-build-provenance@")},
	}

	// release is the step that advances what production can see.
	release = marker{"tell Flux to reconcile (`workload reconcile`)", runContains("workload reconcile")}
)

// digestRef is the form the signing step must use. Signing the mutable tag
// instead would let a concurrent deploy move it between resolve and sign, so
// the signature would cover bytes this run never published.
const digestRef = `REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`

// indicesOf returns the position of EVERY step matching m, in file order.
//
// Every occurrence matters, not just the first. A composite that pushes, signs,
// reconciles and then pushes again satisfies the contract on its first push
// while republishing bytes nothing signed or attested — so locating one step of
// each kind and stopping would validate exactly the window this guard exists to
// close.
func indicesOf(steps []step, m marker) []int {
	var at []int

	for i, s := range steps {
		if m.match(s) {
			at = append(at, i)
		}
	}

	return at
}

// signRefArg is the argument the cosign invocation must consume. Asserting the
// assignment of digestRef alone is not enough: a step can resolve the digest
// into REF and still sign "${REF_TAG}", leaving the assignment dead and the
// signature covering whatever the mutable tag resolves to at sign time.
const signRefArg = `"${REF}"`

// signsTheResolvedDigest reports whether every cosign invocation in run signs
// the reference built from the resolved digest.
func signsTheResolvedDigest(run string) bool {
	for _, line := range strings.Split(executable(run), "\n") {
		if !strings.Contains(line, "cosign sign ") {
			continue
		}

		if !strings.Contains(line, signRefArg) {
			return false
		}
	}

	return true
}

// mustNotSkipIndependently rejects a required step whose condition lets it be skipped while the
// release still runs. releaseIdx[0] is the binding release for ordering, so its condition is the one
// a required step must share to stand or fall with it.
func mustNotSkipIndependently(steps []step, m marker, idx []int, releaseIdx []int) error {
	releaseIf := strings.TrimSpace(steps[releaseIdx[0]].If)

	for _, at := range idx {
		stepIf := strings.TrimSpace(steps[at].If)
		if stepIf == "" || stepIf == releaseIf {
			continue
		}

		return fmt.Errorf(
			"the step that must %s carries `if: %s`, which does not match the release step's"+
				" condition (%q), so it can be skipped while the release still runs.\n"+
				"GitHub skips a false-conditioned step without failing the composite, so the ordering"+
				" above would still hold while production is released without that evidence",
			m.label, stepIf, releaseIf)
	}

	return nil
}

func validate(source []byte) error {
	var parsed action
	if err := yaml.Unmarshal(source, &parsed); err != nil {
		return fmt.Errorf("could not parse as YAML: %w", err)
	}

	steps := parsed.Runs.Steps
	if len(steps) == 0 {
		return errors.New("no runs.steps; this does not look like a composite action")
	}

	locate := func(m marker) ([]int, error) {
		at := indicesOf(steps, m)
		if len(at) == 0 {
			return nil, fmt.Errorf(
				"no step appears to %s.\n"+
					"If that step was renamed, this check follows what a step does rather than what it is called — "+
					"restore the command or action it matches on, or update the marker here deliberately",
				m.label)
		}

		return at, nil
	}

	// Comparing the LAST occurrence of the earlier kind against the FIRST of the
	// later kind is what makes this hold across every pair: if even the latest
	// publish precedes the earliest piece of evidence, all of them do.
	mustPrecede := func(earlier marker, earlierIdx []int, later marker, laterIdx []int) error {
		earlierAt := earlierIdx[len(earlierIdx)-1]
		laterAt := laterIdx[0]

		if earlierAt < laterAt {
			return nil
		}

		return fmt.Errorf(
			"the deploy must %s BEFORE it can %s, but the steps are in the opposite order "+
				"(positions %d and %d).\n"+
				"Flux resolves the mutable tag on its own schedule, so any step that advances what "+
				"production can see must come after the evidence it depends on is published",
			earlier.label, later.label, earlierAt+1, laterAt+1)
	}

	publishAt, err := locate(publish)
	if err != nil {
		return err
	}

	releaseAt, err := locate(release)
	if err != nil {
		return err
	}

	for _, m := range evidence {
		at, err := locate(m)
		if err != nil {
			return err
		}

		if err := mustPrecede(publish, publishAt, m, at); err != nil {
			return err
		}

		if err := mustPrecede(m, at, release, releaseAt); err != nil {
			return err
		}
	}

	// Ordering is positional, so a step satisfies every comparison while carrying a condition that
	// never fires. GitHub then skips it WITHOUT failing the composite and runs the unconditional
	// release anyway, publishing without evidence the sequence claims is there.
	//
	// The test is not "may it have a condition" but "may it skip while the release still runs". A
	// step guarded by the SAME condition as the release stands or falls with it, so that stays legal;
	// anything else can vanish on its own.
	if err := mustNotSkipIndependently(steps, publish, publishAt, releaseAt); err != nil {
		return err
	}

	for _, m := range evidence {
		at, err := locate(m)
		if err != nil {
			return err
		}

		if err := mustNotSkipIndependently(steps, m, at, releaseAt); err != nil {
			return err
		}
	}

	// sign is in evidence, so the loop above has already proved it resolves.
	signAt, err := locate(sign)
	if err != nil {
		return err
	}

	for _, at := range signAt {
		if !strings.Contains(executable(steps[at].Run), digestRef) {
			return fmt.Errorf(
				"the signing step must build its reference from the resolved digest (%s); "+
					"signing the mutable tag lets a concurrent deploy move it between resolve and sign",
				digestRef)
		}

		if !signsTheResolvedDigest(steps[at].Run) {
			return fmt.Errorf(
				"the signing step resolves the digest into %s but does not pass %s to `cosign sign`; "+
					"the assignment is dead and the signature would cover whatever the mutable tag "+
					"resolves to at sign time",
				digestRef, signRefArg)
		}
	}

	return nil
}

func run(paths []string, stderr io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: validate-publication-order <action.yml>")
	}

	failed := false

	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			// Fail closed: an unreadable action is not a validated action.
			_, _ = fmt.Fprintf(stderr, "::error::%s: could not read: %v\n", path, err)
			failed = true

			continue
		}

		if err := validate(source); err != nil {
			_, _ = fmt.Fprintf(stderr, "::error::%s: %v\n", path, err)
			failed = true
		}
	}

	if failed {
		return errors.New("the publication ordering contract is not satisfied")
	}

	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}
