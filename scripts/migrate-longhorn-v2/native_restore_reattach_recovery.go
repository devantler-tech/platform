package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
)

const nativeReattachmentOperation = "resume-native-v2-reattach"
const nativeReattachmentConfirmation = "resume-longhorn-native-v2-reattach:"

func authorizeNativeReattachment(getenv func(string) string) (original, previous string, err error) {
	scope := strings.TrimPrefix(getenv("LONGHORN_V2_CONFIRM"), nativeReattachmentConfirmation)
	original, previous, ok := strings.Cut(scope, ":")
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	if !ok || getenv("LONGHORN_V2_OPERATION") != nativeReattachmentOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" ||
		!validBenchmarkOwner(original, source) || !validBenchmarkOwner(previous, source) ||
		!validBenchmarkOwner(run, source) || original == previous || run == original || run == previous ||
		authorizeWorkflow(getenv, nativeReattachmentConfirmation+original+":"+previous) != nil {
		return "", "", errNativeRestoreEvidence
	}
	return original, previous, nil
}

// Only a never-assigned, memory-rejected consumer is eligible. Its exact
// admitted identity and storage reference remain bound to the finished owner.
func verifyUnscheduledReattachment(pod object, state nativeRestoreState, seed canarySeedState) error {
	if len(state.Targets) != 3 || state.FirstNode != state.Targets[0].Name || state.SecondNode != "" ||
		benchmarkIdentity(pod) != state.Resources["SecondPod"] ||
		text(pod, "metadata", "resourceVersion") == "" ||
		text(pod, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != state.Run ||
		value(pod, "metadata", "ownerReferences") != nil ||
		text(pod, "metadata", "labels", "velero.io/restore-name") != "" ||
		!absentOrEmptyString(value(pod, "spec", "nodeName")) ||
		text(pod, "spec", "restartPolicy") != "Never" || text(pod, "status", "phase") != "Pending" ||
		value(pod, "status", "startTime") != nil || value(pod, "status", "containerStatuses") != nil ||
		value(pod, "status", "ephemeralContainerStatuses") != nil {
		return errNativeRestoreEvidence
	}
	node := text(pod, "spec", "nodeSelector", "kubernetes.io/hostname")
	if (node != state.Targets[1].Name && node != state.Targets[2].Name) ||
		!reflect.DeepEqual(value(pod, "spec", "nodeSelector"), object{"kubernetes.io/hostname": node}) {
		return errNativeRestoreEvidence
	}
	conditions, _ := value(pod, "status", "conditions").([]any)
	if len(conditions) != 1 {
		return errNativeRestoreEvidence
	}
	condition, _ := conditions[0].(object)
	if text(condition, "type") != "PodScheduled" || text(condition, "status") != "False" ||
		text(condition, "reason") != "Unschedulable" || !strings.Contains(text(condition, "message"), "Insufficient memory") {
		return errNativeRestoreEvidence
	}
	script, err := nativeRestoreVerifyScript(seed.PayloadSHA256, state.Token)
	if err != nil {
		return err
	}
	normalized, err := nativeRestorePodWithoutUIDEnvironment(pod)
	if err != nil || verifyNativeCanaryPodSpec(normalized, state.Resources["SecondPod"].UID, script+nativeReattachWait, nil) != nil {
		return errNativeRestoreEvidence
	}
	containers := value(normalized, "spec", "containers").([]any)
	container := containers[0].(object)
	desired, err := nativeRestoreReaderPod(state.Run, node, script+nativeReattachWait)
	if err != nil {
		return err
	}
	expected := value(desired, "spec", "containers").([]any)[0].(object)
	if !reflect.DeepEqual(value(container, "resources"), value(expected, "resources")) {
		return errNativeRestoreEvidence
	}
	return nil
}

func verifyPreviousReattachmentOwner(ctx context.Context, c client, state nativeRestoreState, previousExecutor, previousRun string) error {
	sha, run, ok := strings.Cut(previousExecutor, ":")
	if !ok || run != previousRun || sha == state.Source || sha == os.Getenv("GITHUB_SHA") ||
		verifyNativeRestoreResumeOwner(ctx, c, state) != nil {
		return errNativeRestoreEvidence
	}
	prior, err := readFinishedMigrationRun(ctx, c, run, sha)
	if err != nil || prior.Attempt != 1 || prior.Conclusion != "failure" {
		return errors.New("the previous reattachment executor must have a confirmed failed first attempt")
	}
	return nil
}

func resumeNativeReattachment(ctx context.Context, c client, config []byte) error {
	original, previous, err := authorizeNativeReattachment(os.Getenv)
	if err != nil || c.context != "admin@prod" || c.nativeSeed || c.run == nil || !commandContextActive(ctx) {
		return errNativeRestoreEvidence
	}
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, c)
	previousExecutor := text(journal, "metadata", "annotations", nativeRestoreExecutorAnnotation)
	if err != nil || state.Run != original || state.Phase != "reattach-owned" || state.Reattachment != nil || state.Expired == nil ||
		!nativeRestoreExecutorMatches(journal, previousExecutor) || verifyPreviousReattachmentOwner(ctx, c, state, previousExecutor, previous) != nil {
		return errors.New("an owned pending reattachment from a failed first attempt is required")
	}
	source, executor, err := nativeRestoreGrant(os.Getenv, state)
	if err != nil || executor == "" || sourceCurrent(ctx, c.run, source) != nil {
		return errNativeRestoreEvidence
	}
	seedReceipt, err := readNativeRestoreSeed(ctx, c, source, os.Getenv("GITHUB_RUN_ID"))
	if err != nil || seedReceipt.Journal != state.SeedJournal || !reflect.DeepEqual(seedReceipt.Record, record) || seedReceipt.Seed != seed {
		return errors.New("the original retained backup could not be rebound")
	}
	targets, managers, err := observeNativeRestorePool(ctx, c, config, state.Run, &state)
	if err != nil || !reflect.DeepEqual(targets, state.Targets) {
		return errNativeRestoreEvidence
	}
	r := nativeRestoreRun{c: c, config: config, plan: nativeRestoreAdmission{Seed: seedReceipt, Managers: managers},
		state: state, source: source, journalUID: benchmarkIdentity(journal).UID}
	pod, err := c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil || verifyUnscheduledReattachment(pod, state, seed) != nil {
		return errors.New("the original second consumer is no longer the proven unassigned test Pod")
	}
	planned := state
	planned.Phase = nativeReattachmentDeleteRequested
	planned.Reattachment = &nativeReattachmentRecovery{PreviousExecutor: previousExecutor, UnscheduledPod: benchmarkIdentity(pod)}
	detached, err := readNativeRestoreDetached(ctx, c, planned)
	if err != nil || verifyUnscheduledReattachmentDetached(record, seed, planned, detached) != nil {
		return errors.New("the unassigned consumer must have no attachment or storage activity")
	}
	asset, err := selectReattachmentConsumer(record, seed, state, detached.Inventory, text(pod, "spec", "nodeSelector", "kubernetes.io/hostname"))
	if err != nil {
		return err
	}
	if err := r.verifyExpiredRestorePolicy(ctx); err != nil {
		return err
	}
	if err := r.claimUnscheduledReattachment(ctx, planned, previous, executor); err != nil {
		return err
	}
	return r.replaceUnscheduledReattachment(ctx, pod, asset)
}

func verifyUnscheduledReattachmentDetached(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, observation nativeRestoreDetachedObservation) error {
	if state.Phase != nativeReattachmentDeleteRequested || state.Reattachment == nil || observation.Inventory.pods == nil {
		return errNativeRestoreEvidence
	}
	pods := make([]object, 0, len(observation.Inventory.pods))
	found := false
	for _, pod := range observation.Inventory.pods {
		if benchmarkIdentity(pod) != state.Reattachment.UnscheduledPod {
			pods = append(pods, pod)
			continue
		}
		if found || verifyUnscheduledReattachment(pod, state, seed) != nil {
			return errNativeRestoreEvidence
		}
		found = true
	}
	if !found {
		return errNativeRestoreEvidence
	}
	observation.Inventory.pods = pods
	return verifyNativeRestoreDetached(record, seed, state, observation)
}

func (r *nativeRestoreRun) claimUnscheduledReattachment(ctx context.Context, planned nativeRestoreState, previous, executor string) error {
	journal, record, seed, current, err := readNativeRestoreJournal(ctx, r.c)
	if err != nil || benchmarkIdentity(journal).UID != r.journalUID || !reflect.DeepEqual(current, r.state) ||
		!reflect.DeepEqual(record, r.plan.Seed.Record) || seed != r.plan.Seed.Seed ||
		!nativeRestoreExecutorMatches(journal, planned.Reattachment.PreviousExecutor) ||
		validateNativeRestoreTransition(current, planned, record, seed) != nil {
		return errNativeRestoreEvidence
	}
	if err := verifyPreviousReattachmentOwner(ctx, r.c, current, planned.Reattachment.PreviousExecutor, previous); err != nil {
		return err
	}
	if err := r.checkActivation(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(planned)
	if err != nil || sourceCurrent(ctx, r.c.run, r.source) != nil {
		return errNativeRestoreEvidence
	}
	patch := metadataChanges(journal, "annotations", map[string]string{nativeRestoreExecutorAnnotation: executor})
	patch = append(patch, object{"op": "replace", "path": "/data/restore", "value": string(body)})
	if err := r.c.patch(ctx, "configmap", "longhorn-system", journal, patch...); err != nil {
		return errors.New("the replacement reattachment ownership could not be confirmed")
	}
	r.state = planned
	return r.guard(ctx)
}

func (r *nativeRestoreRun) replaceUnscheduledReattachment(ctx context.Context, pod, asset object) error {
	if err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.source, "/api/v1/namespaces/"+benchmarkNamespace+"/pods/fio", pod); err != nil {
		return errors.New("the unassigned test Pod deletion could not be confirmed")
	}
	if err := waitReady(ctx, r.observeDetached); err != nil {
		return err
	}
	if err := r.advance(ctx, nativeReattachmentPodDeleted, nil); err != nil {
		return err
	}
	if err := r.advance(ctx, nativeReattachmentCreateRequested, nil); err != nil {
		return err
	}
	if err := r.observeDetached(ctx); err != nil {
		return err
	}
	inventory, err := r.c.snapshot(ctx)
	if err != nil || verifyBenchmarkClientMemory(inventory, asset) != nil || r.verifyExpiredRestorePolicy(ctx) != nil || r.guard(ctx) != nil {
		return errNativeRestoreEvidence
	}
	created, err := r.c.create(ctx, r.source, asset)
	if err != nil {
		return errors.New("the third-worker consumer creation could not be confirmed")
	}
	id := benchmarkIdentity(created)
	actual, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil || id.UID == "" || benchmarkIdentity(actual) != id {
		return errNativeRestoreEvidence
	}
	if err := r.advance(ctx, nativeReattachmentReplacement, func(next *nativeRestoreState) { next.Resources["SecondPod"] = id }); err != nil {
		return err
	}
	return r.finishReattachment(ctx, text(asset, "spec", "nodeSelector", "kubernetes.io/hostname"))
}
