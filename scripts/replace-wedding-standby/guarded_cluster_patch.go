package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

const clusterPatchAttempts = 5
const clusterPatchDeadline = time.Minute

// protectedClusterState freezes all spec, status and metadata except the
// observed diagnostic churn. Only the phaseReason ownership leaf and timestamp
// of a status-only managed-field entry are normalized; its owner stays frozen.
func protectedClusterState(cluster object) (object, error) {
	data, err := json.Marshal(cluster)
	if err != nil {
		return nil, errors.New("protected Cluster state is unknown")
	}
	var state object
	if json.Unmarshal(data, &state) != nil || !validMeta(state, namespace) {
		return nil, errors.New("protected Cluster state is incomplete")
	}
	metadata := state["metadata"].(map[string]any)
	delete(metadata, "resourceVersion")
	if fields, present := metadata["managedFields"]; present {
		items, ok := fields.([]any)
		if !ok || len(list(state, "metadata", "managedFields")) != len(items) {
			return nil, errors.New("cluster field ownership is incomplete")
		}
		for _, field := range list(state, "metadata", "managedFields") {
			roots, ok := value(field, "fieldsV1").(map[string]any)
			statusFields, valid := roots["f:status"].(map[string]any)
			if str(field, "subresource") == "status" && ok && len(roots) == 1 && valid {
				delete(field, "time")
				delete(statusFields, "f:phaseReason")
			}
		}
	}
	if status, ok := state["status"].(map[string]any); ok {
		delete(status, "phaseReason")
	}
	return state, nil
}

// guardedClusterPatch never replays a request. It keeps UID/version predicates
// on at most five new requests, and only rebinds after a verified non-commit.
// The callback must re-prove every phase-specific guard before returning.
func guardedClusterPatch(ctx context.Context, c client, cluster object, rebind func(context.Context) (object, error), patch func(object) []object) error {
	remaining := clusterPatchAttempts
	return guardedClusterPatchBudget(ctx, c, cluster, rebind, patch, &remaining)
}

// A continuation shares one pause-phase budget across its initial and refreshed
// marker. Every attempted request consumes a slot, including rejected requests.
func guardedClusterPatchBudget(ctx context.Context, c client, cluster object, rebind func(context.Context) (object, error), patch func(object) []object, remaining *int) error {
	if remaining == nil || *remaining <= 0 || *remaining > clusterPatchAttempts {
		return errors.New("guarded Cluster request budget exhausted; no further write")
	}
	baseline, err := protectedClusterState(cluster)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, clusterPatchDeadline)
	defer cancel()
	for *remaining > 0 {
		if bounded.Err() != nil {
			return errors.New("guarded Cluster request deadline reached; no further write")
		}
		*remaining--
		err = c.patch(bounded, "cluster", cluster, patch(cluster))
		if err == nil {
			return nil
		}
		var failure conditionalWriteFailure
		if !errors.As(err, &failure) || !failure.rejected || *remaining == 0 {
			return err
		}
		cluster, err = rebind(bounded)
		if err != nil {
			return err
		}
		current, e := protectedClusterState(cluster)
		if e != nil || !reflect.DeepEqual(baseline, current) {
			return errors.New("protected Cluster state changed after rejection; no further write")
		}
	}
	return err
}
