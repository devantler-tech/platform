package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func admissionFixture(a *fakeAPI) {
	w := resource("MutatingWebhookConfiguration", "", "kyverno-resource-mutating-webhook-cfg")
	w["webhooks"] = []any{object{"failurePolicy": "Fail", "rules": []any{object{"apiGroups": []any{"longhorn.io"}, "apiVersions": []any{"v1beta2"}, "resources": []any{"nodes"}, "operations": []any{"CREATE", "UPDATE"}}}}}
	a.objects[fakeKey("mutatingwebhookconfiguration", "", "kyverno-resource-mutating-webhook-cfg")] = []object{w}
}

func TestAdmissionAndExistingComputeExclusionBeforeAnyPause(t *testing.T) {
	for _, drift := range []string{"fail-open-zero-pool", "dry-run-zero-pool", "already-labeled-compute"} {
		t.Run(drift, func(t *testing.T) {
			a := fakeFixture()
			admissionFixture(a)
			e := testEngine(a)
			switch drift {
			case "fail-open-zero-pool":
				fakeSet(a.objects[fakeKey("mutatingwebhookconfiguration", "", "kyverno-resource-mutating-webhook-cfg")][0], "/webhooks", []any{}, false)
			case "dry-run-zero-pool":
				e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
					if args[0] == "create" && args[1] == "--dry-run=server" {
						return input, nil // Admission did not exclude storage.
					}
					return a.command(ctx, input, args...)
				}
			case "already-labeled-compute":
				n := resource("Node", "", "autoscale-test")
				n["metadata"].(object)["labels"] = object{"node.longhorn.io/create-default-disk": "true", "ksail.io/autoscaled": "true"}
				n["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
				a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], n)
				lh := resource("Node", "longhorn-system", "autoscale-test")
				lh["spec"] = object{"allowScheduling": true, "evictionRequested": false}
				a.objects[fakeKey("nodes.longhorn.io", "longhorn-system", "")] = []object{lh}
			}
			if err := e.prepare(context.Background()); err == nil || a.writes != 0 {
				t.Fatalf("unproven storage exclusion reached a pause: writes=%d err=%v", a.writes, err)
			}
		})
	}
}

func TestReleasedInterruptedJournalDoesNotRequireManualDeletion(t *testing.T) {
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
	j, _, err := e.readJournal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j.Phase = "detached" // The final journal update was lost, not the restoration.
	b, _ := json.Marshal(j)
	fakeSet(a.objects[fakeKey("configmap", journalNS, journalName)][0], "/data/journal.json", string(b), false)
	if err = e.prepare(ctx); err != nil {
		t.Fatalf("same owning run cannot recover fully released interrupted journal: %v", err)
	}
}

func TestTerminalInterruptedHolderCanResumeOnlyOriginalPausedIdentities(t *testing.T) {
	for _, state := range []string{"terminal", "running", "unknown", "foreign-spec", "foreign-owner", "replaced-storage", "resized-storage"} {
		t.Run(state, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			e.holder = "101.1"
			e.previousFinished = func(context.Context, journal) error {
				if state == "running" || state == "unknown" {
					return errors.New(state)
				}
				return nil
			}
			switch state {
			case "foreign-spec":
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/spec/template", object{"drift": true}, false)
			case "foreign-owner":
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/metadata/annotations/"+escape(ownerKey), "foreign", false)
			case "replaced-storage":
				fakeSet(a.objects[fakeKey("persistentvolumeclaims", "*", "")][0], "/metadata/uid", "replacement", false)
			case "resized-storage":
				fakeSet(a.objects[fakeKey("persistentvolumeclaims", "*", "")][0], "/spec/resources/requests/storage", "2Gi", false)
			}
			writes := a.writes
			err := e.prepare(ctx)
			if state != "terminal" {
				if err == nil || a.writes != writes {
					t.Fatalf("unsafe adoption wrote resources: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = e.afterPublish(ctx); err != nil {
				t.Fatal(err)
			}
			if err = e.verify(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyRecoveryRequiresPositiveUnstartedPublicationAndLiveRelease(t *testing.T) {
	for _, variation := range []string{"released", "might-have-published", "still-paused", "replaced"} {
		t.Run(variation, func(t *testing.T) {
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
			j, o, err := e.readJournal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if variation == "might-have-published" {
				j.PublicationNotStarted = false
				if err = e.writeJournal(ctx, j, o); err != nil {
					t.Fatal(err)
				}
			}
			if variation == "still-paused" {
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/spec/replicas", float64(0), false)
			}
			if variation == "replaced" {
				fakeSet(a.objects[fakeKey("persistentvolumeclaims", "*", "")][0], "/metadata/uid", "replacement", false)
			}
			err = e.legacyPublicationSafe(ctx)
			if (variation == "released") != (err == nil) {
				t.Fatalf("unexpected legacy safety verdict: %v", err)
			}
		})
	}
}

func TestMigrationRejectsCombinedConsumerChangeBeforeAnyPause(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	e.revisionSafe = func(context.Context) error { return errors.New("combined consumer change") }
	if err := e.prepare(context.Background()); err == nil || a.writes != 0 {
		t.Fatalf("combined revision paused services: %v", err)
	}
}

func TestUnverifiedMigratedJournalStillRejectsCombinedConsumerChange(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.afterPublish(ctx); err != nil {
		t.Fatal(err)
	}
	e.holder = "101.1"
	e.revisionSafe = func(context.Context) error { return errors.New("combined consumer change") }
	writes := a.writes
	if err := e.prepare(ctx); err == nil || a.writes != writes {
		t.Fatalf("unverified migration accepted consumer upgrade: %v", err)
	}
}

func TestFinishedAttemptEvidenceIsExactAndFailClosed(t *testing.T) {
	j := journal{Holder: "100.2", Source: strings.Repeat("a", 40)}
	for _, tc := range []struct {
		name, body string
		allowed    bool
	}{
		{"completed", fmt.Sprintf(`{"id":100,"run_attempt":2,"head_sha":%q,"status":"completed"}`, j.Source), true},
		{"running", fmt.Sprintf(`{"id":100,"run_attempt":2,"head_sha":%q,"status":"in_progress"}`, j.Source), false},
		{"wrong-attempt", fmt.Sprintf(`{"id":100,"run_attempt":1,"head_sha":%q,"status":"completed"}`, j.Source), false},
		{"wrong-run", fmt.Sprintf(`{"id":101,"run_attempt":2,"head_sha":%q,"status":"completed"}`, j.Source), false},
		{"wrong-source", `{"id":100,"run_attempt":2,"head_sha":"other","status":"completed"}`, false},
		{"missing", `{}`, false}, {"malformed", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (verifyFinishedAttempt(j, strings.NewReader(tc.body)) == nil) != tc.allowed {
				t.Fatal("unbound liveness evidence accepted")
			}
		})
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if finishedAttempt(context.Background(), j) == nil {
		t.Fatal("missing liveness credential accepted")
	}
}

func TestIsolatedRevisionBindsCheckoutBaseAndProductionChangeInventory(t *testing.T) {
	for _, variation := range []string{"selector-only", "consumer-change", "base-missing", "wrong-head", "wrong-checkout", "manual", "manual-accumulated-change"} {
		t.Run(variation, func(t *testing.T) {
			t.Chdir(t.TempDir())
			git := func(args ...string) string {
				t.Helper()
				c := exec.Command("git", args...)
				c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
				b, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("fixture git: %v %s", err, b)
				}
				return strings.TrimSpace(string(b))
			}
			git("init", "-q")
			path := "k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml"
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "--", path)
			git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture base")
			base := git("rev-parse", "HEAD")
			if variation == "manual-accumulated-change" {
				other := "k8s/consumer.yaml"
				if err := os.WriteFile(other, []byte("undeployed consumer change"), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", "--", other)
				git("-c", "commit.gpgsign=false", "commit", "-qm", "undeployed consumer")
			}
			if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "--", path)
			if variation == "consumer-change" {
				other := "k8s/consumer.yaml"
				if err := os.WriteFile(other, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", "--", other)
			}
			git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture candidate")
			source := git("rev-parse", "HEAD")
			head := source
			if variation == "base-missing" {
				base = strings.Repeat("b", 40)
			}
			if variation == "wrong-head" {
				head = strings.Repeat("b", 40)
			}
			if variation == "wrong-checkout" {
				source = strings.Repeat("b", 40)
			}
			event := fmt.Sprintf(`{"merge_group":{"base_sha":%q,"head_sha":%q}}`, base, head)
			if err := os.WriteFile("event.json", []byte(event), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GITHUB_EVENT_PATH", "event.json")
			t.Setenv("GITHUB_EVENT_NAME", "merge_group")
			if strings.HasPrefix(variation, "manual") {
				t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
			}
			err := isolatedRevision(context.Background(), source)
			allowed := variation == "selector-only"
			if (err == nil) != allowed {
				t.Fatalf("unexpected revision safety verdict: %v", err)
			}
		})
	}
}

func TestJournalCannotRedirectRecoveryOutsideRecordedScope(t *testing.T) {
	for _, field := range []string{"empty-clusters", "foreign-cluster", "foreign-deployment", "unknown-holder", "unknown-source"} {
		t.Run(field, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			j, _, err := e.readJournal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "empty-clusters":
				j.Clusters = nil
			case "foreign-cluster":
				fakeSet(j.Clusters[0], "/metadata/namespace", "foreign", false)
			case "foreign-deployment":
				fakeSet(j.Deployment, "/metadata/name", "foreign", false)
			case "unknown-holder":
				j.ResourceHolder = "foreign"
			case "unknown-source":
				j.Source = "unknown"
			}
			b, _ := json.Marshal(j)
			fakeSet(a.objects[fakeKey("configmap", journalNS, journalName)][0], "/data/journal.json", string(b), false)
			if _, _, err = e.readJournal(ctx); err == nil {
				t.Fatal("malformed journal could redirect recovery")
			}
		})
	}
}
