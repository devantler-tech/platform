package main

import (
	"context"
	"errors"
	"strings"
)

// Recovery diagnostics identify the fixed stage without publishing command or
// host replies. Unwrap retains terminal and deadline classification.
type hostRecoveryStageError struct {
	stage string
	cause error
}

func (e hostRecoveryStageError) Error() string { return "host recovery held at " + e.stage }
func (e hostRecoveryStageError) Unwrap() error { return e.cause }

func hostRecoveryFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return hostRecoveryStageError{stage: stage, cause: err}
}

// recoveryNode refreshes only the Node after the expensive native and peer
// proofs. Its identity, boot, address and ownership must still match that proof;
// its fresh resource version remains a JSON Patch precondition.
func (h hostMemoryReboot) recoveryNode(ctx context.Context, r hostRebootRecord, proven object, cordoned bool, fence string) (object, string, error) {
	cm, err := h.owned(ctx, r)
	if err != nil {
		return nil, "", err
	}
	n, err := h.c.get(ctx, "node", "", h.target, false)
	if err != nil {
		return nil, "", err
	}
	address, identityErr := verifyNode(n, r.identity(), true)
	provenAddress, provenErr := workerPrivateAddress(proven)
	if identityErr != nil || provenErr != nil || address != provenAddress || text(n, "metadata", "resourceVersion") == "" ||
		text(n, "status", "nodeInfo", "bootID") != r.RecoveredBootID || !hugepagesReady(n) || !ready(valueObject(n, "status")) || !workerPressureClear(n) ||
		text(n, "status", "nodeInfo", "osImage") != r.OSImage || text(n, "status", "nodeInfo", "architecture") != r.Architecture ||
		(value(n, "spec", "unschedulable") == true) != cordoned || text(n, "metadata", "annotations", fenceAnnotation) != fence {
		return nil, "", errHostMemory
	}
	return n, text(cm, "metadata", "uid"), nil
}

// changeRecoveredNode makes one conditional write. A failed response is never
// retried: only an exact, still-owned postcondition readback can confirm it.
func (h hostMemoryReboot) changeRecoveredNode(ctx context.Context, r hostRebootRecord, proven object, removeFence bool) error {
	stage, afterFence := "uncordon-node", h.name()
	change := object{"op": "add", "path": "/spec/unschedulable", "value": false}
	if removeFence {
		stage, afterFence = "remove-fence", ""
		change = object{"op": "remove", "path": "/metadata/annotations/" + strings.ReplaceAll(fenceAnnotation, "/", "~1")}
	}
	n, journalUID, err := h.recoveryNode(ctx, r, proven, !removeFence, h.name())
	if err != nil {
		return hostRecoveryFailure("refresh-"+stage, err)
	}
	if err = h.c.patch(ctx, "node", "", n, change); err == nil {
		return nil
	}
	_, confirmedJournalUID, confirmationErr := h.recoveryNode(ctx, r, n, false, afterFence)
	if confirmationErr == nil && confirmedJournalUID == journalUID {
		return nil
	}
	return hostRecoveryFailure(stage+"-unconfirmed", errors.Join(err, confirmationErr))
}
