package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// preparedTemporaryBinding validates a completed unused-storage reset journal
// against the current worker and its changed boot and EPHEMERAL partition.
func preparedTemporaryBinding(cm object, binding temporaryV2Binding) (journal, error) {
	name := "longhorn-v2-prepare-" + binding.Selection
	if text(cm, "metadata", "name") != name || text(cm, "metadata", "namespace") != "longhorn-system" ||
		text(cm, "metadata", "uid") == "" || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil {
		return journal{}, errTemporaryActivation
	}
	record, err := decodePreparationJournal([]byte(text(cm, "data", "record")))
	if err != nil {
		return journal{}, errTemporaryActivation
	}
	p := preparation{target: binding.Name, name: name, temporarySelection: binding.Selection}
	if p.validateRecord(record) != nil || (record.Schema != 2 && record.Schema != 3) || !record.StagingLayout || record.Phase != "prepared" || record.NodeUID != binding.NodeUID ||
		record.ProviderID != binding.ProviderID || record.MachineUUID != binding.MachineUUID || record.LonghornUID != binding.LonghornUID ||
		record.OriginalBootID == binding.BootID || strings.EqualFold(record.EphemeralUUID, binding.EphemeralUUID) {
		return journal{}, errTemporaryActivation
	}
	if record.PreparedRawUUID != "" && !strings.EqualFold(record.PreparedRawUUID, binding.RawPartitionUUID) {
		return journal{}, errTemporaryActivation
	}
	return record, nil
}

// prepared requires successful completion of the matching protected preparation
// before enabling a rebuilt worker's raw disk.
func (a temporaryV2Activation) prepared(ctx context.Context, binding temporaryV2Binding, expected temporaryV2Binding) (temporaryV2Binding, error) {
	if !inventoryTemporarySelection.MatchString(binding.Selection) {
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
	run, err := previousMigrationRun(ctx, a.c, record.OwnerRun, record.Source)
	if err != nil || text(run, "conclusion") != "success" {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	binding.PreparationUID = text(cm, "metadata", "uid")
	if record.PreparedRawUUID == "" && expected.PreparationUID == "" {
		// A legacy journal must be paired with an independently retained
		// post-preparation readback before the first activation journal exists.
		identity := strings.Join([]string{binding.Selection, record.Source, record.OwnerRun, record.NativeEmptyDigest, strings.ToLower(binding.RawPartitionUUID)}, "\n")
		if a.legacyReceipts[binding.Selection] != fmt.Sprintf("%x", sha256.Sum256([]byte(identity))) {
			return temporaryV2Binding{}, errLegacyPreparation
		}
	}
	return binding, nil
}

var errLegacyPreparation = errors.New("legacy preparation requires a matching independently retained post-preparation receipt")

var legacyPreparationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func parseLegacyPreparationReceipts(input string) (map[string]string, error) {
	receipts := map[string]string{}
	if input == "" {
		return receipts, nil
	}
	for _, field := range strings.Split(input, ",") {
		selection, digest, _ := strings.Cut(field, ":")
		if (selection != "temporary-v1-1" && selection != "temporary-v1-2") ||
			receipts[selection] != "" || !legacyPreparationDigest.MatchString(digest) {
			return nil, errLegacyPreparation
		}
		receipts[selection] = digest
	}
	return receipts, nil
}
