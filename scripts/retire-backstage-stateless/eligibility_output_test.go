package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestEligibilityOutputIdentifiesPredicateWithoutDisclosingEvidence(t *testing.T) {
	for _, mode := range []string{"--execute", "--check"} {
		for _, sample := range []int{1, 2} {
			for _, tc := range []struct {
				name, category string
				change         func(*portalFixture)
			}{
				{"release state", "release-state", func(f *portalFixture) { object(f.release["info"])["status"] = "pending-upgrade" }},
				{"owner profile", "owner-profile", func(f *portalFixture) { object(f.hr["spec"])["suspend"] = true }},
				{"owner observation", "owner-observation", func(f *portalFixture) { object(f.hr["status"])["observedGeneration"] = 0 }},
				{"release history", "release-history", func(f *portalFixture) { delete(object(f.hr["status"]), "history") }},
				{"chart binding", "chart-binding", func(f *portalFixture) {
					object(field(f.chart, "metadata", "labels"))["helm.toolkit.fluxcd.io/name"] = "other"
				}},
				{"deployment source", "deployment-source", func(f *portalFixture) { object(field(f.sources, "status", "artifact"))["revision"] = "other" }},
				{"private parser error", "unclassified", func(f *portalFixture) { f.release["manifest"] = "metadata: [DO_NOT_PRINT" }},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", mode, sample, tc.name), func(t *testing.T) {
					if mode == "--check" {
						checkRunner(t)
					} else {
						retirementRunner(t)
					}
					f := retirementFixture(t)
					samples := 0
					execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
						if program == "helm" {
							samples++
							if samples == sample {
								tc.change(f)
							}
						}
						return f.execute(ctx, program, args...)
					}
					var out bytes.Buffer
					code := run([]string{mode}, &out, execute)
					phase := "first"
					if sample == 2 {
						phase = "second"
					}
					want := "PORTAL-RETIREMENT=HOLD_PRECONDITION stage=eligibility-" + phase + " category=" + tc.category + "\n"
					if code != 2 || out.String() != want || samples != sample || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || f.chartDeleted {
						t.Fatalf("eligibility refusal lost its bounded no-write classification: code=%d samples=%d output=%q want=%q", code, samples, out.String(), want)
					}
				})
			}
		}
	}
}

func TestEligibilityCategoryDoesNotEchoOrPartiallyMatchPrivateErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("DO_NOT_PRINT credential"),
		errors.New("uninstall owner unverified: DO_NOT_PRINT"),
		fmt.Errorf("DO_NOT_PRINT: %w", errors.New("uninstall owner unverified")),
	} {
		if got := eligibilityCategory(err); got != "unclassified" {
			t.Fatalf("unknown or private error acquired a public classification: %q", got)
		}
	}
}
