package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// cnpgProofExecutor grants execution without rewriting creation provenance.
type cnpgProofExecutor struct{ Source, Run, JournalUID string }

// executionOwner keeps the admitted executor separate from persisted provenance.
func (p *cnpgProofRun) executionOwner() (cnpgProofExecutor, error) {
	owner := cnpgProofExecutor{Source: p.record.Source, Run: p.record.Run, JournalUID: p.journalUID}
	if p.record.Executor == nil {
		if p.executing != (cnpgProofExecutor{}) {
			return cnpgProofExecutor{}, errors.New("database executor claim disappeared")
		}
	} else {
		owner = p.executing
		if owner != *p.record.Executor || owner.Run == p.record.Run || owner.JournalUID != p.journalUID {
			return cnpgProofExecutor{}, errors.New("database executor differs from its admitted claim")
		}
	}
	if !validBenchmarkOwner(owner.Run, owner.Source) || owner.JournalUID == "" {
		return cnpgProofExecutor{}, errors.New("database execution ownership is incomplete")
	}
	return owner, nil
}

// currentExecution fences source and native journal ownership before side effects.
func (p *cnpgProofRun) currentExecution(ctx context.Context) (cnpgProofExecutor, error) {
	owner, err := p.executionOwner()
	if err != nil {
		return cnpgProofExecutor{}, err
	}
	cm, current, err := readCNPGProof(ctx, p.c, p.record.Target)
	if err != nil || text(cm, "metadata", "uid") != p.journalUID || !recoveryJSONEqual(current, p.record) {
		return cnpgProofExecutor{}, errors.New("database execution journal changed")
	}
	if p.record.Executor != nil {
		if err = cnpgExecutorRunning(ctx, p.c, owner); err != nil {
			return cnpgProofExecutor{}, err
		}
	}
	if err = sourceCurrent(ctx, p.c.run, owner.Source); err != nil {
		return cnpgProofExecutor{}, err
	}
	return owner, nil
}

func (p *cnpgProofRun) createOwned(ctx context.Context, desired object) (object, error) {
	owner, err := p.currentExecution(ctx)
	if err != nil {
		return nil, err
	}
	return p.c.create(ctx, owner.Source, desired)
}

// claimCNPGProofResume changes only the executor of the journal the caller confirmed.
// Umami's existing namespace and policy must still be empty and qualified.
func claimCNPGProofResume(ctx context.Context, c client, cm object, record cnpgProofRecord, source, run string) (*cnpgProofRun, error) {
	refused := errors.New("database proof continuation is not qualified")
	if !validBenchmarkOwner(run, source) || c.context != "admin@prod" || record.Run == run || !cnpgResumeCheckpointEligible(record) {
		return nil, refused
	}
	want := cnpgProofExecutor{Source: source, Run: run, JournalUID: text(cm, "metadata", "uid")}
	if err := qualifyCNPGResumeOwners(ctx, c, record, want); err != nil {
		return nil, err
	}
	if record.Phase == "policy-created" {
		pending := cnpgProofRun{c: c, record: record}
		if _, err := pending.readRecoveryIsolation(ctx); err != nil {
			return nil, err
		}
	}
	if err := qualifyCNPGResumeCheckpoint(ctx, c, record, source); err != nil {
		return nil, err
	}
	if err := qualifyCNPGResumeOwners(ctx, c, record, want); err != nil {
		return nil, err
	}
	return claimCNPGProofExecutor(ctx, c, cm, record, want)
}

func claimCNPGProofExecutor(ctx context.Context, c client, cm object, record cnpgProofRecord, want cnpgProofExecutor) (*cnpgProofRun, error) {
	refused := errors.New("database proof continuation is not qualified")
	if err := sourceCurrent(ctx, c.run, want.Source); err != nil {
		return nil, err
	}
	currentCM, current, err := readCNPGProof(ctx, c, record.Target)
	if err != nil || benchmarkIdentity(currentCM) != benchmarkIdentity(cm) || text(currentCM, "metadata", "resourceVersion") != text(cm, "metadata", "resourceVersion") || !reflect.DeepEqual(current, record) {
		return nil, refused
	}
	if record.Executor == nil || *record.Executor != want {
		record.Executor = &want
		body, encodeErr := encodeCNPGProofRecord(record)
		if encodeErr != nil {
			return nil, encodeErr
		}
		if err = c.patch(ctx, "configmap", "longhorn-system", currentCM, object{"op": "replace", "path": "/data/record", "value": body}); err != nil {
			return nil, errors.New("database executor claim is unconfirmed; preserve its journal")
		}
	}
	checkedCM, checked, err := readCNPGProof(ctx, c, record.Target)
	if err != nil || text(checkedCM, "metadata", "uid") != want.JournalUID || !reflect.DeepEqual(checked, record) {
		return nil, refused
	}
	return &cnpgProofRun{c: c, record: checked, journalUID: want.JournalUID, executing: want, storage: temporaryV2Activation{c: c, source: want.Source, owner: want.Run, pools: c.inventoryPools}}, nil
}

func cnpgResumeCheckpointEligible(record cnpgProofRecord) bool {
	if record.Cleaned || record.RestoreVerified || record.Preparation != nil || record.Credential != nil ||
		record.Admission != nil || record.Storage != nil || record.RestoreReceipt != nil {
		return false
	}
	switch record.Phase {
	case "backup-created":
		return len(record.Resources) == 0 && record.Plan == nil
	case "restore-planned":
		return len(record.Resources) == 0 && record.Plan != nil
	case "policy-created":
		namespace, policy := record.Resources["Namespace"], record.Resources["Policy"]
		return record.Target == "umami-db" && record.Plan != nil && len(record.Plan.Resources) == 4 &&
			len(record.Resources) == 2 && namespace.Name == "longhorn-db-proof-"+record.Target+"-"+record.Run &&
			namespace.Namespace == "" && isolationUID.MatchString(namespace.UID) &&
			policy.Name == "restore-isolation" && policy.Namespace == namespace.Name &&
			isolationUID.MatchString(policy.UID) && policy.UID != namespace.UID
	default:
		return false
	}
}

func qualifyCNPGResumeOwners(ctx context.Context, c client, record cnpgProofRecord, want cnpgProofExecutor) error {
	if err := benchmarkOwnerFinished(ctx, c, record.Run, record.Source); err != nil {
		return err
	}
	if record.Executor != nil && *record.Executor != want {
		if err := benchmarkOwnerFinished(ctx, c, record.Executor.Run, record.Executor.Source); err != nil {
			return err
		}
	}
	return cnpgExecutorRunning(ctx, c, want)
}

// qualifyCNPGResumeCheckpoint rebinds the native archive and Backup, and checks
// an established plan against today's renderer without replacing its WAL barrier.
func qualifyCNPGResumeCheckpoint(ctx context.Context, c client, record cnpgProofRecord, source string) error {
	hostname := cnpgRecoveryHostname(record.Plan)
	if record.Plan == nil {
		// This read-only backup validation does not retain or deploy its rendered plan.
		hostname = "read-only-validation"
	}
	live, err := planCNPGRecovery(ctx, c, cnpgRecoveryRequest{Target: record.Target, Source: source, Run: record.Run, Hostname: hostname, Archive: record.Archive, Backup: record.Backup})
	if err != nil {
		return err
	}
	if record.Plan == nil {
		return nil
	}
	if record.Plan.Source != live.Source || len(live.Resources) != 4 {
		return errors.New("recorded recovery source changed")
	}
	_, digest, ok := strings.Cut(text(live.Resources[3], "spec", "imageName"), "@")
	if !ok {
		return errors.New("current recovery process image is unverified")
	}
	want, err := cnpgRecoveryResources(record.Run, live.Source, "ghcr.io/cloudnative-pg/postgresql@"+digest, record.Plan.Barrier, hostname)
	if err != nil || !recoveryJSONEqual(want, record.Plan.Resources) {
		return errors.New("recorded recovery plan differs from the reviewed renderer")
	}
	return waitCNPGRecoveryArchive(ctx, c, cnpgRecoveryRequest{Target: record.Target, Source: source, Run: record.Run, Archive: record.Archive, Backup: record.Backup}, *record.Plan)
}

// cnpgExecutorRunning binds a new claim to the latest native first-attempt run.
func cnpgExecutorRunning(ctx context.Context, c client, owner cnpgProofExecutor) error {
	if !validBenchmarkOwner(owner.Run, owner.Source) || owner.JournalUID == "" {
		return errors.New("database executor identity is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+owner.Run)
	if err != nil || ctx.Err() != nil || len(raw) > 1024*1024 || strictjson.Validate(raw, map[string][]string{"": {"id", "status", "head_sha", "head_branch", "event", "run_attempt", "path"}}) != nil {
		return errors.New("current database executor is unverified")
	}
	var actual struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		Source  string `json:"head_sha"`
		Branch  string `json:"head_branch"`
		Event   string `json:"event"`
		Attempt uint64 `json:"run_attempt"`
		Path    string `json:"path"`
	}
	wantID, _ := strconv.ParseInt(owner.Run, 10, 64)
	if json.Unmarshal(raw, &actual) != nil || actual.ID != wantID || actual.Source != owner.Source || actual.Status != "in_progress" || actual.Branch != "main" || actual.Event != "workflow_dispatch" || actual.Attempt != 1 || !reviewedWorkflowPath(actual.Path) {
		return errors.New("current database executor is outside the reviewed workflow")
	}
	return nil
}
