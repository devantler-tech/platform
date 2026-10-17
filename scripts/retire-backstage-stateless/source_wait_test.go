package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func sourceWaitFixture(t *testing.T) *portalFixture {
	t.Helper()
	f := retirementFixture(t)
	object(f.hr["status"])["conditions"] = []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "SourceNotReady", "observedGeneration": 1},
		map[string]any{"type": "Reconciling", "status": "True", "reason": "Progressing", "message": "Fulfilling prerequisites", "observedGeneration": 1},
		map[string]any{"type": "Released", "status": "True", "reason": "UpgradeSucceeded", "observedGeneration": 1, "message": "Helm upgrade succeeded for release backstage/backstage.v4 with chart backstage@2.10.2"},
	}
	history := object(field(f.hr, "status", "history").([]any)[0])
	history["chartName"], history["chartVersion"] = "backstage", "2.10.2"
	object(f.chart["metadata"])["generation"] = 4
	f.chart["status"] = map[string]any{"observedGeneration": 4, "conditions": []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "SourceUnavailable", "observedGeneration": 4},
	}}
	return f
}

func TestSourceWaitPreservesHistoricalReleaseSuccessAndRefreshedPrerequisites(t *testing.T) {
	for _, action := range []string{"install", "upgrade"} {
		for _, refresh := range []string{"unobserved patch", "refreshed conditions", "observed patch"} {
			t.Run(action+"/"+refresh, func(t *testing.T) {
				retirementRunner(t)
				f := sourceWaitFixture(t)
				object(f.hr["metadata"])["generation"] = 2
				object(f.hr["status"])["observedGeneration"] = 2
				cs := field(f.hr, "status", "conditions").([]any)
				object(cs[0])["observedGeneration"], object(cs[1])["observedGeneration"] = 2, 2
				if action == "install" {
					object(cs[2])["reason"] = "InstallSucceeded"
				}
				object(cs[2])["message"] = fmt.Sprintf("Helm %s succeeded for release backstage/backstage.v4 with chart backstage@2.10.2", action)
				execute := func(c context.Context, p string, a ...string) ([]byte, error) {
					b, err := f.execute(c, p, a...)
					if p == "kubectl" && strings.Contains(strings.Join(a, " "), "patch helmrelease backstage") && err == nil {
						object(f.hr["metadata"])["generation"] = 3
						if refresh != "unobserved patch" {
							object(cs[0])["observedGeneration"], object(cs[1])["observedGeneration"] = 3, 3
						}
						if refresh == "observed patch" {
							object(f.hr["status"])["observedGeneration"] = 3
						}
						return encode(t, f.hr), nil
					}
					return b, err
				}
				var out bytes.Buffer
				if code := run([]string{"--execute"}, &out, execute); code != 0 || !f.deleted || f.historyStatus != "uninstalled" {
					t.Fatalf("pinned Flux source-wait transition rejected: code=%d %s", code, &out)
				}
			})
		}
	}
}

func TestHistoricalSuccessCannotNameAnotherReleaseOrUseUnjoinedGenerations(t *testing.T) {
	for _, mode := range []string{"future success", "future ready", "mixed prerequisites", "different revision", "different chart", "wrong action", "missing chart name", "missing chart version"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := sourceWaitFixture(t)
			cs := field(f.hr, "status", "conditions").([]any)
			switch mode {
			case "future success":
				object(cs[2])["observedGeneration"] = 2
			case "future ready":
				object(cs[0])["observedGeneration"] = 2
			case "mixed prerequisites":
				object(f.hr["metadata"])["generation"] = 2
				object(cs[0])["observedGeneration"] = 2
			case "different revision":
				object(cs[2])["message"] = "Helm upgrade succeeded for release backstage/backstage.v3 with chart backstage@2.10.2"
			case "different chart":
				object(cs[2])["message"] = "Helm upgrade succeeded for release backstage/backstage.v4 with chart backstage@2.10.1"
			case "wrong action":
				object(cs[2])["reason"] = "InstallSucceeded"
			case "missing chart name":
				delete(object(field(f.hr, "status", "history").([]any)[0]), "chartName")
			case "missing chart version":
				delete(object(field(f.hr, "status", "history").([]any)[0]), "chartVersion")
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted {
				t.Fatalf("unjoined source-wait evidence mutated resources: code=%d %s", code, &out)
			}
		})
	}
}

func TestControllerChartIdentityMustJoinTheStoredRelease(t *testing.T) {
	for _, sourceWait := range []bool{false, true} {
		for _, mode := range []string{"different name", "different version", "missing name", "missing version"} {
			t.Run(fmt.Sprintf("source_wait_%t/%s", sourceWait, mode), func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				if sourceWait {
					f = sourceWaitFixture(t)
				}
				history := object(field(f.hr, "status", "history").([]any)[0])
				switch mode {
				case "different name":
					history["chartName"] = "other"
				case "different version":
					history["chartVersion"] = "9.9.9"
				case "missing name":
					delete(history, "chartName")
				case "missing version":
					delete(history, "chartVersion")
				}
				if sourceWait {
					cs := field(f.hr, "status", "conditions").([]any)
					object(cs[2])["message"] = fmt.Sprintf("Helm upgrade succeeded for release backstage/backstage.v4 with chart %s@%s", text(history["chartName"]), text(history["chartVersion"]))
				}
				var out bytes.Buffer
				if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted {
					t.Fatalf("unjoined controller chart reached mutation: code=%d %s", code, &out)
				}
			})
		}
	}
}

func TestUnavailableSourceDoesNotPreventPreservingStatelessRetirement(t *testing.T) {
	retirementRunner(t)
	f := sourceWaitFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		b, err := f.execute(c, p, a...)
		if p == "kubectl" && strings.Contains(strings.Join(a, " "), "patch helmrelease backstage") && err == nil {
			// The options patch increments generation before the source-blocked
			// controller can observe it. Its deployed history remains unchanged.
			object(f.hr["metadata"])["generation"] = 2
			return encode(t, f.hr), nil
		}
		return b, err
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 0 || !f.deleted || !f.configured || f.historyStatus != "uninstalled" || f.preservationReads < 4 {
		t.Fatalf("source prerequisite wait blocked safe native retirement: code=%d %s", code, &out)
	}
}

func TestSourceWaitCannotImpersonateAQuiescentDeployedRelease(t *testing.T) {
	for _, mode := range []string{"missing ready", "unknown ready", "wrong ready reason", "stale ready", "duplicate ready", "active action", "wrong prerequisite message", "missing released", "failed released", "stale released", "failed history", "stalled", "missing chart status", "chart ready", "wrong chart reason", "stale chart", "duplicate chart ready"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := sourceWaitFixture(t)
			cs := field(f.hr, "status", "conditions").([]any)
			chartCs := field(f.chart, "status", "conditions").([]any)
			switch mode {
			case "missing ready":
				object(f.hr["status"])["conditions"] = cs[1:]
			case "unknown ready":
				object(cs[0])["status"] = "Unknown"
			case "wrong ready reason":
				object(cs[0])["reason"] = "UpgradeFailed"
			case "stale ready":
				object(cs[0])["observedGeneration"] = 0
			case "duplicate ready":
				object(f.hr["status"])["conditions"] = append(cs, cs[0])
			case "active action":
				object(cs[1])["reason"] = "ProgressingWithRetry"
			case "wrong prerequisite message":
				object(cs[1])["message"] = "Running upgrade"
			case "missing released":
				object(f.hr["status"])["conditions"] = cs[:2]
			case "failed released":
				object(cs[2])["status"] = "False"
			case "stale released":
				object(cs[2])["observedGeneration"] = 0
			case "failed history":
				object(field(f.hr, "status", "history").([]any)[0])["status"] = "failed"
			case "stalled":
				object(f.hr["status"])["conditions"] = append(cs, map[string]any{"type": "Stalled", "status": "True", "observedGeneration": 1})
			case "missing chart status":
				delete(f.chart, "status")
			case "chart ready":
				object(chartCs[0])["status"] = "True"
			case "wrong chart reason":
				object(chartCs[0])["reason"] = "FetchFailed"
			case "stale chart":
				object(f.chart["status"])["observedGeneration"] = 3
			case "duplicate chart ready":
				object(f.chart["status"])["conditions"] = append(chartCs, chartCs[0])
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted {
				t.Fatalf("ambiguous source-wait state reached mutation: code=%d %s", code, &out)
			}
		})
	}
}

func TestSourceWaitChangesAfterOptionsPatchPreventOwnerDeletion(t *testing.T) {
	for _, mode := range []string{"active action", "new generation", "chart recovered", "new release"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := sourceWaitFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				if f.configured && !f.deleted {
					switch mode {
					case "active action":
						object(field(f.hr, "status", "conditions").([]any)[1])["message"] = "Running upgrade"
					case "new generation":
						object(f.hr["metadata"])["generation"] = 3
					case "chart recovered":
						object(field(f.chart, "status", "conditions").([]any)[0])["status"] = "True"
					case "new release":
						object(field(f.hr, "status", "history").([]any)[0])["version"] = 5
					}
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || !f.configured || f.deleted || !strings.Contains(out.String(), "HOLD_MUTATION") {
				t.Fatalf("changed prerequisite state reached owner deletion: code=%d %s", code, &out)
			}
		})
	}
}
