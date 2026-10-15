package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func checkRunner(t *testing.T) {
	t.Helper()
	retirementRunner(t)
	t.Setenv("PORTAL_RETIREMENT_CONFIRM", "CHECK BACKSTAGE RETIREMENT ONLY")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/check-backstage-retirement.yaml@refs/heads/main")
}

// A check must exercise both complete snapshots and the closing source read,
// but must never create an operation receipt or enter the native lifecycle.
func TestReadOnlyCheckProvesOpeningPreconditionsWithoutMutation(t *testing.T) {
	checkRunner(t)
	f := retirementFixture(t)
	sources, separated := 0, 0
	execute := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "bash" {
			sources++
		}
		if name == "sleep" {
			separated++
		}
		return f.execute(ctx, name, args...)
	}
	var out bytes.Buffer
	code := run([]string{"--check"}, &out, execute)
	if code != 0 || out.String() != "PORTAL-RETIREMENT=CHECKED preservationSamples=2 mutation=NONE retirement=NOT_EXECUTED dataRetirement=UNASSESSED\n" {
		t.Fatalf("read-only preflight unavailable: code=%d output=%q", code, out.String())
	}
	if sources != 2 || separated != 1 || f.preservationReads != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || f.chartDeleted || f.historyStatus != "deployed" {
		t.Fatalf("check skipped evidence or changed the lifecycle: sources=%d separation=%d reads=%d receipt=%t fence=%t configured=%t deleted=%t history=%s", sources, separated, f.preservationReads, f.receiptCreated, f.fenceInstalled, f.configured, f.deleted, f.historyStatus)
	}
}

func TestReadOnlyCheckCannotUseExecutionConfirmation(t *testing.T) {
	for _, tc := range []struct{ arg, confirmation string }{
		{"--check", "RETIRE BACKSTAGE STATELESS ONLY"},
		{"--execute", "CHECK BACKSTAGE RETIREMENT ONLY"},
		{"--check", ""},
	} {
		t.Run(tc.arg+tc.confirmation, func(t *testing.T) {
			checkRunner(t)
			t.Setenv("PORTAL_RETIREMENT_CONFIRM", tc.confirmation)
			calls := 0
			var out bytes.Buffer
			code := run([]string{tc.arg}, &out, func(context.Context, string, ...string) ([]byte, error) {
				calls++
				return nil, fmt.Errorf("must not run")
			})
			if code != 2 || calls != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_CONFIRMATION\n" {
				t.Fatalf("mode confirmation crossed: code=%d calls=%d output=%q", code, calls, out.String())
			}
		})
	}
}

func TestReadOnlyCheckRejectsUnsafeOrChangedEvidence(t *testing.T) {
	for _, scenario := range []string{"unretained volume", "changed credential", "read failure", "existing journal", "closing source changed"} {
		t.Run(scenario, func(t *testing.T) {
			checkRunner(t)
			f := retirementFixture(t)
			if scenario == "unretained volume" {
				object(item(f.volumes, 0)["spec"])["persistentVolumeReclaimPolicy"] = "Delete"
			}
			if scenario == "existing journal" {
				object(f.namespace["metadata"])["annotations"].(map[string]any)[fenceAnnotation] = "foreign"
			}
			sources := 0
			execute := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "bash" {
					sources++
					if scenario == "closing source changed" && sources == 2 {
						return nil, fmt.Errorf("DO_NOT_PRINT")
					}
				}
				if name == "sleep" && scenario == "changed credential" {
					object(item(f.inventory, 0)["data"])["password"] = "CHANGED_DO_NOT_PRINT"
				}
				if name == "helm" && scenario == "read failure" {
					return []byte("DO_NOT_PRINT\nPORTAL-RETIREMENT=CHECKED"), fmt.Errorf("DO_NOT_PRINT")
				}
				return f.execute(ctx, name, args...)
			}
			var out bytes.Buffer
			code := run([]string{"--check"}, &out, execute)
			if code != 2 || !strings.HasPrefix(out.String(), "PORTAL-RETIREMENT=HOLD_") || strings.Contains(out.String(), "DO_NOT_PRINT") || strings.Contains(out.String(), "CHECKED") || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
				t.Fatalf("unsafe check admitted or leaked evidence: code=%d output=%q", code, out.String())
			}
		})
	}
}

// Removing the preflight's early return must still be unable to submit any
// operation, including admission dry-runs, through the shared write transport.
func TestReadOnlyTransportRefusesEveryMutationBeforeSubmission(t *testing.T) {
	for name, args := range map[string][]string{
		"receipt": {"create", "-f", "REQUEST", "--output=json"},
		"fence":   {"apply", "-f", "REQUEST", "--output=json"},
		"options": {"--namespace=backstage", "patch", "helmrelease", "backstage", "--type=json", "--patch-file=REQUEST", "--output=json"},
		"delete":  {"delete", "--raw", "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/backstage", "-f", "REQUEST"},
		"dry-run": {"delete", "--raw", "/api/v1/namespaces/backstage/configmaps/backstage-app-config?dryRun=All", "-f", "REQUEST"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			e := engine{readOnly: true, execute: func(context.Context, string, ...string) ([]byte, error) {
				calls++
				return []byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`), nil
			}}
			response, submitted, err := e.writeObserved(context.Background(), map[string]any{"synthetic": "DO_NOT_PRINT"}, args...)
			if err == nil || submitted || response != nil || calls != 0 {
				t.Fatalf("read-only transport submitted a mutation: error=%t submitted=%t response=%v calls=%d", err != nil, submitted, response, calls)
			}
		})
	}
}

func TestReadOnlyCheckRetainsProtectedRunnerAdmission(t *testing.T) {
	for key, value := range map[string]string{
		"GITHUB_ACTIONS": "false", "GITHUB_REPOSITORY": "other/repo", "GITHUB_REF": "refs/heads/branch",
		"GITHUB_RUN_ATTEMPT": "2", "GITHUB_SHA": "short", "GITHUB_RUN_ID": "0", "PORTAL_RETIREMENT_REVISION": "latest",
		"GITHUB_EVENT_NAME": "push", "GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/retire-backstage-stateless.yaml@refs/heads/main",
	} {
		t.Run(key, func(t *testing.T) {
			checkRunner(t)
			t.Setenv(key, value)
			calls := 0
			var out bytes.Buffer
			code := run([]string{"--check"}, &out, func(context.Context, string, ...string) ([]byte, error) {
				calls++
				return nil, fmt.Errorf("must not run")
			})
			if code != 2 || calls != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_CONFIRMATION\n" {
				t.Fatalf("untrusted check invoked a command: code=%d calls=%d output=%q", code, calls, out.String())
			}
		})
	}
}

func TestReadOnlyCheckCannotReportUnwrittenSuccess(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			checkRunner(t)
			f := retirementFixture(t)
			if code := run([]string{"--check"}, failedResultWriter{short: short}, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
				t.Fatalf("failed check output accepted: code=%d", code)
			}
		})
	}
}
