package longhorninventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"

	resource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// BlockLayoutReport summarizes the EPHEMERAL and raw Longhorn volumes' phase,
// size and encryption properties, without device paths or identities.
type BlockLayoutReport struct {
	VolumeStatuses, DiscoveredVolumes                           int
	EphemeralPhase                                              string
	EphemeralSizeBytes                                          uint64
	EphemeralLUKS2, EphemeralLockedToState, EphemeralNodeIDOnly bool
	RawPresent                                                  bool
	RawPhase                                                    string
	RawSizeBytes                                                uint64
	RawLUKS2, RawLockedToState, RawNodeIDOnly                   bool
	RawDiscoveredPartitions, OtherUserVolumes                   int
	RawExtentObserved                                           bool `json:",omitempty"`
	Digest                                                      string
	retained                                                    RetainedLayoutIdentity
}

// RetainedLayoutIdentity binds the raw and STATE physical extents across
// boots. It is private journal evidence, separate from the public report.
type RetainedLayoutIdentity struct {
	PartitionUUID, ExtentDigest string
	StagePartitionUUID          string
	StageStatusPresent          bool
	EphemeralUUID               string
}

type blockLayoutMode int

const (
	ordinaryLayout blockLayoutMode = iota
	retainedOpen
	retainedClosed
)

// ListBlockLayout reads the block layout twice and reports it only when both
// complete reads agree.
func (n Native) ListBlockLayout(ctx context.Context) (BlockLayoutReport, error) {
	return n.listBlockLayout(ctx, ordinaryLayout)
}

// ListRetainedBlockLayout compares the raw and STATE extents and the expected
// raw-volume controller state in both reads.
func (n Native) ListRetainedBlockLayout(ctx context.Context, closed bool) (BlockLayoutReport, RetainedLayoutIdentity, error) {
	mode := retainedOpen
	if closed {
		mode = retainedClosed
	}
	report, err := n.listBlockLayout(ctx, mode)
	identity := report.retained
	report.retained = RetainedLayoutIdentity{}
	return report, identity, err
}

func (n Native) listBlockLayout(ctx context.Context, mode blockLayoutMode) (BlockLayoutReport, error) {
	ctx, cancel, err := n.requestContext(ctx)
	if err != nil {
		return BlockLayoutReport{}, nativeFailure{code: "request"}
	}
	defer cancel()
	if n.Blocks == nil {
		return BlockLayoutReport{}, nativeFailure{code: "block-client"}
	}
	first, _, err := n.readBlockLayout(ctx, mode)
	if err != nil {
		return BlockLayoutReport{}, err
	}
	second, report, err := n.readBlockLayout(ctx, mode)
	if err != nil {
		return BlockLayoutReport{}, err
	}
	if !active(ctx) || !bytes.Equal(first, second) {
		return BlockLayoutReport{}, nativeFailure{code: "block-change"}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return BlockLayoutReport{}, nativeFailure{code: "block-shape"}
	}
	digest := sha256.Sum256(encoded)
	report.Digest = hex.EncodeToString(digest[:])
	return report, nil
}

// Selected layout metadata is compared privately. The public commitment covers
// only reported counters and properties, without private device identities,
// key material, configuration contents, filesystem contents or provider errors.
func (n Native) readBlockLayout(ctx context.Context, mode blockLayoutMode) ([]byte, BlockLayoutReport, error) {
	if !active(ctx) {
		return nil, BlockLayoutReport{}, nativeFailure{code: "block-list"}
	}
	statuses, err := n.Blocks.List(ctx, block.NewVolumeStatus(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) {
		return nil, BlockLayoutReport{}, nativeFailure{code: "block-list"}
	}
	discovered, err := n.Blocks.List(ctx, block.NewDiscoveredVolume(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) {
		return nil, BlockLayoutReport{}, nativeFailure{code: "block-list"}
	}
	invalid := nativeFailure{code: "block-shape"}
	if len(statuses.Items) == 0 || len(statuses.Items) > 128 || len(discovered.Items) == 0 || len(discovered.Items) > 512 {
		return nil, BlockLayoutReport{}, invalid
	}
	report := BlockLayoutReport{VolumeStatuses: len(statuses.Items), DiscoveredVolumes: len(discovered.Items), RawPhase: "absent"}
	rows := make([][]byte, 0, len(statuses.Items)+len(discovered.Items))
	metadataBytes := 0
	appendRow := func(row any) bool {
		b, err := json.Marshal(row)
		metadataBytes += len(b)
		if err != nil || metadataBytes > MaxMetadataBytes {
			return false
		}
		rows = append(rows, b)
		return true
	}
	seen := map[string]bool{}
	ephFound := false
	var ephemeralUUID string
	var rawStatus, stageStatus, ephemeralStatus *block.VolumeStatusSpec
	for _, item := range statuses.Items {
		v, ok := item.(*block.VolumeStatus)
		if !ok || v == nil || v.Metadata().Namespace() != block.NamespaceName || v.Metadata().Phase() != resource.PhaseRunning || v.Metadata().ID() == "" || len(v.Metadata().ID()) > 256 || seen[v.Metadata().ID()] {
			return nil, BlockLayoutReport{}, invalid
		}
		id, s := v.Metadata().ID(), v.TypedSpec()
		seen[id] = true
		if s == nil {
			return nil, BlockLayoutReport{}, invalid
		}
		phase := "other"
		switch s.Phase {
		case block.VolumePhaseReady:
			phase = "ready"
		case block.VolumePhaseWaiting:
			phase = "waiting"
		case block.VolumePhaseFailed:
			phase = "failed"
		case block.VolumePhaseMissing:
			phase = "missing"
		case block.VolumePhaseLocated:
			phase = "located"
		case block.VolumePhaseProvisioned:
			phase = "provisioned"
		case block.VolumePhasePrepared:
			phase = "prepared"
		case block.VolumePhaseClosed:
			phase = "closed"
		}
		nodeIDOnly := len(s.ConfiguredEncryptionKeys) == 1 && s.ConfiguredEncryptionKeys[0] == "nodeID"
		switch id {
		case "EPHEMERAL":
			ephFound = true
			ephemeralUUID = s.UUID
			ephemeralStatus = s
			report.EphemeralPhase = phase
			report.EphemeralSizeBytes = s.Size
			report.EphemeralLUKS2 = s.EncryptionProvider == block.EncryptionProviderLUKS2
			report.EphemeralLockedToState = s.EncryptionLockedToState
			report.EphemeralNodeIDOnly = nodeIDOnly
		case "r-longhorn":
			rawStatus = s
			report.RawPresent = true
			report.RawPhase = phase
			report.RawSizeBytes = s.Size
			report.RawLUKS2 = s.EncryptionProvider == block.EncryptionProviderLUKS2
			report.RawLockedToState = s.EncryptionLockedToState
			report.RawNodeIDOnly = nodeIDOnly
		case "r-longhorn-stage":
			stageStatus = s
			report.OtherUserVolumes++
		default:
			if strings.HasPrefix(id, "r-") || strings.HasPrefix(id, "u-") {
				if mode != ordinaryLayout {
					return nil, BlockLayoutReport{}, invalid
				}
				report.OtherUserVolumes++
			}
		}
		if !appendRow(struct {
			Kind, ID string
			Metadata any
		}{"status", id, []any{s.Phase, s.Type, s.Location, s.MountLocation, s.ParentLocation, s.UUID, s.PartitionUUID, s.PartitionIndex, s.Size, s.EncryptionProvider, s.EncryptionLockedToState, s.ConfiguredEncryptionKeys}}) {
			return nil, BlockLayoutReport{}, invalid
		}
	}
	if !ephFound {
		return nil, BlockLayoutReport{}, invalid
	}
	seen = map[string]bool{}
	var rawPartition, statePartition, stagePartition, ephemeralPartition *block.DiscoveredVolumeSpec
	for _, item := range discovered.Items {
		v, ok := item.(*block.DiscoveredVolume)
		if !ok || v == nil || v.Metadata().Namespace() != block.NamespaceName || v.Metadata().Phase() != resource.PhaseRunning || v.Metadata().ID() == "" || len(v.Metadata().ID()) > 256 || seen[v.Metadata().ID()] {
			return nil, BlockLayoutReport{}, invalid
		}
		id, s := v.Metadata().ID(), v.TypedSpec()
		seen[id] = true
		if s == nil || s.DevPath == "" || s.Size == 0 {
			return nil, BlockLayoutReport{}, invalid
		}
		if s.PartitionLabel == "r-longhorn" {
			report.RawDiscoveredPartitions++
			rawPartition = s
		}
		if mode != ordinaryLayout && s.PartitionLabel == "STATE" {
			if statePartition != nil {
				return nil, BlockLayoutReport{}, invalid
			}
			statePartition = s
		}
		if mode != ordinaryLayout && s.PartitionLabel == "EPHEMERAL" {
			if ephemeralPartition != nil {
				return nil, BlockLayoutReport{}, invalid
			}
			ephemeralPartition = s
		}
		if mode != ordinaryLayout && s.PartitionLabel != "r-longhorn" &&
			(strings.HasPrefix(s.PartitionLabel, "r-") || strings.HasPrefix(s.PartitionLabel, "u-")) {
			if s.PartitionLabel != "r-longhorn-stage" || stagePartition != nil {
				return nil, BlockLayoutReport{}, invalid
			}
			stagePartition = s
		}
		if !appendRow(struct {
			Kind, ID string
			Metadata any
		}{"discovered", id, []any{s.DevPath, s.ParentDevPath, s.PartitionLabel, s.PartitionUUID, s.Size}}) {
			return nil, BlockLayoutReport{}, invalid
		}
	}
	if mode != ordinaryLayout {
		if ephemeralUUID == "" || report.RawDiscoveredPartitions != 1 || !validRetainedRaw(rawPartition) ||
			!retainedStateMatches(statePartition, rawPartition) || !retainedEphemeralMatches(ephemeralStatus, ephemeralPartition, rawPartition, statePartition) {
			return nil, BlockLayoutReport{}, invalid
		}
		if (mode == retainedOpen && (!report.RawLUKS2 || !report.RawLockedToState || !report.RawNodeIDOnly ||
			!retainedRawMatches(rawStatus, rawPartition))) || (mode == retainedClosed && rawStatus != nil) {
			return nil, BlockLayoutReport{}, invalid
		}
		if mode == retainedClosed {
			if err := n.verifyRetainedConfigClosed(ctx); err != nil {
				return nil, BlockLayoutReport{}, err
			}
			if err := n.verifyEmptyPartitionHolders(ctx, rawPartition.DevPath); err != nil {
				return nil, BlockLayoutReport{}, err
			}
		}
		extent := []any{"retained-extent", rawPartition.ParentDevPath,
			rawPartition.PartitionUUID, rawPartition.UUID, rawPartition.PartitionType, rawPartition.Offset, rawPartition.Size,
			statePartition.PartitionUUID, statePartition.PartitionType, statePartition.Offset, statePartition.Size}
		encoded, err := json.Marshal(extent)
		if err != nil || !appendRow(extent) || !appendRow([]any{"ephemeral-extent", ephemeralPartition.Offset}) ||
			!validStagedPartition(stageStatus, stagePartition, rawPartition, statePartition) ||
			(stagePartition != nil && (stagePartition.PartitionUUID == ephemeralPartition.PartitionUUID || !extentsDisjoint(stagePartition, ephemeralPartition))) {
			return nil, BlockLayoutReport{}, invalid
		}
		digest := sha256.Sum256(encoded)
		report.retained = RetainedLayoutIdentity{PartitionUUID: rawPartition.PartitionUUID, ExtentDigest: hex.EncodeToString(digest[:])}
		report.retained.EphemeralUUID = ephemeralUUID
		report.retained.StageStatusPresent = stageStatus != nil
		if stagePartition != nil {
			report.retained.StagePartitionUUID = stagePartition.PartitionUUID
			if !appendRow([]any{"staging-extent", stagePartition.UUID, stagePartition.PartitionType, stagePartition.Offset}) {
				return nil, BlockLayoutReport{}, invalid
			}
		}
		report.RawExtentObserved = true
	}
	slices.SortFunc(rows, bytes.Compare)
	encoded, err := json.Marshal(rows)
	if err != nil || len(encoded) > MaxMetadataBytes || !active(ctx) {
		return nil, BlockLayoutReport{}, invalid
	}
	return encoded, report, nil
}

func (n Native) verifyRetainedConfigClosed(ctx context.Context) error {
	refused := nativeFailure{code: "block-config"}
	configs, err := n.Blocks.List(ctx, block.NewVolumeConfig(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) || len(configs.Items) == 0 || len(configs.Items) > 128 {
		return refused
	}
	seen := map[string]bool{}
	for _, item := range configs.Items {
		v, ok := item.(*block.VolumeConfig)
		if !ok || v == nil || v.Metadata().Namespace() != block.NamespaceName || v.Metadata().Phase() != resource.PhaseRunning {
			return refused
		}
		id := v.Metadata().ID()
		if id == "" || id == "r-longhorn" || seen[id] {
			return refused
		}
		seen[id] = true
	}
	if !seen["r-longhorn-stage"] || !seen["EPHEMERAL"] {
		return refused
	}
	return nil
}

func validRetainedRaw(partition *block.DiscoveredVolumeSpec) bool {
	return partition != nil && partition.Type == "partition" && partition.Name == "luks" && partition.UUID != "" &&
		partition.PartitionUUID != "" && partition.ParentDevPath != "" && partition.Size == 96<<30 && validPartitionExtent(partition)
}

func retainedRawMatches(status *block.VolumeStatusSpec, partition *block.DiscoveredVolumeSpec) bool {
	return status != nil && status.Phase == block.VolumePhaseReady && status.Type == block.VolumeTypePartition &&
		status.Location == partition.DevPath && status.ParentLocation == partition.ParentDevPath &&
		strings.HasPrefix(status.MountLocation, "/dev/dm-") &&
		status.PartitionUUID == partition.PartitionUUID && status.PartitionIndex > 0 &&
		uint(status.PartitionIndex) == partition.PartitionIndex && status.Size == partition.Size
}

func retainedStateMatches(state, raw *block.DiscoveredVolumeSpec) bool {
	return state != nil && state.Type == "partition" && state.PartitionUUID != "" && state.PartitionUUID != raw.PartitionUUID &&
		state.DevPath != raw.DevPath && state.ParentDevPath == raw.ParentDevPath && validPartitionExtent(state) &&
		extentsDisjoint(state, raw)
}

func retainedEphemeralMatches(status *block.VolumeStatusSpec, ephemeral, raw, state *block.DiscoveredVolumeSpec) bool {
	return ephemeral != nil && ephemeral.Type == "partition" && ephemeral.PartitionUUID != "" &&
		ephemeral.PartitionUUID != raw.PartitionUUID && ephemeral.PartitionUUID != state.PartitionUUID &&
		ephemeral.ParentDevPath == raw.ParentDevPath && validPartitionExtent(ephemeral) &&
		extentsDisjoint(ephemeral, raw) && extentsDisjoint(ephemeral, state) && retainedRawMatches(status, ephemeral)
}

func validStagedPartition(status *block.VolumeStatusSpec, stage, raw, state *block.DiscoveredVolumeSpec) bool {
	if stage == nil {
		if status == nil {
			return true
		}
		waiting := status.Phase == block.VolumePhaseWaiting || status.Phase == block.VolumePhaseFailed &&
			(status.PreFailPhase == block.VolumePhaseWaiting || status.PreFailPhase == block.VolumePhaseMissing)
		return waiting && status.Type == block.VolumeTypePartition && status.Size == 0 && status.Location == "" &&
			status.MountLocation == "" && status.ParentLocation == "" && status.UUID == "" && status.PartitionUUID == "" && status.PartitionIndex == 0
	}
	return stage.Type == "partition" && stage.Name == "luks" && stage.UUID != "" && stage.Size == 144<<30 &&
		stage.PartitionUUID != "" && stage.PartitionUUID != raw.PartitionUUID && stage.PartitionUUID != state.PartitionUUID &&
		stage.ParentDevPath == raw.ParentDevPath && validPartitionExtent(stage) && extentsDisjoint(stage, raw) && extentsDisjoint(stage, state) &&
		retainedRawMatches(status, stage) && status.EncryptionProvider == block.EncryptionProviderLUKS2 && status.EncryptionLockedToState &&
		len(status.ConfiguredEncryptionKeys) == 1 && status.ConfiguredEncryptionKeys[0] == "nodeID"
}

func extentsDisjoint(a, b *block.DiscoveredVolumeSpec) bool {
	return a.Offset+a.Size <= b.Offset || b.Offset+b.Size <= a.Offset
}

func validPartitionExtent(p *block.DiscoveredVolumeSpec) bool {
	return p.Offset > 0 && p.Offset%512 == 0 && p.Size > 0 && p.Size%512 == 0 && p.Offset <= math.MaxUint64-p.Size
}
