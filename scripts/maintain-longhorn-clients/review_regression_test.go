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

func TestDeletedTemporaryComputeNodeDoesNotStrandRestoredJournal(t *testing.T) {
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
			n["metadata"].(object)["labels"] = object{}
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
			err = e.restore(ctx)
			if !replacement && err != nil {
				t.Fatalf("deleted temporary node stranded cleanup: %v", err)
			}
			if replacement && err == nil {
				t.Fatal("replacement node was adopted")
			}
		})
	}
}
