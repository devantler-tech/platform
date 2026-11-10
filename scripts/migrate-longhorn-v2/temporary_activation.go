package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

type temporaryV2Binding = longhornmigration.ActivationBinding

var errTemporaryActivation = errors.New("temporary raw storage identity or registration ownership is unproven")

const temporaryActivationOwner = longhornmigration.ActivationOwner

// authorizeTemporaryActivation requires a distinct confirmation for registering
// the fixed three-disk pool through the protected first-attempt main workflow.
func authorizeTemporaryActivation(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != "register-temporary-v2-storage" {
		return errTemporaryActivation
	}
	return authorizeWorkflow(getenv, "register-longhorn-v2-temporary-storage")
}

type temporaryV2Activation struct {
	c             client
	source, owner string
	pools         map[string]migrationPool
	reader        inventoryReaderFactory
}

// readBinding brackets the exact native partition reads with hardware and boot
// identity, then repeats the Kubernetes projection. An incomplete read or failed
// close cannot produce a binding for a subsequent registration mutation.
func (a temporaryV2Activation) readBinding(ctx context.Context, selection string, node, storage object, pool migrationPool) (binding temporaryV2Binding, err error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if !commandContextActive(ctx) {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	address, err := verifyNode(node, journal{Target: text(node, "metadata", "name"), NodeUID: text(node, "metadata", "uid"), ProviderID: text(node, "spec", "providerID"), MachineUUID: text(node, "status", "nodeInfo", "systemUUID")}, true)
	if err != nil || a.c.verifyHost(ctx, address, node) != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	factory := a.reader
	if factory == nil {
		factory = newInventoryReader
	}
	reader, closeReader, err := factory(ctx, address)
	if err != nil || reader == nil || closeReader == nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	defer func() {
		if closeReader() != nil || !commandContextActive(ctx) {
			binding, err = temporaryV2Binding{}, errTemporaryActivation
		}
	}()
	boot, err := reader.BootID(ctx)
	if err != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	raw, err := a.c.talosVolume(ctx, address, "r-longhorn")
	if err != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	ephemeral, err := a.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	before, err := projectTemporaryV2Binding(selection, node, storage, raw, ephemeral, pool, boot)
	if err != nil {
		return temporaryV2Binding{}, err
	}
	fresh, err := a.c.get(ctx, "node", "", before.Name, false)
	if err != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	boot, err = reader.BootID(ctx)
	if err != nil || a.c.verifyHost(ctx, address, fresh) != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	after, err := projectTemporaryV2Binding(selection, fresh, storage, raw, ephemeral, pool, boot)
	if err != nil || before != after {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	return after, nil
}

// temporaryRegistrationState accepts an empty fence or this journal's exact
// partial activation. A matching disk path without the journal UID is not owned.
func temporaryRegistrationState(storage object, journalUID string) (bool, error) {
	owner := text(storage, "metadata", "annotations", temporaryActivationOwner)
	if owner == "" && unusedStorageFenced(storage) {
		return false, nil
	}
	if journalUID == "" || owner != journalUID || text(storage, "metadata", "labels", migrationEnabledLabel) != "true" ||
		value(storage, "spec", "allowScheduling") != true || value(storage, "spec", "evictionRequested") != false || !reviewedBlockDisks(value(storage, "spec", "disks")) {
		return false, errTemporaryActivation
	}
	return true, nil
}

// projectTemporaryV2Binding binds a fixed temporary selection to its pinned
// hardware and native encrypted partitions. Both legacy pool roles receive raw
// V2 storage; this profile never selects an original data-bearing worker.
func projectTemporaryV2Binding(selection string, node, storage, raw, ephemeral object, pool migrationPool, nativeBoot string) (temporaryV2Binding, error) {
	b := temporaryV2Binding{Selection: selection, Name: text(node, "metadata", "name"), NodeUID: text(node, "metadata", "uid"),
		ProviderID: text(node, "spec", "providerID"), MachineUUID: text(node, "status", "nodeInfo", "systemUUID"),
		BootID: text(node, "status", "nodeInfo", "bootID"), OSImage: text(node, "status", "nodeInfo", "osImage"),
		LonghornUID: text(storage, "metadata", "uid"), EphemeralUUID: text(ephemeral, "spec", "uuid"), RawPartitionUUID: text(raw, "spec", "partitionUUID")}
	if !inventoryTemporarySelection.MatchString(selection) || !strings.HasPrefix(selection, "temporary-"+pool.role+"-") ||
		!inventoryTemporaryNode.MatchString(b.Name) || verifyMigrationNode(node, pool) != nil || verifyEncryptedRawDisk(raw, ephemeral, stagingRawDiskGiB) != nil ||
		b.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(b.ProviderID) || b.MachineUUID == "" || b.OSImage == "" ||
		!inventoryBoot.MatchString(b.BootID) || nativeBoot != b.BootID || !inventoryBoot.MatchString(strings.ToLower(b.RawPartitionUUID)) || b.RawPartitionUUID == b.EphemeralUUID ||
		text(node, "status", "nodeInfo", "architecture") != "amd64" || !hugepagesReady(node) || text(node, "status", "allocatable", "cpu") != "8" ||
		value(node, "spec", "unschedulable") == true || text(node, "metadata", "annotations", fenceAnnotation) != "" ||
		text(node, "metadata", "annotations", capacityOwnerAnnotation) != capacityOwnerValue || text(node, "metadata", "annotations", scaleDownDisabled) != "true" ||
		text(node, "metadata", "labels", "node.longhorn.io/create-default-disk") != "" ||
		text(storage, "metadata", "name") != b.Name || text(storage, "metadata", "namespace") != "longhorn-system" || b.LonghornUID == "" ||
		text(storage, "metadata", "resourceVersion") == "" || value(storage, "metadata", "deletionTimestamp") != nil {
		return temporaryV2Binding{}, errTemporaryActivation
	}
	return b, nil
}
