package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

type cnpgResumeFixture struct {
	c                client
	cm               object
	record           cnpgProofRecord
	runs             map[string]object
	currentSource    string
	patches, creates int
	namespace        object
}

func TestDatabaseResumeValidatesTheRetainedBarrier(t *testing.T) {
	for _, scenario := range []string{"system identity", "pre-backup target", "unarchived target"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCNPGResumeFixture(t, "restore-planned")
			plan := f.record.Plan
			if scenario == "system identity" {
				plan.Barrier.SystemID = "7777777777777777777"
			}
			if scenario == "pre-backup target" {
				plan.Barrier.FlushLSN = "0/1"
			}
			image := text(plan.Resources[3], "spec", "imageName")
			_, digest, _ := strings.Cut(image, "@")
			resources, err := cnpgRecoveryResources(f.record.Run, plan.Source, "ghcr.io/cloudnative-pg/postgresql@"+digest, plan.Barrier, "prod-worker-3")
			if err != nil {
				t.Fatal(err)
			}
			plan.Resources = resources
			body, _ := encodeCNPGProofRecord(f.record)
			f.cm["data"].(object)["record"] = body
			delegate := f.c.run
			f.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				raw, err := delegate(ctx, input, program, args...)
				if scenario == "unarchived target" && program == "kubectl" && len(args) > 2 && args[2] == "exec" {
					var result object
					if json.Unmarshal(raw, &result) == nil && value(result, "archive_done") != nil {
						result["archive_done"] = false
						raw, _ = json.Marshal(result)
					}
				}
				return raw, err
			}
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			if _, err := claimCNPGProofResume(ctx, f.c, "umami-db", f.currentSource, "5678"); err == nil || f.patches != 0 || f.creates != 0 {
				t.Fatal("unverified retained recovery target acquired an executor")
			}
		})
	}
}

func TestDatabaseResumeRefreshesWorkflowAdmissionAfterQualification(t *testing.T) {
	for _, scenario := range []string{"executor finishes", "original retries"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCNPGResumeFixture(t, "restore-planned")
			delegate := f.c.run
			f.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					if scenario == "executor finishes" {
						f.runs["5678"]["status"] = "completed"
					} else {
						f.runs["1234"]["status"], f.runs["1234"]["run_attempt"] = "in_progress", 2
					}
				}
				return delegate(ctx, input, program, args...)
			}
			if _, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "5678"); err == nil || f.patches != 0 || f.creates != 0 {
				t.Fatal("outdated workflow admission reached a claim write")
			}
		})
	}
}

func newCNPGResumeFixture(t *testing.T, phase string) *cnpgResumeFixture {
	t.Helper()
	c, request, plan, _ := archiveWaitFixture(t, "")
	image := text(plan.Resources[3], "spec", "imageName")
	_, digest, _ := strings.Cut(image, "@")
	resources, err := cnpgRecoveryResources(request.Run, plan.Source, "ghcr.io/cloudnative-pg/postgresql@"+digest, plan.Barrier, "prod-worker-3")
	if err != nil {
		t.Fatal(err)
	}
	plan.Resources = resources
	f := &cnpgResumeFixture{currentSource: strings.Repeat("b", 40), record: cnpgProofRecord{Version: 1, Source: request.Source, Run: request.Run, Target: request.Target, Phase: phase, Archive: request.Archive, Backup: request.Backup, Resources: map[string]benchmarkResource{}}}
	if phase == "restore-planned" {
		f.record.Plan = &plan
	}
	f.cm = ownedCNPGJournalFixture(t, f.record)
	meta := func(id int64, source, status string) object {
		return object{"id": id, "head_sha": source, "head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "status": status, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"}
	}
	f.runs = map[string]object{"1234": meta(1234, request.Source, "completed"), "5678": meta(5678, f.currentSource, "in_progress"), "6789": meta(6789, f.currentSource, "in_progress")}
	delegate := c.run
	c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(f.currentSource + "\trefs/heads/main\n"), nil
		}
		if program == "gh" && len(args) == 2 && args[0] == "api" {
			const prefix = "repos/devantler-tech/platform/actions/runs/"
			if !strings.HasPrefix(args[1], prefix) {
				return nil, errors.New("unexpected workflow owner read")
			}
			id := strings.TrimPrefix(args[1], prefix)
			if strings.HasSuffix(id, "/attempts/1") {
				id = strings.TrimSuffix(id, "/attempts/1")
				first := object{}
				for key, v := range f.runs[id] {
					first[key] = v
				}
				first["status"], first["run_attempt"] = "completed", 1
				return json.Marshal(first)
			}
			if f.runs[id] == nil {
				return nil, errors.New("unrecorded workflow owner")
			}
			return json.Marshal(f.runs[id])
		}
		if program == "kubectl" && len(args) > 3 {
			switch args[2] {
			case "get":
				if args[3] == "configmap" {
					return json.Marshal(f.cm)
				}
				if args[3] == "namespace" && f.namespace != nil {
					return json.Marshal(f.namespace)
				}
			case "patch":
				f.patches++
				var ops []object
				if args[3] != "configmap" || json.Unmarshal(input, &ops) != nil || len(ops) != 3 || text(ops[0], "path") != "/metadata/uid" || ops[0]["value"] != text(f.cm, "metadata", "uid") || text(ops[1], "path") != "/metadata/resourceVersion" || ops[1]["value"] != text(f.cm, "metadata", "resourceVersion") || text(ops[2], "path") != "/data/record" {
					return nil, errors.New("executor claim lost its original journal CAS")
				}
				f.cm["data"].(object)["record"] = text(ops[2], "value")
				f.cm["metadata"].(object)["resourceVersion"] = strconv.Itoa(f.patches + 1)
				return []byte("patched"), nil
			case "create":
				f.creates++
				var ns object
				if json.Unmarshal(input, &ns) != nil || text(ns, "kind") != "Namespace" || text(ns, "metadata", "name") != "longhorn-db-proof-umami-db-1234" || text(ns, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != "1234" {
					return nil, errors.New("continuation replaced the original resource identity")
				}
				ns["metadata"].(object)["uid"] = "88888888-8888-4888-8888-888888888888"
				ns["metadata"].(object)["resourceVersion"] = "10"
				f.namespace = ns
				return json.Marshal(ns)
			}
		}
		raw, err := delegate(ctx, input, program, args...)
		if err == nil && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "backups.postgresql.cnpg.io" {
			var backup object
			if json.Unmarshal(raw, &backup) != nil {
				t.Fatal("invalid native backup fixture")
			}
			backup["spec"].(object)["target"] = "primary"
			return json.Marshal(backup)
		}
		return raw, err
	}
	f.c = c
	return f
}

func TestDatabaseResumeKeepsCreationIdentityUnderTheCurrentExecutor(t *testing.T) {
	for _, phase := range []string{"backup-created", "restore-planned"} {
		t.Run(phase, func(t *testing.T) {
			f := newCNPGResumeFixture(t, phase)
			p, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", strings.Repeat("b", 40), "5678")
			if err != nil {
				t.Fatal("completed checkpoint could not continue:", err)
			}
			if p.record.Source != strings.Repeat("a", 40) || p.record.Run != "1234" || p.record.Phase != phase || p.record.Backup != f.record.Backup || p.record.Archive != f.record.Archive || f.patches != 1 || f.creates != 0 {
				t.Fatal("claim rewrote original evidence or advanced the checkpoint")
			}
			if phase == "backup-created" {
				if err := p.prepareRecovery(t.Context(), "prod-worker-3"); err != nil {
					t.Fatal("continuation used the obsolete creation revision:", err)
				}
			}
			if _, err := p.createEnvironmentResource(t.Context(), "Namespace"); err != nil {
				t.Fatal("current executor could not create the original planned namespace:", err)
			}
			var persisted object
			if json.Unmarshal([]byte(text(f.cm, "data", "record")), &persisted) != nil || text(persisted, "Source") != strings.Repeat("a", 40) || text(persisted, "Run") != "1234" || text(persisted, "Executor", "Source") != strings.Repeat("b", 40) || text(persisted, "Executor", "Run") != "5678" || text(persisted, "Executor", "JournalUID") != text(f.cm, "metadata", "uid") || f.creates != 1 {
				t.Fatal("continued write lost separate execution and creation ownership")
			}
		})
	}
}

func TestDatabaseResumeRejectsLiveOwnersAndUnsupportedCheckpoints(t *testing.T) {
	for _, scenario := range []string{"live original", "live retry", "foreign original source", "retried executor", "completed executor", "foreign executor source", "main advanced", "uncertain checkpoint", "allocated resources", "changed archive", "changed backup", "cleaned", "changed plan"} {
		t.Run(scenario, func(t *testing.T) {
			phase := "backup-created"
			if scenario == "changed plan" {
				phase = "restore-planned"
			}
			f := newCNPGResumeFixture(t, phase)
			switch scenario {
			case "live original":
				f.runs["1234"]["status"] = "in_progress"
			case "live retry":
				f.runs["1234"]["status"], f.runs["1234"]["run_attempt"] = "in_progress", 2
			case "foreign original source":
				f.runs["1234"]["head_sha"] = strings.Repeat("c", 40)
			case "retried executor":
				f.runs["5678"]["run_attempt"] = 2
			case "completed executor":
				f.runs["5678"]["status"] = "completed"
			case "foreign executor source":
				f.runs["5678"]["head_sha"] = strings.Repeat("c", 40)
			case "main advanced":
				f.currentSource = strings.Repeat("c", 40)
			case "uncertain checkpoint":
				f.record.Phase = "backup-requested"
			case "allocated resources":
				f.record.Resources["Namespace"] = benchmarkResource{Name: "prior", UID: "prior-uid"}
			case "changed archive":
				f.record.Archive.StoreUID = "replacement"
			case "changed backup":
				f.record.Backup.UID = "replacement"
			case "cleaned":
				f.record.Cleaned = true
			case "changed plan":
				f.record.Plan.Resources[3]["spec"].(object)["enableSuperuserAccess"] = true
			}
			body, err := encodeCNPGProofRecord(f.record)
			if err != nil {
				t.Fatal(err)
			}
			f.cm["data"].(object)["record"] = body
			if _, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", strings.Repeat("b", 40), "5678"); err == nil || f.patches != 0 || f.creates != 0 {
				t.Fatalf("unqualified continuation reached a write: err=%v patches=%d creates=%d", err, f.patches, f.creates)
			}
		})
	}
}

func TestDatabaseResumeFencesAReplacedExecutorAtTheSameSource(t *testing.T) {
	f := newCNPGResumeFixture(t, "restore-planned")
	first, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "5678")
	if err != nil {
		t.Fatal(err)
	}
	f.runs["5678"]["status"] = "completed"
	second, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "6789")
	if err != nil {
		t.Fatal("terminal executor could not be replaced:", err)
	}
	before := f.patches
	if _, err = first.createEnvironmentResource(t.Context(), "Namespace"); err == nil || f.patches != before || f.creates != 0 {
		t.Fatal("superseded executor changed the journal or created a namespace")
	}
	if _, err = first.createOwned(t.Context(), second.record.Plan.Resources[0]); err == nil || f.creates != 0 {
		t.Fatal("superseded executor crossed the adjacent create fence")
	}
	if _, err = second.createEnvironmentResource(t.Context(), "Namespace"); err != nil || f.creates != 1 {
		t.Fatal("current replacement executor could not advance:", err)
	}
}

func TestDatabaseResumeRechecksItsOwnerAndIdempotentClaim(t *testing.T) {
	for _, scenario := range []string{"same owner", "finished owner", "live displaced owner", "retried displaced owner", "unadmitted executor"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCNPGResumeFixture(t, "restore-planned")
			p, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "5678")
			if err != nil {
				t.Fatal(err)
			}
			before := f.patches
			switch scenario {
			case "same owner":
				if _, err = claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "5678"); err != nil || f.patches != before {
					t.Fatal("qualified idempotent claim rewrote its checkpoint:", err)
				}
				return
			case "finished owner":
				f.runs["5678"]["status"] = "completed"
			case "live displaced owner":
				_, err = claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "6789")
			case "retried displaced owner":
				f.runs["5678"]["run_attempt"] = 2
				_, err = claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "6789")
			case "unadmitted executor":
				p.executing = cnpgProofExecutor{}
			}
			if scenario == "live displaced owner" || scenario == "retried displaced owner" {
				if err == nil || f.patches != before || f.creates != 0 {
					t.Fatal("live displaced executor was replaced")
				}
				return
			}
			if _, err = p.createEnvironmentResource(t.Context(), "Namespace"); err == nil || f.patches != before || f.creates != 0 {
				t.Fatal("unqualified execution reached a journal or resource write")
			}
		})
	}
}

func TestDatabaseResumePreservesUncertainClaimWrites(t *testing.T) {
	for _, scenario := range []string{"concurrent checkpoint", "CAS conflict", "lost response", "replaced journal", "changed readback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCNPGResumeFixture(t, "restore-planned")
			delegate := f.c.run
			reads, attempts := 0, 0
			f.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 && args[3] == "configmap" {
					if args[2] == "get" {
						reads++
						if reads == 2 && scenario == "concurrent checkpoint" {
							f.cm["metadata"].(object)["resourceVersion"] = "replaced-version"
						}
						if reads == 3 && scenario == "replaced journal" {
							f.cm["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
						}
						if reads == 3 && scenario == "changed readback" {
							f.record.Phase = "backup-requested"
							body, _ := encodeCNPGProofRecord(f.record)
							f.cm["data"].(object)["record"] = body
						}
					}
					if args[2] == "patch" {
						attempts++
						if scenario == "CAS conflict" {
							return nil, errors.New("synthetic resourceVersion conflict")
						}
						if scenario == "lost response" {
							if _, err := delegate(ctx, input, program, args...); err != nil {
								return nil, err
							}
							return nil, errors.New("synthetic lost API response")
						}
					}
				}
				return delegate(ctx, input, program, args...)
			}
			if _, err := claimCNPGProofResume(t.Context(), f.c, "umami-db", f.currentSource, "5678"); err == nil || f.creates != 0 {
				t.Fatal("unconfirmed claim was accepted or advanced a resource")
			}
			wantAttempts := 1
			if scenario == "concurrent checkpoint" {
				wantAttempts = 0
			}
			if attempts != wantAttempts {
				t.Fatal("claim was blindly repeated after an uncertain result")
			}
		})
	}
}
