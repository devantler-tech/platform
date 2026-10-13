package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

type failedResultWriter struct{ short bool }

func (w failedResultWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestFailedResultOutputNeverReportsSuccess(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprintf("short_%t", short), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			if code := run([]string{"--execute"}, failedResultWriter{short: short}, f.execute); code != 2 || !f.deleted {
				t.Fatalf("unreported terminal result accepted: code=%d deleted=%t", code, f.deleted)
			}
		})
	}
}

// This catches a retirement that drops history/credentials, skips preservation,
// or deletes a newly-created object by name instead of the assessed identity.
func TestConfirmedStatelessRetirementPreservesDataAndCredentials(t *testing.T) {
	protectedRunner(t)
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("PORTAL_RETIREMENT_CONFIRM", "RETIRE BACKSTAGE STATELESS ONLY")
	t.Setenv("PORTAL_RETIREMENT_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
	f := retirementFixture(t)
	var output bytes.Buffer
	if code := run([]string{"--execute"}, &output, f.execute); code != 0 {
		t.Fatalf("confirmed stateless retirement failed: code=%d output=%s", code, &output)
	}
	if !f.deleted || !f.configured || f.historyStatus != "uninstalled" || f.preservationReads < 4 {
		t.Fatalf("retirement did not prove the lifecycle/preservation outcome: configured=%t deleted=%t history=%s reads=%d", f.configured, f.deleted, f.historyStatus, f.preservationReads)
	}
	if output.String() != "PORTAL-RETIREMENT=RETIRED_STATELESS objects=3 preservationSamples=2 dataRetirement=UNASSESSED\n" || strings.Contains(output.String(), "DO_NOT_PRINT") {
		t.Fatalf("unexpected or sensitive public output: %s", &output)
	}
}

func TestIncompleteOrUnsafeRetirementCannotWrite(t *testing.T) {
	for name, change := range map[string]func(*portalFixture){
		"credential in Helm footprint":   func(f *portalFixture) { addObservedResource(f.release, "Secret", "backstage-db-app") },
		"unhealthy retained database":    func(f *portalFixture) { object(item(f.live, 2)["status"])["readyInstances"] = 0 },
		"terminating retained namespace": func(f *portalFixture) { object(f.namespace["metadata"])["deletionTimestamp"] = "2026-10-13T00:00:00Z" },
		"unbound retained claim":         func(f *portalFixture) { object(item(f.live, 3)["status"])["phase"] = "Pending" },
		"terminating retained volume": func(f *portalFixture) {
			object(item(f.volumes, 0)["metadata"])["deletionTimestamp"] = "2026-10-13T00:00:00Z"
		},
		"malformed stored hooks": func(f *portalFixture) { f.release["hooks"] = map[string]any{"unexpected": true} },
		"malformed workload volumes": func(f *portalFixture) {
			f.release["manifest"] = strings.Replace(text(f.release["manifest"]), "      volumes:\n        - name: tmp\n          emptyDir: {}", "      volumes: invalid", 1)
		},
		"missing healthy owner conditions": func(f *portalFixture) { delete(object(f.hr["status"]), "conditions") },
		"missing controller history":       func(f *portalFixture) { delete(object(f.hr["status"]), "history") },
		"newer mismatched controller release": func(f *portalFixture) {
			object(f.hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "backstage", "version": 5, "status": "deployed"}}
		},
		"foreign latest controller release": func(f *portalFixture) {
			object(f.hr["status"])["history"] = []any{map[string]any{"name": "other", "namespace": "backstage", "version": 4, "status": "deployed"}}
		},
		"foreign latest controller namespace": func(f *portalFixture) {
			object(f.hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "other", "version": 4, "status": "deployed"}}
		},
		"undeployed latest controller snapshot": func(f *portalFixture) {
			object(f.hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "backstage", "version": 4, "status": "uninstalled"}}
		},
		"missing stored release history": func(f *portalFixture) { f.inventory["items"] = f.inventory["items"].([]any)[:1] },
		"different volume claim identity": func(f *portalFixture) {
			object(object(f.volumes["items"].([]any)[0])["spec"])["claimRef"] = map[string]any{"namespace": "backstage", "name": "backstage-db-3", "uid": "replaced-claim"}
		},
		"suspended owner":             func(f *portalFixture) { object(f.hr["spec"])["suspend"] = true },
		"missing uninstall finalizer": func(f *portalFixture) { object(f.hr["metadata"])["finalizers"] = []any{} },
		"active Helm action": func(f *portalFixture) {
			object(f.hr["status"])["conditions"] = []any{map[string]any{"type": "Reconciling", "status": "True"}}
		},
		"other namespace target": func(f *portalFixture) { object(f.hr["spec"])["targetNamespace"] = "other" },
		"foreign chart owner": func(f *portalFixture) {
			object(f.chart["metadata"])["labels"] = map[string]any{"helm.toolkit.fluxcd.io/name": "other", "helm.toolkit.fluxcd.io/namespace": "backstage"}
		},
		"foreign chart garbage-collection owner": func(f *portalFixture) {
			object(f.chart["metadata"])["ownerReferences"] = []any{map[string]any{"kind": "HelmRelease", "name": "other", "uid": "foreign-owner"}}
		},
		"alternate uninstall service account": func(f *portalFixture) { object(f.hr["spec"])["serviceAccountName"] = "other" },
		"non-characterized wait strategy":     func(f *portalFixture) { object(f.hr["spec"])["waitStrategy"] = "legacy" },
		"chart used by another release": func(f *portalFixture) {
			other := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": metadata("other", "other-uid"), "spec": map[string]any{"chartRef": map[string]any{"kind": "HelmChart", "name": "backstage-backstage", "namespace": "backstage"}}}
			f.otherHR = other
		},
		"source still owns release": func(f *portalFixture) {
			object(object(f.layers["items"].([]any)[0])["status"])["inventory"] = map[string]any{"entries": []any{map[string]any{"id": "backstage_backstage_helm.toolkit.fluxcd.io_HelmRelease"}}}
		},
		"source revision drift": func(f *portalFixture) {
			object(f.sources["status"])["artifact"] = map[string]any{"revision": "latest@sha256:" + strings.Repeat("c", 64)}
		},
		"incomplete inventory": func(f *portalFixture) { f.inventory["metadata"] = map[string]any{"continue": "next-page"} },
	} {
		t.Run(name, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			change(f)
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, f.execute) != 2 || !strings.Contains(out.String(), "HOLD_PRECONDITION") || f.configured || f.deleted || f.receiptCreated {
				t.Fatalf("unsafe prerequisite produced writes or a wrong verdict: %s", &out)
			}
		})
	}
}

func TestFailedAndChangedReadsNeverBecomeDeletionPermission(t *testing.T) {
	for _, mode := range []string{"failed partial read", "changed snapshot", "unknown current main"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			reads := 0
			execute := func(c context.Context, program string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if mode == "unknown current main" && program == "bash" {
					return []byte("PROD_RECOVERY_SOURCE=PASS"), fmt.Errorf("DO_NOT_PRINT")
				}
				if strings.Contains(joined, "get service,configmap,secret") {
					reads++
					if mode == "failed partial read" {
						b, _ := f.execute(c, program, args...)
						return b, fmt.Errorf("DO_NOT_PRINT")
					}
					if reads == 2 {
						object(f.inventory["items"].([]any)[0])["data"] = map[string]any{"password": "changed"}
					}
				}
				return f.execute(c, program, args...)
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_PRECONDITION") || strings.Contains(out.String(), "DO_NOT_PRINT") || f.configured || f.deleted || f.receiptCreated {
				t.Fatalf("failed/changed evidence authorized writes: %s", &out)
			}
		})
	}
}

func TestUnknownDeleteOutcomeIsNotRetriedOrDeclaredRetired(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	deletes := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if p == "kubectl" && strings.Contains(strings.Join(a, " "), " delete ") && !strings.Contains(strings.Join(a, " "), "?dryRun=All") {
			deletes++
			return []byte(`{"kind":"Status","status":"Success"}`), fmt.Errorf("DO_NOT_PRINT")
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || deletes != 1 || !strings.Contains(out.String(), "HOLD_MUTATION") || strings.Contains(out.String(), "DO_NOT_PRINT") {
		t.Fatalf("ambiguous delete was retried or accepted: deletes=%d %s", deletes, &out)
	}
}

func TestNormalFinalizerDelayAndOwnedChartCleanupConverge(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	f.pendingDeleteReads = 1
	f.chartRemaining = true
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, f.execute) != 0 || !f.chartDeleted || f.pendingDeleteReads != 0 {
		t.Fatalf("normal finalizer delay/chart cleanup did not converge: %s", &out)
	}
}

func TestMalformedDeleteResponseCannotAuthorizeMoreWrites(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	deletes := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if p == "kubectl" && strings.Contains(strings.Join(a, " "), " delete ") && !strings.Contains(strings.Join(a, " "), "?dryRun=All") {
			deletes++
			return []byte(`{}`), nil
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || deletes != 1 || !strings.Contains(out.String(), "HOLD_MUTATION") {
		t.Fatalf("ambiguous native delete response was accepted: %s", &out)
	}
}

func TestADataChangeAfterDeleteIsHoldNotRetirementAcceptance(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.deleted {
			object(f.inventory["items"].([]any)[0])["data"] = map[string]any{"password": "DO_NOT_PRINT_CHANGED"}
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") || strings.Contains(out.String(), "DO_NOT_PRINT") {
		t.Fatalf("changed retained data was accepted: %s", &out)
	}
}

func TestRecreatedFootprintAfterUninstallConfigurationCannotBeAdopted(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.configured && p == "helm" {
			object(object(object(f.release["info"])["resources"])["v1/Service"].([]any)[0])["metadata"].(map[string]any)["uid"] = "replacement-service"
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || f.deleted || !strings.Contains(out.String(), "HOLD_MUTATION") {
		t.Fatalf("a replacement footprint was adopted: %s", &out)
	}
}

func TestDeletedOrReplacedReleaseHistoryCannotPassPreservation(t *testing.T) {
	for _, mode := range []string{"deleted", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				if f.deleted {
					if mode == "deleted" {
						f.inventory["items"] = f.inventory["items"].([]any)[:1]
					} else {
						object(item(f.inventory, 1)["metadata"])["uid"] = "replacement-history"
					}
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") {
				t.Fatalf("lost retained history was accepted: %s", &out)
			}
		})
	}
}

func TestIncompleteReceiptCannotAuthorizeOwnerMutation(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if p == "kubectl" && strings.Contains(strings.Join(a, " "), " create ") {
			return []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"backstage-retirement-42","namespace":"flux-system","uid":"receipt-uid","resourceVersion":"1"}}`), nil
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || f.configured || f.deleted || !strings.Contains(out.String(), "HOLD_MUTATION") {
		t.Fatalf("incomplete receipt authorized deletion: %s", &out)
	}
}

func TestSameUIDOwnerStatusUpdateUsesFreshDeletePrecondition(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.configured && !f.deleted {
			object(f.hr["metadata"])["resourceVersion"] = "14"
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 0 {
		t.Fatalf("legitimate owner status update prevented guarded uninstall: %s", &out)
	}
}

func TestPartialNativeGCIsObservedUntilCapturedDescendantsDisappear(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	delayed := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.deleted && strings.Contains(strings.Join(a, " "), "get deployment,pod,persistentvolumeclaim") && delayed < 2 {
			delayed++
			list := map[string]any{"apiVersion": "v1", "kind": "List", "items": f.live["items"]}
			return encode(t, list), nil
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 0 || delayed != 2 {
		t.Fatalf("normal delayed native GC was not observed: %s", &out)
	}
}

func TestForgedTargetPodLabelsDoNotHideUnrelatedWorkload(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	object(item(f.live, 1)["metadata"])["ownerReferences"] = []any{map[string]any{"kind": "ReplicaSet", "name": "other", "uid": "foreign-rs"}}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, f.execute) != 2 || f.receiptCreated {
		t.Fatalf("foreign pod hidden by target labels: %s", &out)
	}
}

func TestChartFinalizerDelayConvergesWithoutReplay(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	f.chartRemaining = true
	delayed := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.chartDeleted && strings.Contains(strings.Join(a, " "), "get helmchart") && delayed < 2 {
			delayed++
			object(f.chart["metadata"])["deletionTimestamp"] = "2026-10-13T00:00:00Z"
			return encode(t, f.chart), nil
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 0 || delayed != 2 {
		t.Fatalf("normal chart finalizer delay did not converge: %s", &out)
	}
}

func TestPartialOrReplacedWorkloadCannotBecomeRetirementAcceptance(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(c, p, a...)
				if f.deleted && strings.Contains(strings.Join(a, " "), "get deployment,pod,persistentvolumeclaim") {
					var list map[string]any
					if json.Unmarshal(b, &list) != nil {
						return nil, fmt.Errorf("bad fixture")
					}
					o := item(f.live, 0)
					if replacement {
						object(o["metadata"])["uid"] = "replacement-deployment"
					}
					list["items"] = append(list["items"].([]any), o)
					b = encode(t, list)
				}
				return b, err
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") || f.chartDeleted {
				t.Fatalf("partial/replaced live footprint was accepted: %s", &out)
			}
		})
	}
}

func TestStoredReleaseContentCannotDriftAfterUninstall(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		b, err := f.execute(c, p, a...)
		if f.deleted && p == "helm" {
			var r map[string]any
			if json.Unmarshal(b, &r) != nil {
				return nil, fmt.Errorf("bad fixture")
			}
			r["config"] = map[string]any{"marker": "CHANGED_PRIVATE"}
			b = encode(t, r)
		}
		return b, err
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") {
		t.Fatalf("retained release content drift was accepted: %s", &out)
	}
}

func TestRetainedDeletionProtectionCannotDisappearDuringUninstall(t *testing.T) {
	for _, kind := range []string{"Namespace", "Cluster", "PersistentVolumeClaim", "PersistentVolume", "HelmHistory"} {
		t.Run(kind, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			var retained map[string]any
			switch kind {
			case "Namespace":
				retained = f.namespace
			case "Cluster":
				retained = item(f.live, 2)
			case "PersistentVolumeClaim":
				retained = item(f.live, 3)
			case "PersistentVolume":
				retained = item(f.volumes, 0)
			case "HelmHistory":
				retained = item(f.inventory, 1)
			}
			object(retained["metadata"])["finalizers"] = []any{"test.invalid/deletion-protection"}
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				if f.deleted {
					if kind == "Namespace" {
						retained = f.namespace
					}
					delete(object(retained["metadata"]), "finalizers")
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, execute) != 2 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") || f.chartDeleted {
				t.Fatalf("retained deletion protection loss was accepted: %s", &out)
			}
		})
	}
}

func TestControllerLatestHistoryUsesHighestVersionNotInputOrder(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	object(f.hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "backstage", "version": 3, "status": "superseded"}, map[string]any{"name": "backstage", "namespace": "backstage", "version": 4, "status": "deployed", "chartName": "backstage", "chartVersion": "2.10.2"}}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, f.execute) != 0 {
		t.Fatalf("valid latest controller history was not joined by highest version: %s", &out)
	}
}

func TestControllerHistoryDriftAfterConfigurationCannotAuthorizeDelete(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if f.configured {
			object(f.hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "backstage", "version": 5, "status": "deployed"}}
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if run([]string{"--execute"}, &out, execute) != 2 || f.deleted || !strings.Contains(out.String(), "HOLD_MUTATION") {
		t.Fatalf("controller history drift authorized owner deletion: %s", &out)
	}
}

func retirementRunner(t *testing.T) {
	t.Helper()
	protectedRunner(t)
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("PORTAL_RETIREMENT_CONFIRM", "RETIRE BACKSTAGE STATELESS ONLY")
	t.Setenv("PORTAL_RETIREMENT_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
}

type portalFixture struct {
	t                                                                        *testing.T
	release, live, inventory, namespace, volumes, hr, chart, sources, layers map[string]any
	configured, deleted                                                      bool
	historyStatus                                                            string
	preservationReads                                                        int
	otherHR                                                                  map[string]any
	receiptCreated                                                           bool
	pendingDeleteReads                                                       int
	chartRemaining, chartDeleted                                             bool
	fenceInstalled                                                           bool
}

func retirementFixture(t *testing.T) *portalFixture {
	t.Helper()
	r, l := fixture()
	addObservedResource(r, "ConfigMap", "backstage-app-config")
	for _, values := range object(object(r["info"])["resources"]) {
		for _, raw := range values.([]any) {
			o := object(raw)
			if o["kind"] != "PodList" {
				object(o["metadata"])["annotations"] = map[string]any{"meta.helm.sh/release-name": "backstage", "meta.helm.sh/release-namespace": "backstage"}
			}
		}
	}
	db := item(l, 2)
	db["spec"] = map[string]any{"instances": 1, "bootstrap": map[string]any{"initdb": map[string]any{"database": "app", "owner": "app"}}}
	db["status"] = map[string]any{"readyInstances": 1, "currentPrimary": "backstage-db-3", "targetPrimary": "backstage-db-3"}
	claim := item(l, 3)
	claim["spec"] = map[string]any{"volumeName": "retained-volume"}
	claim["status"] = map[string]any{"phase": "Bound"}
	object(claim["metadata"])["annotations"] = map[string]any{"kustomize.toolkit.fluxcd.io/prune": "disabled"}
	object(claim["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "database-uid", "controller": true}}
	object(claim["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "primary", "cnpg.io/instanceRole": "primary"}
	dbPod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata("backstage-db-3", "database-pod-uid"), "spec": map[string]any{"containers": []any{map[string]any{"name": "postgres"}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	object(dbPod["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "primary", "cnpg.io/instanceRole": "primary"}
	object(dbPod["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "database-uid", "controller": true}}
	l["items"] = append(l["items"].([]any), dbPod)
	list := func(values ...any) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "List", "items": values}
	}
	ns := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata("backstage", "namespace-uid")}
	delete(object(ns["metadata"]), "namespace")
	object(ns["metadata"])["annotations"] = map[string]any{"kustomize.toolkit.fluxcd.io/prune": "disabled"}
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/basic-auth", "metadata": metadata("backstage-db-app", "credential-uid"), "data": map[string]any{"username": "SYNTHETIC_USERNAME", "password": "DO_NOT_PRINT"}}
	object(secret["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "database-uid", "controller": true}}
	object(secret["metadata"])["labels"] = map[string]any{"app.kubernetes.io/managed-by": "cloudnative-pg", "cnpg.io/cluster": "backstage-db", "cnpg.io/reload": "true", "cnpg.io/userType": "app"}
	object(secret["metadata"])["annotations"] = map[string]any{"cnpg.io/operatorVersion": "1.30.1"}
	history := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata("sh.helm.release.v1.backstage.v4", "history-uid"), "type": "helm.sh/release.v1", "data": map[string]any{"release": "SYNTHETIC_DO_NOT_PRINT"}}
	history["data"] = nativeReleaseSecret(t, r)["data"]
	object(history["metadata"])["labels"] = map[string]any{"owner": "helm", "name": "backstage", "version": "4", "status": "deployed"}
	rs := map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": metadata("backstage-rs", "rs-uid")}
	object(rs["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "backstage", "uid": "deployment-uid"}}
	object(item(l, 1)["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "backstage-rs", "uid": "rs-uid"}}
	pv := map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": metadata("retained-volume", "volume-uid"), "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain", "claimRef": map[string]any{"namespace": "backstage", "name": "backstage-db-3", "uid": "claim-uid"}}, "status": map[string]any{"phase": "Bound"}}
	delete(object(pv["metadata"]), "namespace")
	hr := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": metadata("backstage", "release-uid"), "spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": "backstage", "version": "2.10.2", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "backstage"}}}}, "status": map[string]any{"storageNamespace": "backstage", "helmChart": "backstage/backstage-backstage"}}
	object(hr["metadata"])["generation"] = 1
	object(hr["metadata"])["finalizers"] = []any{"finalizers.fluxcd.io"}
	object(hr["status"])["observedGeneration"] = 1
	object(hr["status"])["history"] = []any{map[string]any{"name": "backstage", "namespace": "backstage", "version": 4, "status": "deployed", "chartName": "backstage", "chartVersion": "2.10.2"}}
	object(hr["status"])["conditions"] = []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": 1}, map[string]any{"type": "Released", "status": "True", "observedGeneration": 1}}
	chart := map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "HelmChart", "metadata": metadata("backstage-backstage", "chart-uid"), "spec": map[string]any{"chart": "backstage", "version": "2.10.2", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "backstage"}}}
	object(chart["metadata"])["labels"] = map[string]any{"helm.toolkit.fluxcd.io/name": "backstage", "helm.toolkit.fluxcd.io/namespace": "backstage"}
	ready := func(kind, name, uid string) map[string]any {
		o := map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": kind, "metadata": metadata(name, uid), "status": map[string]any{"observedGeneration": 1, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": 1}}}}
		object(o["metadata"])["namespace"] = "flux-system"
		object(o["metadata"])["generation"] = 1
		return o
	}
	revision := "latest@sha256:" + strings.Repeat("b", 64)
	source := ready("OCIRepository", "flux-system", "source-uid")
	object(source["status"])["artifact"] = map[string]any{"revision": revision}
	layer := ready("Kustomization", "apps", "layer-uid")
	layer["apiVersion"] = "kustomize.toolkit.fluxcd.io/v1"
	layer["spec"] = map[string]any{"sourceRef": map[string]any{"kind": "OCIRepository", "name": "flux-system"}}
	object(layer["status"])["lastAppliedRevision"] = revision
	object(layer["status"])["inventory"] = map[string]any{"entries": []any{map[string]any{"id": "_backstage__Namespace"}}}
	service := object(field(r, "info", "resources", "v1/Service").([]any)[0])
	config := object(field(r, "info", "resources", "v1/ConfigMap").([]any)[0])
	return &portalFixture{t: t, release: r, live: l, inventory: list(secret, history, rs, service, config), namespace: ns, volumes: list(pv), hr: hr, chart: chart, sources: source, layers: list(layer), historyStatus: "deployed"}
}

func (f *portalFixture) execute(_ context.Context, program string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if program == "bash" {
		return []byte("PROD_RECOVERY_SOURCE=PASS\n"), nil
	}
	if program == "go" {
		return []byte("PORTAL-FOOTPRINT=ASSESSED_STATELESS objects=3 dataRetirement=UNASSESSED mutation=NONE\n"), nil
	}
	if program == "sleep" {
		return nil, nil
	}
	if program == "helm" {
		if f.deleted {
			status := f.historyStatus
			if f.pendingDeleteReads > 0 {
				status = "uninstalling"
			}
			var r map[string]any
			_ = json.Unmarshal(encode(f.t, f.release), &r)
			r["info"] = map[string]any{"status": status}
			for _, raw := range array(f.inventory["items"]) {
				h := object(raw)
				if field(h, "metadata", "name") == "sh.helm.release.v1.backstage.v4" {
					h["data"] = nativeReleaseSecret(f.t, r)["data"]
					object(field(h, "metadata", "labels"))["status"] = status
				}
			}
			return encode(f.t, r), nil
		}
		return encode(f.t, f.release), nil
	}
	if program != "kubectl" {
		return nil, fmt.Errorf("unexpected program")
	}
	if strings.Contains(joined, "get clusterpolicy restrict-backstage-retirement") {
		spec, err := expectedFenceSpec()
		if err != nil {
			return nil, err
		}
		return encode(f.t, map[string]any{"apiVersion": "kyverno.io/v1", "kind": "ClusterPolicy", "metadata": map[string]any{"name": "restrict-backstage-retirement", "uid": "policy-uid", "resourceVersion": "1", "generation": 1}, "spec": spec, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}), nil
	}
	if strings.Contains(joined, "get deployment helm-controller") {
		return encode(f.t, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "helm-controller", "namespace": "flux-system", "uid": "controller-uid", "resourceVersion": "1", "generation": 1}, "spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "helm-controller", "containers": []any{map[string]any{"name": "manager", "image": nativeControllerImage}}}}}, "status": map[string]any{"observedGeneration": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}}), nil
	}
	if strings.Contains(joined, "get serviceaccount cloudnative-pg") {
		return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "cnpg-controller-uid", "resourceVersion": "1"}}), nil
	}
	if strings.Contains(joined, "get serviceaccount helm-controller") {
		return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "helm-controller", "namespace": "flux-system", "uid": "helm-writer-uid", "resourceVersion": "1"}}), nil
	}
	if strings.Contains(joined, "get configmap cnpg-controller-manager-config") {
		return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cnpg-controller-manager-config", "namespace": "cnpg-system", "uid": "cnpg-config-uid", "resourceVersion": "1"}}), nil
	}
	if strings.Contains(joined, "get deployment cloudnative-pg") {
		return encode(f.t, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "cnpg-deployment-uid", "resourceVersion": "1", "generation": 1}, "spec": map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "cloudnative-pg", "containers": []any{map[string]any{"image": cnpgControllerImage, "env": []any{map[string]any{"name": "MONITORING_QUERIES_CONFIGMAP", "value": "cnpg-default-monitoring"}}}}}}}, "status": map[string]any{"observedGeneration": 1, "readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2}}), nil
	}
	if strings.Contains(joined, " patch ") {
		path := ""
		for _, a := range args {
			if strings.HasPrefix(a, "--patch-file=") {
				path = strings.TrimPrefix(a, "--patch-file=")
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var ops []map[string]any
		if strings.Contains(joined, "patch namespace backstage") {
			if json.Unmarshal(data, &ops) != nil || len(ops) != 3 || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "namespace-uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "12" || ops[2]["path"] != "/metadata/annotations/platform.devantler.tech~1backstage-retirement" || strings.Contains(text(ops[2]["value"]), "DO_NOT_PRINT") {
				return nil, fmt.Errorf("fence lost namespace identity or disclosed data")
			}
			copy, err := decode(encode(f.t, f.namespace))
			if err != nil {
				return nil, err
			}
			f.namespace = copy
			object(field(f.namespace, "metadata", "annotations"))[fenceAnnotation] = ops[2]["value"]
			object(f.namespace["metadata"])["resourceVersion"] = "13"
			f.fenceInstalled = true
			return encode(f.t, f.namespace), nil
		}
		if json.Unmarshal(data, &ops) != nil || len(ops) != 3 || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "release-uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "12" || ops[2]["path"] != "/spec/uninstall" {
			return nil, fmt.Errorf("patch lost the assessed identity")
		}
		opts := object(ops[2]["value"])
		if opts["keepHistory"] != true || opts["disableHooks"] != true || opts["disableWait"] != false {
			return nil, fmt.Errorf("unsafe uninstall")
		}
		f.configured = true
		object(f.hr["spec"])["uninstall"] = opts
		object(f.hr["metadata"])["resourceVersion"] = "13"
		return encode(f.t, f.hr), nil
	}
	if strings.Contains(joined, " delete ") {
		if strings.Contains(joined, "?dryRun=All") {
			if !f.fenceInstalled {
				return nil, fmt.Errorf("dry-run needs an installed fence")
			}
			rule := "preserve-retained-identities"
			if strings.Contains(joined, "/api/v1/namespaces/backstage?dryRun=All") {
				rule = "preserve-retirement-journal"
			} else if strings.Contains(joined, "/deployments/backstage?dryRun=All") || strings.Contains(joined, "/services/backstage?dryRun=All") || strings.Contains(joined, "/configmaps/backstage-app-config?dryRun=All") || strings.Contains(joined, "/helmreleases/backstage?dryRun=All") || strings.Contains(joined, "/helmcharts/backstage-backstage?dryRun=All") {
				return []byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`), nil
			}
			return []byte("admission webhook \"validate.kyverno.svc-fail\" denied the request:\nrestrict-backstage-retirement:\n  " + rule + ": retained"), fmt.Errorf("denied")
		}
		chart := strings.Contains(joined, "/helmcharts/backstage-backstage")
		if !f.fenceInstalled || !f.configured || !chart && !strings.Contains(joined, "/helmreleases/backstage") {
			return nil, fmt.Errorf("unexpected delete target")
		}
		path := args[len(args)-1]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var options map[string]any
		uid, rv := "release-uid", text(field(f.hr, "metadata", "resourceVersion"))
		if chart {
			uid, rv = "chart-uid", "12"
		}
		if json.Unmarshal(data, &options) != nil || object(options["preconditions"])["uid"] != uid || object(options["preconditions"])["resourceVersion"] != rv {
			return nil, fmt.Errorf("delete lost concurrency guards")
		}
		f.deleted = true
		if chart {
			f.chartDeleted = true
			f.chartRemaining = false
		}
		f.historyStatus = "uninstalled"
		return []byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`), nil
	}
	if strings.Contains(joined, " create ") {
		if strings.Contains(joined, "--dry-run=server") {
			return []byte("admission webhook \"validate.kyverno.svc-fail\" denied the request:\nrestrict-backstage-retirement:\n  fence-chart-consumers: exclusive"), fmt.Errorf("denied")
		}
		f.receiptCreated = true
		data, err := os.ReadFile(args[len(args)-2])
		if err != nil {
			return nil, err
		}
		var receipt map[string]any
		if json.Unmarshal(data, &receipt) != nil {
			return nil, fmt.Errorf("malformed receipt")
		}
		object(receipt["metadata"])["uid"] = "receipt-uid"
		object(receipt["metadata"])["resourceVersion"] = "1"
		return encode(f.t, receipt), nil
	}
	if strings.Contains(joined, "backstage-retirement-42") {
		return nil, nil
	}
	if strings.Contains(joined, "get namespace backstage") {
		return encode(f.t, f.namespace), nil
	}
	if strings.Contains(joined, "get persistentvolumes") {
		return encode(f.t, f.volumes), nil
	}
	if strings.Contains(joined, "get kustomizations") {
		return encode(f.t, f.layers), nil
	}
	if strings.Contains(joined, "get ocirepository flux-system") {
		return encode(f.t, f.sources), nil
	}
	if strings.Contains(joined, "get helmreleases") {
		if f.deleted {
			if f.pendingDeleteReads > 0 {
				f.pendingDeleteReads--
				data := encode(f.t, f.hr)
				var hr map[string]any
				if err := json.Unmarshal(data, &hr); err != nil {
					f.t.Fatal("invalid fixture encoding")
				}
				object(hr["metadata"])["deletionTimestamp"] = "2026-10-13T00:00:00Z"
				return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{hr}}), nil
			}
			return []byte(`{"apiVersion":"v1","kind":"List","items":[]}`), nil
		}
		items := []any{f.hr}
		if f.otherHR != nil {
			items = append(items, f.otherHR)
		}
		return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "List", "items": items}), nil
	}
	if strings.Contains(joined, "get helmchart") {
		if f.deleted && !f.chartRemaining {
			return nil, nil
		}
		return encode(f.t, f.chart), nil
	}
	if strings.Contains(joined, "get deployment,pod,persistentvolumeclaim") {
		if f.deleted {
			return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "List", "items": f.live["items"].([]any)[2:]}), nil
		}
		return encode(f.t, f.live), nil
	}
	if strings.Contains(joined, "get service,configmap,secret") {
		f.preservationReads++
		if f.deleted {
			items := []any{}
			for _, r := range f.inventory["items"].([]any) {
				if object(r)["kind"] != "ReplicaSet" && !targetResource(object(r)) {
					items = append(items, r)
				}
			}
			return encode(f.t, map[string]any{"apiVersion": "v1", "kind": "List", "items": items}), nil
		}
		return encode(f.t, f.inventory), nil
	}
	return nil, fmt.Errorf("unexpected reader: %s", joined)
}
