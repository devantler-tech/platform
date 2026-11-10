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
}

// ListBlockLayout reads the block layout twice and reports it only when both
// complete reads agree.
func (n Native) ListBlockLayout(ctx context.Context) (BlockLayoutReport, error) {
	return n.listBlockLayout(ctx, false)
}

// ListRetainedBlockLayout additionally joins the configured r-longhorn status
// to its physical partition and compares the byte extent in both reads.
func (n Native) ListRetainedBlockLayout(ctx context.Context) (BlockLayoutReport, error) {
	return n.listBlockLayout(ctx, true)
}

func (n Native) listBlockLayout(ctx context.Context, retainRaw bool) (BlockLayoutReport, error) {
	ctx, cancel, err := n.requestContext(ctx)
	if err != nil {
		return BlockLayoutReport{}, nativeFailure{code: "request"}
	}
	defer cancel()
	if n.Blocks == nil {
		return BlockLayoutReport{}, nativeFailure{code: "block-client"}
	}
	first, _, err := n.readBlockLayout(ctx, retainRaw)
	if err != nil {
		return BlockLayoutReport{}, err
	}
	second, report, err := n.readBlockLayout(ctx, retainRaw)
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
func (n Native) readBlockLayout(ctx context.Context, retainRaw bool) ([]byte, BlockLayoutReport, error) {
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
	var rawStatus *block.VolumeStatusSpec
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
		default:
			if strings.HasPrefix(id, "r-") || strings.HasPrefix(id, "u-") {
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
	var rawPartition *block.DiscoveredVolumeSpec
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
		if !appendRow(struct {
			Kind, ID string
			Metadata any
		}{"discovered", id, []any{s.DevPath, s.ParentDevPath, s.PartitionLabel, s.PartitionUUID, s.Size}}) {
			return nil, BlockLayoutReport{}, invalid
		}
	}
	if retainRaw {
		if report.RawDiscoveredPartitions != 1 || !report.RawLUKS2 || !report.RawLockedToState ||
			!report.RawNodeIDOnly || !retainedRawMatches(rawStatus, rawPartition) ||
			!appendRow(struct {
				Kind, FilesystemUUID string
				Offset               uint64
			}{"retained-extent", rawPartition.UUID, rawPartition.Offset}) {
			return nil, BlockLayoutReport{}, invalid
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

func retainedRawMatches(status *block.VolumeStatusSpec, partition *block.DiscoveredVolumeSpec) bool {
	return status != nil && partition != nil && status.Phase == block.VolumePhaseReady &&
		status.Type == block.VolumeTypePartition && partition.Type == "partition" && partition.Name == "luks" && partition.UUID != "" &&
		status.Location == partition.DevPath && status.ParentLocation != "" && status.ParentLocation == partition.ParentDevPath &&
		status.PartitionUUID != "" && status.PartitionUUID == partition.PartitionUUID &&
		status.PartitionIndex > 0 && uint(status.PartitionIndex) == partition.PartitionIndex &&
		status.Size == 96<<30 && status.Size == partition.Size &&
		partition.Offset > 0 && partition.Offset%512 == 0 && partition.Offset <= math.MaxUint64-partition.Size
}
