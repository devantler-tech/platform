package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
)

const (
	nativeRestoreResumeOperation    = "resume-native-v2-seed-restore"
	nativeRestoreResumeConfirmation = "resume-longhorn-native-v2-seed:"
	nativeRestoreExecutorAnnotation = "storage.platform.devantler.tech/native-restore-executor"
)

func nativeRestoreResumeRun(getenv func(string) string) (string, error) {
	original := strings.TrimPrefix(getenv("LONGHORN_V2_CONFIRM"), nativeRestoreResumeConfirmation)
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	if getenv("LONGHORN_V2_OPERATION") != nativeRestoreResumeOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" ||
		!validBenchmarkOwner(original, source) || !validBenchmarkOwner(run, source) || original == run ||
		authorizeWorkflow(getenv, nativeRestoreResumeConfirmation+original) != nil {
		return "", errNativeRestoreEvidence
	}
	return original, nil
}

// The original run remains the owner of the names, backup and identities in
// the journal. Only the revision and run allowed to execute writes change.
func nativeRestoreGrant(getenv func(string) string, state nativeRestoreState) (source, executor string, err error) {
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	if source == state.Source && run == state.Run && authorizeNativeRestore(getenv) == nil {
		return source, "", nil
	}
	original, err := nativeRestoreResumeRun(getenv)
	if err != nil || original != state.Run || !validBenchmarkOwner(state.Run, state.Source) || source == state.Source ||
		nativeRestorePhaseIndex(state.Phase) < nativeRestorePhaseIndex("restore-owned") {
		return "", "", errNativeRestoreEvidence
	}
	return source, source + ":" + run, nil
}

func nativeRestoreExecutorMatches(journal object, executor string) bool {
	actual, exists := valueObject(journal, "metadata", "annotations")[nativeRestoreExecutorAnnotation]
	if executor == "" {
		return !exists
	}
	return exists && actual == executor
}

func verifyNativeRestoreResumeOwner(ctx context.Context, c client, state nativeRestoreState) error {
	run, err := readFinishedMigrationRun(ctx, c, state.Run, state.Source)
	if err != nil || run.Attempt != 1 || run.Conclusion != "failure" {
		return errors.New("the original restore does not have a verified failed first attempt")
	}
	return nil
}

func resumeNativeRestore(ctx context.Context, c client, config []byte) error {
	// Authorize the dispatch before making any external call.
	_, err := nativeRestoreResumeRun(os.Getenv)
	if err != nil || c.context != "admin@prod" || c.nativeSeed || c.run == nil || !commandContextActive(ctx) {
		return errNativeRestoreEvidence
	}
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, c)
	if err != nil || state.Phase != "restore-owned" || !nativeRestoreExecutorMatches(journal, "") {
		return errors.New("the original restore must still be awaiting verification without a continuation")
	}
	source, executor, err := nativeRestoreGrant(os.Getenv, state)
	if err != nil || executor == "" || sourceCurrent(ctx, c.run, source) != nil {
		return errNativeRestoreEvidence
	}
	if err := verifyNativeRestoreResumeOwner(ctx, c, state); err != nil {
		return err
	}
	seedReceipt, err := readNativeRestoreSeed(ctx, c, source, os.Getenv("GITHUB_RUN_ID"))
	if err != nil || seedReceipt.Journal != state.SeedJournal || !reflect.DeepEqual(seedReceipt.Record, record) || seedReceipt.Seed != seed {
		return errors.New("the original retained backup could not be rebound")
	}
	_, managers, err := observeNativeRestorePool(ctx, c, config, state.Run, &state)
	if err != nil {
		return err
	}
	r := nativeRestoreRun{c: c, config: config, plan: nativeRestoreAdmission{Seed: seedReceipt, Managers: managers},
		state: state, source: source, journalUID: benchmarkIdentity(journal).UID}
	velero, err := captureNativeRestoreVelero(ctx, c, source)
	if err != nil {
		return err
	}
	pod, err := c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil {
		return err
	}
	if nativeRestorePodExpired(pod) {
		return r.resumeExpired(ctx, pod, executor, velero)
	}
	if _, _, err := r.readSeedTransfer(ctx, velero); err != nil {
		return errors.Join(errors.New("the original completed transfer and healthy first attachment are required"), err)
	}
	if err := r.claimResume(ctx, executor); err != nil {
		return err
	}
	if err := r.verifySeedTransfer(ctx, velero); err != nil {
		return err
	}
	return r.verifyReattachment(ctx, velero)
}

// Record one continuation without altering the original data. An ambiguous
// patch leaves the journal for explicit recovery instead of replaying a write.
func (r *nativeRestoreRun) claimResume(ctx context.Context, executor string) error {
	source, want, err := nativeRestoreGrant(os.Getenv, r.state)
	if err != nil || want == "" || executor != want || source != r.source || r.state.Phase != "restore-owned" {
		return errNativeRestoreEvidence
	}
	if err := verifyNativeRestoreResumeOwner(ctx, r.c, r.state); err != nil {
		return err
	}
	journal, record, seed, actual, err := readNativeRestoreJournal(ctx, r.c)
	if err != nil || benchmarkIdentity(journal).UID != r.journalUID || !reflect.DeepEqual(actual, r.state) ||
		!reflect.DeepEqual(record, r.plan.Seed.Record) || seed != r.plan.Seed.Seed || !nativeRestoreExecutorMatches(journal, "") {
		return errNativeRestoreEvidence
	}
	if err := r.checkActivation(ctx); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, r.c.run, source); err != nil {
		return err
	}
	patch := metadataChanges(journal, "annotations", map[string]string{nativeRestoreExecutorAnnotation: executor})
	if err := r.c.patch(ctx, "configmap", "longhorn-system", journal, patch...); err != nil {
		return errors.New("the restore continuation could not be confirmed; preserve its journal")
	}
	return r.guard(ctx)
}
