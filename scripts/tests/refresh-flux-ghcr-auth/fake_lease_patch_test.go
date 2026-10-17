package refreshfluxghcrauth

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// This bounded JSON Patch evaluator implements test/replace on existing scalar
// paths. It requires no holder/version test of its own: only operations actually
// carried by the captured production patch determine whether a replay applies.
func evaluateLeasePatch(patch []jsonPatchOperation, state map[string]any) (map[string]any, error) {
	candidate := make(map[string]any, len(state))
	for path, value := range state {
		candidate[path] = value
	}
	for _, operation := range patch {
		current, exists := candidate[operation.Path]
		if !exists {
			return nil, fmt.Errorf("missing patch path %s", operation.Path)
		}
		switch operation.Operation {
		case "test":
			if !reflect.DeepEqual(current, operation.Value) {
				return nil, fmt.Errorf("test failed at %s", operation.Path)
			}
		case "replace":
			candidate[operation.Path] = operation.Value
		default:
			return nil, fmt.Errorf("unsupported fixture operation %s", operation.Operation)
		}
	}
	return candidate, nil
}

func leasePatchSnapshot() map[string]any {
	return map[string]any{
		"/metadata/resourceVersion":  defaultString(markerContent("sync-lease-resource-version"), "10"),
		"/spec/holderIdentity":       markerContent("sync-lease-holder"),
		"/spec/renewTime":            markerContent("sync-lease-renew-time"),
		"/spec/leaseDurationSeconds": float64(parseInt(markerContent("sync-lease-duration"), 120)),
	}
}

type delayedLeaseRenewal struct {
	Patch  []jsonPatchOperation `json:"patch"`
	Before map[string]any       `json:"before"`
}

func exerciseDelayedLeaseRenewal() error {
	var delayed delayedLeaseRenewal
	if err := json.Unmarshal([]byte(markerContent("sync-lease-delayed-renewal")), &delayed); err != nil {
		return fmt.Errorf("no captured renewal: %w", err)
	}
	if len(delayed.Patch) == 0 {
		return fmt.Errorf("captured renewal has no operations")
	}
	// A positive control proves the payload was valid when captured, before the
	// successful release. A parser error cannot stand in for the rejected replay.
	if _, err := evaluateLeasePatch(delayed.Patch, delayed.Before); err != nil {
		return fmt.Errorf("captured renewal was already invalid: %w", err)
	}
	setMarkerContent("sync-lease-delayed-renewal-before-control", "accepted")
	after := leasePatchSnapshot()
	for _, mode := range []string{"actual", "holder-only", "version-only"} {
		var replay []jsonPatchOperation
		for _, operation := range delayed.Patch {
			if operation.Operation == "test" && ((mode == "holder-only" && operation.Path == "/metadata/resourceVersion") || (mode == "version-only" && operation.Path == "/spec/holderIdentity")) {
				continue
			}
			replay = append(replay, operation)
		}
		outcome := "accepted"
		if _, err := evaluateLeasePatch(replay, after); err != nil {
			outcome = "rejected"
		}
		setMarkerContent("sync-lease-delayed-renewal-"+mode, outcome)
	}
	return nil
}
