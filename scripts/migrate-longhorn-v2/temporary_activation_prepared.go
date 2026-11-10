package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// preparedTemporaryBinding validates a completed unused-storage reset journal
// against the current worker and its changed boot and EPHEMERAL partition.
func preparedTemporaryBinding(cm object, binding temporaryV2Binding) (journal, error) {
	name := "longhorn-v2-prepare-" + binding.Selection
	if text(cm, "metadata", "name") != name || text(cm, "metadata", "namespace") != "longhorn-system" ||
		text(cm, "metadata", "uid") == "" || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil {
		return journal{}, errTemporaryActivation
	}
	data := []byte(text(cm, "data", "record"))
	fields := map[string][]string{"": {"schema", "target", "source", "ownerRun", "phase", "nodeUID", "providerID", "machineUUID", "longhornUID", "ephemeralUUID", "originalDisks", "temporarySelection", "nativeEmptyDigest", "originalBootID", "rawVolumeAbsent", "stagingLayout"}}
	var record journal
	if len(data) == 0 || len(data) > 32<<10 || strictjson.Validate(data, fields) != nil || json.Unmarshal(data, &record) != nil {
		return journal{}, errTemporaryActivation
	}
	p := preparation{target: binding.Name, name: name, temporarySelection: binding.Selection}
	if p.validateRecord(record) != nil || record.Schema != 2 || !record.StagingLayout || record.Phase != "prepared" || record.NodeUID != binding.NodeUID ||
		record.ProviderID != binding.ProviderID || record.MachineUUID != binding.MachineUUID || record.LonghornUID != binding.LonghornUID ||
		record.OriginalBootID == binding.BootID || strings.EqualFold(record.EphemeralUUID, binding.EphemeralUUID) {
		return journal{}, errTemporaryActivation
	}
	return record, nil
}

// prepared requires successful completion of the original protected preparation
// before enabling a rebuilt worker's raw disk. The V2-born pool has no reset
// operation; its independently checked native raw layout remains mandatory.
func (a temporaryV2Activation) prepared(ctx context.Context, binding temporaryV2Binding) (temporaryV2Binding, error) {
	if binding.Selection == "temporary-v2-1" {
		return binding, nil
	}
	if binding.Selection != "temporary-v1-1" && binding.Selection != "temporary-v1-2" {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	cm, err := a.c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-prepare-"+binding.Selection, false)
	if err != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	record, err := preparedTemporaryBinding(cm, binding)
	if err != nil {
		return temporaryV2Binding{}, err
	}
	data, err := a.c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+record.OwnerRun+"/attempts/1")
	var run object
	if err != nil || json.Unmarshal(data, &run) != nil || text(run, "status") != "completed" || text(run, "conclusion") != "success" ||
		text(run, "head_sha") != record.Source || text(run, "head_branch") != "main" || text(run, "event") != "workflow_dispatch" ||
		value(run, "run_attempt") != float64(1) || !reviewedWorkflowPath(text(run, "path")) {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	binding.PreparationUID = text(cm, "metadata", "uid")
	return binding, nil
}
