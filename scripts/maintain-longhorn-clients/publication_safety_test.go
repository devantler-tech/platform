package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlanDoesNotInventWALArchivingForUnconfiguredDatabase(t *testing.T) {
	s := fixture()
	s.clusters[0]["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}}
	if _, err := buildPlan(s, time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("healthy database without a WAL archiver was rejected: %v", err)
	}
}

func scheduledFixture(a *fakeAPI) []object {
	var schedules []object
	for _, c := range a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")] {
		if len(items(c, "spec", "plugins")) == 0 {
			continue
		}
		o := resource("ScheduledBackup", str(c, "metadata", "namespace"), str(c, "metadata", "name")+"-daily")
		o["spec"] = object{"cluster": object{"name": str(c, "metadata", "name")}, "method": "plugin", "schedule": "0 0 3 * * *"}
		schedules = append(schedules, o)
		a.objects[fakeKey("scheduledbackups.postgresql.cnpg.io", str(o, "metadata", "namespace"), str(o, "metadata", "name"))] = []object{o}
	}
	a.objects[fakeKey("scheduledbackups.postgresql.cnpg.io", "*", "")] = schedules
	return schedules
}

func controllerFixture(a *fakeAPI) {
	for _, name := range []string{"kustomize-controller", "helm-controller"} {
		d := resource("Deployment", "flux-system", name)
		d["metadata"].(object)["generation"] = 1
		d["spec"] = object{"replicas": 1, "template": object{"metadata": object{"annotations": object{}}}}
		d["status"] = object{"observedGeneration": 1, "readyReplicas": 1, "updatedReplicas": 1}
		a.objects[fakeKey("deployment", "flux-system", name)] = []object{d}
		rs := resource("ReplicaSet", "flux-system", name+"-rs")
		rs["metadata"].(object)["ownerReferences"] = []any{object{"kind": "Deployment", "uid": str(d, "metadata", "uid"), "controller": true}}
		a.objects[fakeKey("replicasets", "flux-system", "")] = append(a.objects[fakeKey("replicasets", "flux-system", "")], rs)
		p := resource("Pod", "flux-system", name+"-old")
		p["metadata"].(object)["labels"] = object{"app": name}
		p["metadata"].(object)["ownerReferences"] = []any{object{"kind": "ReplicaSet", "uid": str(rs, "metadata", "uid"), "controller": true}}
		p["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
		a.objects[fakeKey("pods", "flux-system", "")] = append(a.objects[fakeKey("pods", "flux-system", "")], p)
	}
}

func fakeControllerRollout(a *fakeAPI, d object) {
	d["metadata"].(object)["generation"] = num(d, "metadata", "generation") + 1
	d["status"].(object)["observedGeneration"] = num(d, "metadata", "generation")
	for _, p := range a.objects[fakeKey("pods", "flux-system", "")] {
		if str(p, "metadata", "labels", "app") == str(d, "metadata", "name") {
			p["metadata"].(object)["uid"] = str(p, "metadata", "uid") + "-replacement"
			p["metadata"].(object)["annotations"] = object{quiesceKey: str(d, "spec", "template", "metadata", "annotations", quiesceKey)}
		}
	}
}

func TestPrepareSuspendsSchedulesBeforeHibernationAndRestoresThem(t *testing.T) {
	a := fakeFixture()
	schedules := scheduledFixture(a)
	e := testEngine(a)
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "patch" && args[1] == "clusters.postgresql.cnpg.io" {
			for _, s := range schedules {
				if val(s, "spec", "suspend") != true && !a.migrated {
					return nil, errors.New("hibernation raced an unsuspended scheduled backup")
				}
			}
		}
		return a.command(ctx, input, args...)
	}
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.afterPublish(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range schedules {
		if !unpaused(s) || val(s, "spec", "suspend") != nil {
			t.Fatal("original scheduled backup was not restored")
		}
	}
}

func TestDetachmentRejectsAReleaseWhosePauseWasRemoved(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, _, err := e.ownerJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.objects[fakeKey("helmrelease", "crossview", "crossview")][0]["spec"].(object)["suspend"] = false
	if ok, err := e.detached(context.Background(), j); err == nil || ok {
		t.Fatal("detachment accepted a lost owner pause")
	}
}

func TestDetachmentRejectsLostBackupReconcilerPause(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, _, err := e.ownerJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.objects[fakeKey("helmrelease", "velero", "velero")][0]["spec"].(object)["suspend"] = false
	if ok, err := e.detached(context.Background(), j); err == nil || ok {
		t.Fatal("publication accepted an unpaused backup reconciler")
	}
}

func TestPrepareRetiresPrePauseFluxProcessesBeforeHibernation(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	restarted := map[string]bool{}
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "patch" && args[1] == "deployment" && strings.HasSuffix(args[2], "-controller") {
			restarted[args[2]] = true
		}
		if args[0] == "patch" && args[1] == "clusters.postgresql.cnpg.io" && (!restarted["kustomize-controller"] || !restarted["helm-controller"]) {
			return nil, errors.New("pre-pause reconcilers can still overwrite consumers")
		}
		return a.command(ctx, input, args...)
	}
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationRejectsFailedLastDatabaseBackup(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.afterPublish(ctx); err != nil {
		t.Fatal(err)
	}
	c := a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")][1]
	c["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "False"}}
	if err := e.verify(ctx); err == nil {
		t.Fatal("verification accepted a failed database backup")
	}
}

func TestTerminatingOldReconcilerStillBlocksHibernation(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		var old object
		if args[0] == "patch" && args[1] == "deployment" && args[2] == "kustomize-controller" {
			old = clone(a.objects[fakeKey("pods", "flux-system", "")][0])
			old["metadata"].(object)["deletionTimestamp"] = "2026-10-10T23:00:00Z"
		}
		out, err := a.command(ctx, input, args...)
		if old != nil && err == nil {
			a.objects[fakeKey("pods", "flux-system", "")] = append(a.objects[fakeKey("pods", "flux-system", "")], old)
		}
		return out, err
	}
	if err := e.prepare(context.Background()); err == nil || a.stopped() {
		t.Fatal("a still-terminating old process allowed database hibernation")
	}
}

func TestConcurrentControllerUpgradeBlocksHibernation(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		out, err := a.command(ctx, input, args...)
		if err == nil && args[0] == "patch" && args[1] == "deployment" && args[2] == "kustomize-controller" {
			d := a.objects[fakeKey("deployment", "flux-system", "kustomize-controller")][0]
			d["spec"].(object)["template"].(object)["spec"] = object{"containers": []any{object{"image": "concurrent-upgrade"}}}
		}
		return out, err
	}
	if err := e.prepare(context.Background()); err == nil || a.stopped() {
		t.Fatal("maintenance adopted an unreviewed concurrent controller upgrade")
	}
}

func TestNewScheduledBackupAtPublicationBoundaryIsHold(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, _, err := e.ownerJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newSchedule := clone(a.objects[fakeKey("scheduledbackups.postgresql.cnpg.io", "*", "")][0])
	newSchedule["metadata"].(object)["name"] = "new-schedule"
	newSchedule["metadata"].(object)["uid"] = "new-schedule-uid"
	a.objects[fakeKey("scheduledbackups.postgresql.cnpg.io", "*", "")] = append(a.objects[fakeKey("scheduledbackups.postgresql.cnpg.io", "*", "")], newSchedule)
	if ok, err := e.detached(context.Background(), j); err == nil || ok {
		t.Fatal("publication accepted a schedule not covered by the journal")
	}
}

func TestActivePlatformBackupBlocksEveryMaintenanceWrite(t *testing.T) {
	a := fakeFixture()
	b := resource("Backup", "velero", "active-full-backup")
	b["status"] = object{"phase": "InProgress"}
	a.objects[fakeKey("backups.velero.io", "velero", "")] = []object{b}
	e := testEngine(a)
	if err := e.prepare(context.Background()); err == nil || a.writes != 0 {
		t.Fatalf("active full backup allowed maintenance writes: writes=%d err=%v", a.writes, err)
	}
}

func TestPlatformBackupScheduleIsPausedAndRestored(t *testing.T) {
	a := fakeFixture()
	s := resource("Schedule", "velero", "velero-daily-full")
	s["apiVersion"] = "velero.io/v1"
	s["spec"] = object{"schedule": "0 2 * * *", "template": object{"includedNamespaces": []any{"*"}}}
	a.objects[fakeKey("schedules.velero.io", "velero", "")] = []object{s}
	a.objects[fakeKey("schedules.velero.io", "velero", "velero-daily-full")] = []object{s}
	e := testEngine(a)
	r := a.objects[fakeKey("helmrelease", "velero", "velero")][0]
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "patch" && args[1] == "schedules.velero.io" && !a.migrated && val(r, "spec", "suspend") != true {
			return nil, errors.New("Helm can still unpause the full-platform backup schedule")
		}
		if args[0] == "patch" && args[1] == "clusters.postgresql.cnpg.io" && !a.migrated && val(s, "spec", "paused") != true {
			return nil, errors.New("full-platform backup can still start during hibernation")
		}
		return a.command(ctx, input, args...)
	}
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.afterPublish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if val(s, "spec", "paused") != nil || !unpaused(s) {
		t.Fatal("original platform backup schedule was not restored")
	}
	if val(r, "spec", "suspend") != false || !unpaused(r) {
		t.Fatal("original backup Helm release was not restored")
	}
}

func TestUnprovenBackupHelmOwnershipBlocksEveryMaintenanceWrite(t *testing.T) {
	for _, change := range []string{"missing", "foreign pause", "foreign layer", "foreign owner"} {
		t.Run(change, func(t *testing.T) {
			a := fakeFixture()
			r := a.objects[fakeKey("helmrelease", "velero", "velero")][0]
			switch change {
			case "missing":
				delete(a.objects, fakeKey("helmrelease", "velero", "velero"))
			case "foreign pause":
				r["spec"].(object)["suspend"] = true
			case "foreign layer":
				r["metadata"].(object)["labels"].(object)["kustomize.toolkit.fluxcd.io/name"] = "apps"
			case "foreign owner":
				r["metadata"].(object)["annotations"].(object)[ownerKey] = "other.1"
			}
			if err := testEngine(a).prepare(context.Background()); err == nil || a.writes != 0 {
				t.Fatalf("unknown backup reconciler allowed maintenance writes: writes=%d err=%v", a.writes, err)
			}
		})
	}
}

func TestRetriedHolderStillRetiresEveryPreviousReconciler(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	ctx := context.Background()
	if err := e.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	e.publicationNotStarted = true
	if err := e.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.prepare(ctx); err != nil {
		t.Fatalf("repeated holder did not initiate a fresh controller retirement: %v", err)
	}
}

func TestBackupFailureAfterSuspensionBlocksHibernation(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	e.c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "get" && args[1] == "backups.postgresql.cnpg.io" && val(a.objects[fakeKey("helmrelease", "crossview", "crossview")][0], "spec", "suspend") == true {
			c := a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")][1]
			c["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "False"}}
		}
		return a.command(ctx, input, args...)
	}
	if err := e.prepare(context.Background()); err == nil || a.stopped() {
		t.Fatal("backup failure during suspension allowed database hibernation")
	}
}
