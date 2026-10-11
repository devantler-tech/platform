package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestPauseAndRestoreRetryOnlyConflictsWithFreshOwnership(t *testing.T) {
	for _, phase := range []string{"pause", "restore"} {
		for _, failure := range []string{"status-conflict", "foreign-spec", "forbidden", "persistent-conflict"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				a := fakeFixture()
				e := testEngine(a)
				e.publicationNotStarted = true // The fake publisher has never run.
				if phase == "restore" {
					if err := e.prepare(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				attempts := 0
				e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
					if args[0] == "patch" && args[1] == "clusters.postgresql.cnpg.io" && args[2] == "backstage-db" {
						attempts++
						if failure == "forbidden" {
							return nil, errors.New("Error from server (Forbidden): denied")
						}
						if attempts <= 2 || failure == "persistent-conflict" {
							c := a.objects[fakeKey(args[1], "backstage", args[2])][0]
							fakeSet(c, "/metadata/resourceVersion", fmt.Sprint(20+attempts), false)
							if failure == "foreign-spec" {
								fakeSet(c, "/spec/instances", float64(9), false)
							}
							return nil, errors.New("Error from server (Conflict): the object has been modified")
						}
					}
					return a.command(ctx, input, args...)
				}
				var err error
				if phase == "pause" {
					err = e.prepare(context.Background())
				} else {
					err = e.restore(context.Background())
				}
				if failure == "status-conflict" && (err != nil || attempts != 3) {
					t.Fatalf("status conflict not recovered: attempts=%d err=%v", attempts, err)
				}
				if failure != "status-conflict" && err == nil {
					t.Fatal("unsafe retry accepted")
				}
				if (failure == "foreign-spec" || failure == "forbidden") && attempts != 1 {
					t.Fatalf("unrelated failure retried %d times", attempts)
				}
				if attempts > 5 {
					t.Fatalf("retry was unbounded: %d", attempts)
				}
			})
		}
	}
}

func TestRestoredJournalCanBeSupersededOnlyAfterLiveReleaseProof(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			e.publicationNotStarted = true // The first run failed before publication.
			if err := e.prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := e.restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			if drift {
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/metadata/uid", "replacement", false)
			}
			e.holder = "101.1"
			err := e.prepare(context.Background())
			if drift && err == nil {
				t.Fatal("replaced consumer allowed journal supersession")
			}
			if !drift && err != nil {
				t.Fatalf("fully restored run permanently blocked the next run: %v", err)
			}
			if !drift {
				j, _, err := e.readJournal(context.Background())
				if err != nil || j.Holder != e.holder {
					t.Fatalf("journal owner did not change under CAS: %v", err)
				}
			}
		})
	}
}

func TestRestoredJournalAllowsUnownedConsumerUpgrade(t *testing.T) {
	for _, drift := range []string{"chart-values", "database-image", "still-suspended", "still-scaled-down", "foreign-owner", "replaced", "hibernate", "reconcile"} {
		t.Run(drift, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			e.publicationNotStarted = true
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.restore(ctx); err != nil {
				t.Fatal(err)
			}
			hr := a.objects[fakeKey("helmrelease", "kubescape", "kubescape")][0]
			switch drift {
			case "chart-values":
				fakeSet(hr, "/spec/values/nodeAgent/image/tag", "new-reviewed-release", false)
			case "database-image":
				fakeSet(a.objects[fakeKey("clusters.postgresql.cnpg.io", "backstage", "backstage-db")][0], "/spec/imageName", "new-reviewed-image", false)
			case "still-suspended":
				fakeSet(hr, "/spec/suspend", true, false)
			case "still-scaled-down":
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/spec/replicas", float64(0), false)
			case "foreign-owner":
				fakeSet(hr, "/metadata/annotations/"+escape(ownerKey), "foreign", false)
			case "replaced":
				fakeSet(hr, "/metadata/uid", "replacement", false)
			case "hibernate":
				fakeSet(a.objects[fakeKey("clusters.postgresql.cnpg.io", "backstage", "backstage-db")][0], "/metadata/annotations/"+escape(hibernateKey), "on", false)
			case "reconcile":
				fakeSet(hr, "/metadata/annotations/"+escape(reconcileKey), "disabled", false)
			}
			allowed := drift == "chart-values" || drift == "database-image"
			if err := e.legacyPublicationSafe(ctx); (err == nil) != allowed {
				t.Fatalf("legacy release proof for %s: %v", drift, err)
			}
			e.holder = "101.1"
			writes := a.writes
			if err := e.prepare(ctx); (err == nil) != allowed {
				t.Fatalf("fresh-plan supersession for %s: %v", drift, err)
			}
			if !allowed && a.writes != writes {
				t.Fatal("unsafe supersession wrote resources")
			}
		})
	}
}

func TestProductionMaintenanceCheckoutContainsPublicationBase(t *testing.T) {
	checked := 0
	for _, path := range []string{"../../.github/workflows/ci.yaml", "../../.github/workflows/cd.yaml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var workflow object
		if err = yaml.Unmarshal(b, &workflow); err != nil {
			t.Fatal(err)
		}
		for name, raw := range val(workflow, "jobs").(map[string]any) {
			job := raw.(map[string]any)
			var checkout object
			for _, step := range items(job, "steps") {
				if strings.HasPrefix(str(step, "uses"), "actions/checkout@") {
					checkout = step
				}
				if str(step, "uses") != "./.github/actions/deploy-prod" {
					continue
				}
				checked++
				if val(checkout, "with", "fetch-depth") == nil || num(checkout, "with", "fetch-depth") != 0 {
					t.Errorf("%s job %s deploys without the exact publication base available", path, name)
				}
			}
		}
	}
	if checked != 3 {
		t.Fatalf("expected merge-group, manual and recovery deployment paths, checked %d", checked)
	}
}

func TestCompletedMigrationPollsTransientClientRollout(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.afterPublish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.verify(ctx); err != nil {
		t.Fatal(err)
	}
	reads := 0
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "get" && args[1] == "daemonset" && args[2] == "longhorn-manager" {
			reads++
			d := a.objects[fakeKey("daemonset", "longhorn-system", "longhorn-manager")][0]
			ready := float64(3)
			if reads < 3 {
				ready = 2
			}
			fakeSet(d, "/status/numberReady", ready, false)
		}
		return a.command(ctx, input, args...)
	}
	if err := e.prepare(ctx); err != nil {
		t.Fatalf("temporary client rollout blocked a completed migration: %v", err)
	}
	if reads != 3 {
		t.Fatalf("expected bounded readiness polling, got %d reads", reads)
	}
}

func TestLegacySourceBeforePublicationRefusesUnsafeSelectorRollback(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "maintenance")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	chart := filepath.Join(root, "k8s/providers/hetzner/infrastructure/controllers/longhorn")
	if err := os.MkdirAll(chart, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "helm-release.yaml"), []byte("spec:\n  chart:\n    spec:\n      version: 1.13.0\n  values:\n    defaultSettings:\n      systemManagedComponentsNodeSelector: node.longhorn.io/create-default-disk:true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := "#!/bin/sh\ncase \"$*\" in *'get settings.longhorn.io'*) printf '{\"value\":\"%s\"}' \"$LIVE_SELECTOR\";; *'get configmaps'*) printf '%s' \"$LIVE_JOURNALS\";; *) exit 43;; esac\n"
	if err := os.WriteFile(filepath.Join(root, "kubectl"), []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		selector, journals string
		allowed            bool
	}{
		{restrictedSelector, `{"items":[],"metadata":{}}`, true},
		{"", `{"items":[],"metadata":{}}`, false},
		{"unknown-selector", `{"items":[],"metadata":{}}`, false},
		{restrictedSelector, `{"items":[{"metadata":{"name":"` + journalName + `"}}],"metadata":{}}`, false},
	} {
		cmd := exec.Command(binary, "before-publish")
		cmd.Dir = root
		cmd.Env = []string{"PATH=" + root, "LIVE_SELECTOR=" + tc.selector, "LIVE_JOURNALS=" + tc.journals, "GITHUB_ACTIONS=true", "GITHUB_REPOSITORY=devantler-tech/platform", "GITHUB_SHA=" + strings.Repeat("a", 40), "GITHUB_RUN_ID=100", "GITHUB_RUN_ATTEMPT=1"}
		out, err := cmd.CombinedOutput()
		if tc.allowed && err != nil {
			t.Fatalf("unchanged legacy source rejected: %v %s", err, out)
		}
		if !tc.allowed && err == nil {
			t.Fatalf("legacy publication could reverse live or started migration (%q)", tc.selector)
		}
	}
}

func TestEmptySelectorRequiresAuditedChartVersion(t *testing.T) {
	for _, version := range []string{"1.13.0", "1.13.1", "", "1.14.0"} {
		t.Run(version, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := "k8s/providers/hetzner/infrastructure/controllers/longhorn"
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf("spec:\n  chart:\n    spec:\n      version: %q\n  values:\n    defaultSettings:\n      systemManagedComponentsNodeSelector: \"\"\n      createDefaultDiskLabeledNodes: true\n    longhornManager:\n      nodeSelector: {}\n", version)
			if err := os.WriteFile(filepath.Join(path, "helm-release.yaml"), []byte(manifest), 0600); err != nil {
				t.Fatal(err)
			}
			active, err := armed()
			if version == "1.13.0" && (!active || err != nil) {
				t.Fatalf("audited version rejected: active=%v err=%v", active, err)
			}
			if version != "1.13.0" && err == nil {
				t.Fatal("unaudited empty-selector publication accepted")
			}
		})
	}
}

func TestClientReadinessExcludesOnlyDeclaredRunnerOnlyNodes(t *testing.T) {
	for _, variation := range []string{"runner-only", "no-taint", "wrong-value", "wrong-effect", "wrong-pool", "no-label", "unhealthy"} {
		t.Run(variation, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.afterPublish(ctx); err != nil {
				t.Fatal(err)
			}
			n := resource("Node", "", "autoscale-arc-runners-test")
			n["metadata"].(object)["labels"] = object{"platform.devantler.tech/ci-runner": "enabled"}
			taint := object{"key": "platform.devantler.tech/ci-runner", "value": "enabled", "effect": "NoSchedule"}
			n["spec"] = object{"taints": []any{taint}}
			n["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
			switch variation {
			case "no-taint":
				n["spec"] = object{}
			case "wrong-value":
				taint["value"] = "other"
			case "wrong-effect":
				taint["effect"] = "PreferNoSchedule"
			case "wrong-pool":
				n["metadata"].(object)["name"] = "autoscale-cx43-test"
			case "no-label":
				n["metadata"].(object)["labels"] = object{}
			case "unhealthy":
				n["status"] = object{}
			}
			a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], n)
			ready, err := e.clientsReady(ctx)
			if variation == "runner-only" && (!ready || err != nil) {
				t.Fatalf("intentionally runner-only node blocked clients: ready=%v err=%v", ready, err)
			}
			if variation != "runner-only" && ready {
				t.Fatal("ordinary or unhealthy worker escaped client proof")
			}
		})
	}
}

func TestFailureCleanupKeepsConsumersPausedDuringClientTransition(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	// The controller accepted the new selector but has not finished its clients.
	a.objects[fakeKey("settings.longhorn.io", "longhorn-system", "system-managed-components-node-selector")][0]["value"] = ""
	if err := e.restore(ctx); err == nil {
		t.Fatal("cleanup resumed consumers during an unresolved client transition")
	}
	if !a.stopped() {
		t.Fatal("failure cleanup resumed storage consumers before safe rollout proof")
	}
}

func TestUnsupportedTemporaryNodeJournalRefusesDeletedOrReplacedTargets(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.afterPublish(ctx); err != nil {
				t.Fatal(err)
			}
			j, o, err := e.readJournal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := resource("Node", "", "autoscale-retired")
			n["spec"] = object{}
			n["metadata"].(object)["labels"] = object{"ksail.io/autoscaled": "true"}
			j.Nodes = []object{n}
			if err = e.writeJournal(ctx, j, o); err != nil {
				t.Fatal(err)
			}
			if replacement {
				x := clone(n)
				fakeSet(x, "/metadata/uid", "replacement", false)
				a.objects[fakeKey("node", "", "autoscale-retired")] = []object{x}
			}
			e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				if !replacement && args[0] == "get" && args[1] == "node" {
					return nil, errors.New("Error from server (NotFound): nodes not found")
				}
				return a.command(ctx, input, args...)
			}
			writes := a.writes
			err = e.restore(ctx)
			if err == nil || a.writes != writes {
				t.Fatalf("unsupported node cleanup was adopted: writes=%d err=%v", a.writes-writes, err)
			}
		})
	}
}

func TestHealthyMaintenanceNeverBorrowsComputeClientLabels(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			a := fakeFixture()
			n := resource("Node", "", "autoscale-test")
			n["metadata"].(object)["labels"] = object{"ksail.io/autoscaled": "true"}
			n["spec"] = object{}
			n["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
			a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], n)
			a.objects[fakeKey("node", "", "autoscale-test")] = []object{n}
			e := testEngine(a)
			e.publicationNotStarted = true
			ctx := context.Background()
			nodeWrites := 0
			e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				if args[0] == "patch" && (args[1] == "node" || args[1] == "nodes") {
					nodeWrites++
				}
				return a.command(ctx, input, args...)
			}
			if cancelled {
				a.failWrite = 3 // Cancel during a partial consumer pause.
			}
			err := e.prepare(ctx)
			if cancelled != (err != nil) {
				t.Fatalf("unexpected preparation result: %v", err)
			}
			if val(n, "metadata", "labels", "node.longhorn.io/create-default-disk") != nil || str(n, "metadata", "annotations", ownerKey) != "" {
				t.Fatal("maintenance borrowed a compute client's label")
			}
			a.failWrite = 0
			if err = e.restore(ctx); err != nil {
				t.Fatal(err)
			}
			if nodeWrites != 0 {
				t.Fatalf("maintenance performed %d Node writes", nodeWrites)
			}
		})
	}
}

func TestTemporaryClientJournalCannotRemoveAnInUseClient(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	e.publicationNotStarted = true
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	j, o, err := e.readJournal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := resource("Node", "", "autoscale-test")
	n["metadata"].(object)["labels"] = object{"ksail.io/autoscaled": "true"}
	j.Nodes = []object{clone(n)}
	current := clone(n)
	fakeSet(current, "/metadata/labels/node.longhorn.io~1create-default-disk", "true", false)
	fakeSet(current, "/metadata/annotations/"+escape(ownerKey), e.holder, false)
	a.objects[fakeKey("node", "", "autoscale-test")] = []object{current}
	j.Phase, j.PublicationNotStarted = "prepared", true
	if err = e.writeJournal(ctx, j, o); err != nil {
		t.Fatal(err)
	}
	fakeSet(a.objects[fakeKey("pods", "*", "")][0], "/spec/nodeName", "autoscale-test", false)
	fakeSet(a.objects[fakeKey("volumes.longhorn.io", "longhorn-system", "")][0], "/spec/nodeID", "autoscale-test", false)
	fakeSet(a.objects[fakeKey("volumes.longhorn.io", "longhorn-system", "")][0], "/status/state", "attached", false)
	writes := a.writes
	if err = e.restore(ctx); err == nil || a.writes != writes {
		t.Fatalf("unsupported temporary-client journal reached cleanup: writes=%d err=%v", a.writes-writes, err)
	}
	if str(current, "metadata", "labels", "node.longhorn.io/create-default-disk") != "true" {
		t.Fatal("cleanup removed an in-use client's label")
	}
}
