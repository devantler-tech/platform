package longhorninventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// AttachedDiskReport contains only the observed geometry and receipt digest.
type AttachedDiskReport struct {
	SizeBytes  uint64
	SectorSize uint
	Digest     string
}

// AttachedDiskIdentity is private journal evidence, not a public report.
type AttachedDiskIdentity struct {
	DiskID, DevPath, SystemDiskID, Serial, WWID, BootID string
}

// ListAttachedDisk observes one candidate without preparing or formatting it.
// Two complete metadata and holder reads must agree within one boot. Provider
// ownership, source-census completeness, raw open descriptors, existing-volume
// preservation and recovery-key qualification remain the caller's obligations.
func (n Native) ListAttachedDisk(ctx context.Context, providerID string, sources []string, minimum uint64) (AttachedDiskReport, AttachedDiskIdentity, error) {
	if ctx == nil || n.Blocks == nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	ctx, cancel, err := n.requestContext(ctx)
	if err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	defer cancel()
	beforeBoot, err := n.BootID(ctx)
	if err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	first, err := n.readAttachedDisk(ctx, providerID, sources, minimum)
	if err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	if err = n.verifyEmptyPartitionHolders(ctx, first.DevPath); err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	second, err := n.readAttachedDisk(ctx, providerID, sources, minimum)
	if err != nil || first != second {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	if err = n.verifyEmptyPartitionHolders(ctx, second.DevPath); err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	afterBoot, err := n.BootID(ctx)
	if err != nil || beforeBoot != afterBoot || !active(ctx) {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	encoded, err := json.Marshal([]string{"attached-disk-observation/v1", n.Target, beforeBoot, first.Digest})
	if err != nil {
		return AttachedDiskReport{}, AttachedDiskIdentity{}, errNative
	}
	digest := sha256.Sum256(encoded)
	return AttachedDiskReport{SizeBytes: first.Size, SectorSize: first.SectorSize, Digest: hex.EncodeToString(digest[:])},
		AttachedDiskIdentity{DiskID: first.DiskID, DevPath: first.DevPath, SystemDiskID: first.SystemDiskID,
			Serial: first.Serial, WWID: first.WWID, BootID: beforeBoot}, nil
}

func (n Native) readAttachedDisk(ctx context.Context, providerID string, sources []string, minimum uint64) (attachedDiskCandidate, error) {
	disks, err := n.Blocks.List(ctx, block.NewDisk(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) {
		return attachedDiskCandidate{}, errNative
	}
	systems, err := n.Blocks.List(ctx, block.NewSystemDisk(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) {
		return attachedDiskCandidate{}, errNative
	}
	discovered, err := n.Blocks.List(ctx, block.NewDiscoveredVolume(block.NamespaceName, "").Metadata())
	if err != nil || !active(ctx) {
		return attachedDiskCandidate{}, errNative
	}
	return qualifyAttachedDiskCandidate(providerID, sources, minimum, disks, systems, discovered)
}
