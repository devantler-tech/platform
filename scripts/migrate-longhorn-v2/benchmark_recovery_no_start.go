package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const retirementCheckpointAnnotation = "storage.platform.devantler.tech/retirement-checkpoint"

var benchmarkCheckpointPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}-[a-f0-9]{32}$`)

// This builds an unbound policy, not a checkpoint or physical barrier. Activation
// on all API servers, actual UID/RV metadata writes and direct physical-instance
// observations under pinned process epochs are still required.
func benchmarkRetirementInstanceFence(record benchmarkRecord, groups map[string][]object, checkpoint string) (object, error) {
	if !benchmarkCheckpointPattern.MatchString(checkpoint) {
		return nil, errors.New("a fresh recovery checkpoint identity is required")
	}
	children, err := qualifyRetirementInstances(record, groups)
	if err != nil {
		return nil, err
	}
	uids := map[string][]string{"engines": {}, "replicas": {}, "enginefrontends": {}, "volumeattachments": {}, "snapshots": {}}
	names, owners, managers := map[string]string{}, map[string]string{}, map[string]string{}
	references := map[string][]object{}
	reserved := map[string][]string{"engines": {}, "replicas": {}, "enginefrontends": {}, "volumeattachments": {record.Resources["LonghornVolume"].Name}, "snapshots": {}}
	for _, child := range children {
		uids[child.Resource] = append(uids[child.Resource], child.Identity.UID)
		names[child.Identity.UID], owners[child.Identity.UID], managers[child.Identity.UID] = child.Identity.Name, child.OwnerID, child.InstanceManagerName
		references[child.Identity.UID] = []object{child.OwnerReference}
		if child.Resource != "volumeattachments" {
			reserved[child.Resource] = append(reserved[child.Resource], child.Identity.Name)
		}
	}
	for _, values := range uids {
		sort.Strings(values)
	}
	for _, values := range reserved {
		sort.Strings(values)
	}
	var literalErr error
	literal := func(value any) string {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			literalErr = errors.New("retirement literals contain unsupported values")
			return "null"
		}
		return string(raw)
	}
	quote := strconv.Quote
	backend := record.Resources["LonghornVolume"]
	volumeLabel := func(root string) string {
		return "(has(" + root + ".metadata.labels) && 'longhornvolume' in " + root + ".metadata.labels && " + root + ".metadata.labels['longhornvolume'] == " + quote(backend.Name) + ")"
	}
	refers := func(root string) string {
		return "(" + root + ".metadata.name in " + literal(reserved) + "[request.resource.resource] || " + volumeLabel(root) + " || (has(" + root + ".spec.volumeName) && " + root + ".spec.volumeName == " + quote(backend.Name) + ") || (has(" + root + ".spec.volume) && " + root + ".spec.volume == " + quote(backend.Name) + ") || (has(" + root + ".metadata.ownerReferences) && " + root + ".metadata.ownerReferences.exists(r,r.uid == " + quote(backend.UID) + ")))"
	}
	uid := "dyn(object.metadata).uid"
	falseFlag := func(path string) string { return "(!has(" + path + ") || " + path + " == false)" }
	emptyMap := func(path string) string { return "(!has(" + path + ") || " + path + ".size() == 0)" }
	instance := strings.Join([]string{
		"object.spec.volumeName == " + quote(backend.Name), "object.spec.dataEngine == " + quote(record.Phase),
		`object.spec.desireState == "stopped"`, `has(object.status) && has(object.status.currentState) && object.status.currentState == "stopped"`,
		falseFlag("object.status.starting"), falseFlag("object.status.started"),
		"object.status.ownerID == " + literal(owners) + "[" + uid + "]",
		"(!has(object.status.instanceManagerName) || object.status.instanceManagerName == \"\" || object.status.instanceManagerName == " + literal(managers) + "[" + uid + "])",
	}, " && ")
	attachment := "object.spec.volume == " + quote(backend.Name) + " && " + emptyMap("object.spec.attachmentTickets") + " && " + emptyMap("object.status.attachmentTicketStatuses")
	invariant := strings.Join([]string{
		`request.operation == "UPDATE"`, uid + " in " + literal(uids) + "[request.resource.resource]",
		"dyn(oldObject.metadata).uid == " + uid, "object.metadata.name == " + literal(names) + "[" + uid + "]",
		`(!has(object.metadata.namespace) || object.metadata.namespace == "longhorn-system")`,
		volumeLabel("object"),
		"has(dyn(object.metadata).ownerReferences) && dyn(object.metadata).ownerReferences == " + literal(references) + "[" + uid + "]", "object.spec == oldObject.spec",
		"has(object.metadata.annotations) && " + quote(retirementCheckpointAnnotation) + " in object.metadata.annotations && object.metadata.annotations[" + quote(retirementCheckpointAnnotation) + "] == " + quote(checkpoint),
		"(request.resource.resource == \"volumeattachments\" ? (" + attachment + ") : (" + instance + "))",
	}, " && ")
	policy := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": object{"name": "lh-benchmark-retire-instances-" + record.Phase + "-" + record.Run}, "spec": object{
		"failurePolicy":    "Fail",
		"matchConstraints": object{"matchPolicy": "Equivalent", "objectSelector": object{}, "namespaceSelector": object{}, "resourceRules": []any{object{"apiGroups": []any{"longhorn.io"}, "apiVersions": []any{"v1beta2"}, "resources": []any{"engines", "engines/status", "replicas", "replicas/status", "enginefrontends", "enginefrontends/status", "volumeattachments", "volumeattachments/status", "snapshots", "snapshots/status"}, "operations": []any{"CREATE", "UPDATE"}, "scope": "Namespaced"}}},
		"matchConditions":  []any{object{"name": "references-original-benchmark", "expression": "request.namespace == \"longhorn-system\" && (" + refers("object") + " || (oldObject != null && " + refers("oldObject") + "))"}},
		"validations":      []any{object{"expression": invariant, "reason": "Forbidden", "message": benchmarkInstanceFenceMessage}},
	}}
	if literalErr != nil {
		return nil, literalErr
	}
	return policy, nil
}
