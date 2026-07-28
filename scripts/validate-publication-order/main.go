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

func runContains(substr string) func(step) bool {
	return func(s step) bool { return strings.Contains(s.Run, substr) }
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

// indexOf returns the position of the first step matching m.
func indexOf(steps []step, m marker) (int, bool) {
	for i, s := range steps {
		if m.match(s) {
			return i, true
		}
	}

	return 0, false
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

	locate := func(m marker) (int, error) {
		at, ok := indexOf(steps, m)
		if !ok {
			return 0, fmt.Errorf(
				"no step appears to %s.\n"+
					"If that step was renamed, this check follows what a step does rather than what it is called — "+
					"restore the command or action it matches on, or update the marker here deliberately",
				m.label)
		}

		return at, nil
	}

	mustPrecede := func(earlier marker, earlierAt int, later marker, laterAt int) error {
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

	// sign is in evidence, so the loop above has already proved it resolves.
	signAt, err := locate(sign)
	if err != nil {
		return err
	}

	if !strings.Contains(steps[signAt].Run, digestRef) {
		return fmt.Errorf(
			"the signing step must build its reference from the resolved digest (%s); "+
				"signing the mutable tag lets a concurrent deploy move it between resolve and sign",
			digestRef)
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
