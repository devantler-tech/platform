package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

const (
	temporaryReserveObservation = "observe-retained-v2-capacity"
	temporaryReserveSelection   = "temporary-v2-1"
)

func authorizeTemporaryReserveObservation(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != temporaryReserveObservation || getenv("LONGHORN_V2_NODE") != temporaryReserveSelection ||
		getenv("LONGHORN_V2_DATABASE") != "" {
		return errTemporaryActivation
	}
	return authorizeWorkflow(getenv, temporaryReserveObservation+":"+temporaryReserveSelection)
}

var reserveWorkflowAttempt = regexp.MustCompile(`^repos/devantler-tech/platform/actions/runs/[1-9][0-9]{0,18}/attempts/1$`)

func temporaryReserveReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !temporaryReserveReadAllowed(program, args) {
		return nil, errors.New("the retained storage observation only permits scoped reads")
	}
	return execute(ctx, nil, program, args...)
}

func temporaryReserveReadAllowed(program string, args []string) bool {
	switch program {
	case "git":
		return inventoryReadAllowed(program, args...)
	case "gh":
		return len(args) == 2 && args[0] == "api" && reserveWorkflowAttempt.MatchString(args[1])
	case "kubectl":
		if len(args) < 5 || !slices.Equal(args[:3], []string{"--context=admin@prod", "--request-timeout=20s", "get"}) {
			return false
		}
		switch args[3] {
		case "nodes":
			return slices.Equal(args[4:], []string{"--output=json"})
		case "node":
			return len(args) == 6 && inventoryTemporaryNode.MatchString(args[4]) && args[5] == "--output=json"
		case "pods":
			return slices.Equal(args[4:], []string{"--output=json", "--all-namespaces"})
		case "nodes.longhorn.io", "volumes.longhorn.io", "replicas.longhorn.io", "engines.longhorn.io", "orphans.longhorn.io", "instancemanagers.longhorn.io":
			return slices.Equal(args[4:], []string{"--output=json", "--namespace=longhorn-system"})
		case "configmap":
			if len(args) < 7 {
				return false
			}
			if args[4] == temporaryActivationJournal {
				return slices.Equal(args[5:], []string{"--output=json", "--namespace=longhorn-system", "--ignore-not-found"})
			}
			return slices.Contains(temporaryV2Selections[:], strings.TrimPrefix(args[4], "longhorn-v2-prepare-")) &&
				strings.HasPrefix(args[4], "longhorn-v2-prepare-") && slices.Equal(args[5:], []string{"--output=json", "--namespace=longhorn-system"})
		case "settings.longhorn.io":
			if len(args) != 7 || !slices.Equal(args[5:], []string{"--output=json", "--namespace=longhorn-system"}) {
				return false
			}
			return slices.Contains([]string{
				"v1-data-engine", "v2-data-engine", "guaranteed-instance-manager-cpu",
				"data-engine-cpu-mask", "data-engine-number-of-cpu-cores", "data-engine-cpu-isolation-enabled",
				"data-engine-interrupt-mode-enabled", "data-engine-hugepage-enabled", "data-engine-memory-size",
				"storage-over-provisioning-percentage", "storage-minimal-available-percentage",
			}, args[4])
		}
	case "talosctl":
		if len(args) < 4 || !strings.HasPrefix(args[0], "--nodes=") || args[1] != "get" {
			return false
		}
		address, err := netip.ParseAddr(strings.TrimPrefix(args[0], "--nodes="))
		if err != nil || !netip.MustParsePrefix("10.0.0.0/16").Contains(address) {
			return false
		}
		switch args[2] {
		case "volumestatus":
			return len(args) == 5 && slices.Contains([]string{"r-longhorn", temporaryRawID(temporaryReserveSelection), "EPHEMERAL"}, args[3]) &&
				args[4] == "--output=json"
		case "volumeconfig", "discoveredvolume":
			return slices.Equal(args[3:], []string{"--output=json"})
		case "systeminformation":
			return slices.Equal(args[3:], []string{"systeminformation", "--output=json"})
		}
	}
	return false
}

func (a temporaryV2Activation) observeUnusedReserve(ctx context.Context) error {
	if a.c.context != "admin@prod" || !validBenchmarkOwner(a.owner, a.source) {
		return errTemporaryActivation
	}
	registered, err := a.observeRecoveryStorage(ctx, nil)
	if err != nil {
		return err
	}
	if len(registered.Disks) != len(temporaryV2Selections) {
		return errTemporaryActivation
	}
	current := registered.Targets[temporaryReserveSelection]
	config, err := a.c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-prepare-"+temporaryReserveSelection, false)
	if err != nil || text(config, "metadata", "uid") != current.PreparationUID {
		return errTemporaryActivation
	}
	record, err := preparedTemporaryBinding(config, current)
	if err != nil {
		return err
	}
	worker, err := a.c.get(ctx, "node", "", current.Name, false)
	if err != nil {
		return err
	}
	address, err := verifyNode(worker, record, true)
	if err != nil || a.c.verifyHost(ctx, address, worker) != nil {
		return errTemporaryActivation
	}
	probe := temporaryPreparation{preparation: preparation{c: a.c, j: record}, reader: a.reader}
	layout, err := probe.readRetainedRecovery(ctx, address, current.BootID, false)
	if err != nil {
		return err
	}
	for i := 0; i < 2; i++ {
		if err := a.readUnusedReserve(ctx, address, current, record, layout); err != nil {
			return err
		}
	}
	final, err := a.observeRecoveryStorage(ctx, nil)
	if err != nil || !recoveryJSONEqual(registered, final) {
		return errTemporaryActivation
	}
	sealed, err := a.c.get(ctx, "configmap", "longhorn-system", text(config, "metadata", "name"), false)
	if err != nil || benchmarkIdentity(config) != benchmarkIdentity(sealed) ||
		text(config, "metadata", "resourceVersion") != text(sealed, "metadata", "resourceVersion") ||
		text(config, "data", "record") != text(sealed, "data", "record") {
		return errTemporaryActivation
	}
	return sourceCurrent(ctx, a.c.run, a.source)
}

func (a temporaryV2Activation) readUnusedReserve(ctx context.Context, address string, current temporaryV2Binding, record journal, layout longhorninventory.RetainedLayoutIdentity) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := a.c.talosVolume(ctx, address, "r-longhorn")
	if err != nil {
		return err
	}
	stage, err := a.c.talosVolume(ctx, address, temporaryRawID(current.Selection))
	if err != nil {
		return err
	}
	ephemeral, err := a.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil {
		return err
	}
	payload, err := a.c.run(ctx, nil, "talosctl", "--nodes="+address, "get", "discoveredvolume", "--output=json")
	if err != nil || len(payload) == 0 || len(payload) > 2<<20 {
		return errTemporaryActivation
	}
	discovered, err := decodeTemporaryReserveDevices(payload)
	if err != nil {
		return err
	}
	return verifyUnusedTemporaryReserve(current, record, layout, raw, stage, ephemeral, discovered)
}

func decodeTemporaryReserveDevices(payload []byte) ([]object, error) {
	reader := json.NewDecoder(bytes.NewReader(payload))
	discovered := []object{}
	seen := map[string]bool{}
	for {
		var row object
		err := reader.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		id := text(row, "metadata", "id")
		if err != nil || len(discovered) == 512 || id == "" || seen[id] ||
			text(row, "metadata", "namespace") != string(block.NamespaceName) || text(row, "metadata", "type") != string(block.DiscoveredVolumeType) {
			return nil, errTemporaryActivation
		}
		discovered = append(discovered, row)
		seen[id] = true
	}
	return discovered, nil
}

// Verify the original extent on the single prepared staging worker. This
// only establishes disk identity. Registration still requires an exclusive
// allocation, a fresh native read bracket, and multi-disk scheduler checks.
func verifyUnusedTemporaryReserve(current temporaryV2Binding, prepared journal, layout longhorninventory.RetainedLayoutIdentity,
	raw, stage, ephemeral object, discovered []object) error {
	unverified := errors.New("the unused temporary storage extent is unverified")
	if current.Selection != "temporary-v2-1" || prepared.Schema != 3 || prepared.Phase != "prepared" ||
		!prepared.StagingLayout || prepared.TemporarySelection != current.Selection || prepared.Target != current.Name ||
		prepared.NodeUID != current.NodeUID || prepared.ProviderID != current.ProviderID || prepared.MachineUUID != current.MachineUUID ||
		prepared.LonghornUID != current.LonghornUID || len(prepared.OriginalDisks) != 0 ||
		!inventoryBoot.MatchString(prepared.OriginalBootID) || prepared.OriginalBootID == current.BootID ||
		!retentionDigest.MatchString(prepared.RetainedExtentDigest) || layout.ExtentDigest != prepared.RetainedExtentDigest ||
		!isolationUID.MatchString(prepared.RetainedRawUUID) || layout.PartitionUUID != prepared.RetainedRawUUID ||
		layout.EphemeralUUID != current.EphemeralUUID || !layout.StageStatusPresent || layout.StagePartitionUUID != current.RawPartitionUUID ||
		prepared.PreparedRawUUID != current.RawPartitionUUID || prepared.RetainedRawUUID == prepared.PreparedRawUUID {
		return unverified
	}

	rawMapper := text(raw, "spec", "mountLocation")
	if verifyNamedEncryptedRawDisk(raw, ephemeral, "r-longhorn", permanentRawDiskGiB) != nil ||
		verifyNamedEncryptedRawDisk(stage, ephemeral, temporaryRawID(current.Selection), stagingRawDiskGiB) != nil ||
		text(raw, "spec", "partitionUUID") != prepared.RetainedRawUUID ||
		text(stage, "spec", "partitionUUID") != prepared.PreparedRawUUID ||
		text(raw, "spec", "location") == text(stage, "spec", "location") || rawMapper == text(stage, "spec", "mountLocation") {
		return unverified
	}

	matches := 0
	for _, device := range discovered {
		if text(device, "spec", "dev_path") != rawMapper {
			continue
		}
		if text(device, "metadata", "type") != string(block.DiscoveredVolumeType) || text(device, "spec", "type") != "disk" ||
			!absentOrEmptyString(value(device, "spec", "name")) || !absentOrEmptyString(value(device, "spec", "label")) ||
			!absentOrEmptyString(value(device, "spec", "uuid")) {
			return unverified
		}
		matches++
	}
	if matches != 1 {
		return unverified
	}
	return nil
}
