package main

import (
	"maps"
	"reflect"
	"regexp"
)

const nativeRestorePodUIDVariable = "LONGHORN_NATIVE_RESTORE_POD_UID"

var nativeRestorePodUIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func nativeRestorePodUIDEnvironment() []any {
	return []any{object{"name": nativeRestorePodUIDVariable, "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.uid"}}}}
}

// Admit only the restore consumers' exact Downward API field, then preserve
// the shared verifier's environment prohibition for every other workload.
// The observed Pod is never modified by qualification.
func nativeRestorePodWithoutUIDEnvironment(pod object) (object, error) {
	containers, ok := value(pod, "spec", "containers").([]any)
	if !ok || len(containers) != 1 {
		return nil, errNativeRestoreEvidence
	}
	container, ok := containers[0].(object)
	if !ok || !reflect.DeepEqual(value(container, "env"), nativeRestorePodUIDEnvironment()) {
		return nil, errNativeRestoreEvidence
	}
	result, spec, primary := maps.Clone(pod), maps.Clone(valueObject(pod, "spec")), maps.Clone(container)
	delete(primary, "env")
	spec["containers"] = []any{primary}
	result["spec"] = spec
	return result, nil
}
