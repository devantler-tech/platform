package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// Operator output must distinguish preflight failures without echoing the
// command output or errors that can contain credentials and resource content.
func TestPreconditionOutputIdentifiesStageWithoutDisclosingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   string
		program string
		call    int
		success bool
	}{
		{name: "opening source", stage: "source-opening", program: "bash", call: 1},
		{name: "read-only assessment", stage: "assessment", program: "go", call: 1},
		{name: "invalid successful assessment", stage: "assessment", program: "go", call: 1, success: true},
		{name: "opening evidence", stage: "evidence-first", program: "helm", call: 1},
		{name: "sample separation", stage: "sample-separation", program: "sleep", call: 1},
		{name: "second evidence", stage: "evidence-second", program: "helm", call: 2},
		{name: "final source", stage: "source-before-journal", program: "bash", call: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			calls := 0
			execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
				if program == tc.program {
					calls++
					if calls == tc.call {
						var err error
						if !tc.success {
							err = errors.New("DO_NOT_PRINT credential")
						}
						return []byte("DO_NOT_PRINT command output\nPORTAL-RETIREMENT=RETIRED_STATELESS"), err
					}
				}
				return f.execute(ctx, program, args...)
			}
			var out bytes.Buffer
			code := run([]string{"--execute"}, &out, execute)
			want := "PORTAL-RETIREMENT=HOLD_PRECONDITION stage=" + tc.stage + "\n"
			if code != 2 || out.String() != want || calls != tc.call {
				t.Fatalf("wrong public refusal: code=%d calls=%d output=%q want=%q", code, calls, out.String(), want)
			}
			if f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || strings.Contains(out.String(), "DO_NOT_PRINT") {
				t.Fatalf("failed precondition leaked evidence or performed a write: %q", out.String())
			}
		})
	}
}

func TestPreconditionOutputSeparatesUnsafeAndChangedSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		sample int
		change func(*portalFixture)
	}{
		{name: "unsafe opening sample", stage: "eligibility-first", sample: 1, change: func(f *portalFixture) {
			object(f.hr["spec"])["suspend"] = true
		}},
		{name: "unsafe second sample", stage: "eligibility-second", sample: 2, change: func(f *portalFixture) {
			object(f.hr["spec"])["suspend"] = true
		}},
		{name: "changed owner", stage: "snapshot-second", sample: 2, change: func(f *portalFixture) {
			object(f.hr["metadata"])["resourceVersion"] = "99"
		}},
		{name: "changed retained credentials", stage: "preservation-second", sample: 2, change: func(f *portalFixture) {
			item(f.inventory, 0)["data"] = map[string]any{"password": "DO_NOT_PRINT changed"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			samples := 0
			execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
				if program == "helm" {
					samples++
					if samples == tc.sample {
						tc.change(f)
					}
				}
				return f.execute(ctx, program, args...)
			}
			var out bytes.Buffer
			code := run([]string{"--execute"}, &out, execute)
			want := "PORTAL-RETIREMENT=HOLD_PRECONDITION stage=" + tc.stage + "\n"
			if code != 2 || out.String() != want || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
				t.Fatalf("unsafe sample lost its bounded no-write refusal: code=%d output=%q want=%q", code, out.String(), want)
			}
		})
	}
}
