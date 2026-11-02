package main

import (
	"testing"

	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func temporaryPartitionFixture() (cosiresource.List, cosiresource.List, string) {
	eph := block.NewVolumeStatus(block.NamespaceName, "EPHEMERAL")
	*eph.TypedSpec() = block.VolumeStatusSpec{Phase: block.VolumePhaseReady, Type: block.VolumeTypePartition,
		UUID: "ephemeral-original", Location: "/dev/sda6", ParentLocation: "/dev/sda", MountLocation: "/dev/dm-0", Size: 150 << 30,
		EncryptionProvider: block.EncryptionProviderLUKS2, EncryptionLockedToState: true, ConfiguredEncryptionKeys: []string{"nodeID"}}
	raw := block.NewVolumeStatus(block.NamespaceName, "r-longhorn")
	raw.TypedSpec().Phase = block.VolumePhaseWaiting
	physical := block.NewDiscoveredVolume(block.NamespaceName, "sda6")
	*physical.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/sda6", ParentDevPath: "/dev/sda", PartitionLabel: "EPHEMERAL", PartitionUUID: "partition-original", Size: 150 << 30}
	return cosiresource.List{Items: []cosiresource.Resource{eph, raw}}, cosiresource.List{Items: []cosiresource.Resource{physical}}, eph.TypedSpec().UUID
}

func TestNativeTemporaryPartitionRequiresPositiveOriginalAndNoRawStorage(t *testing.T) {
	statuses, discovered, uuid := temporaryPartitionFixture()
	if err := verifyUnusedTemporaryPartition(statuses, discovered, uuid); err != nil {
		t.Fatal("complete encrypted source partition with no raw storage was rejected:", err)
	}
	for name, mutate := range map[string]func(*cosiresource.List, *cosiresource.List){
		"missing original":    func(s, _ *cosiresource.List) { s.Items = s.Items[1:] },
		"empty discovery":     func(_, d *cosiresource.List) { d.Items = nil },
		"duplicate status":    func(s, _ *cosiresource.List) { s.Items = append(s.Items, s.Items[0]) },
		"duplicate discovery": func(_, d *cosiresource.List) { d.Items = append(d.Items, d.Items[0]) },
		"wrong status type":   func(s, d *cosiresource.List) { s.Items = append(s.Items, d.Items[0]) },
		"wrong namespace": func(s, _ *cosiresource.List) {
			r := block.NewVolumeStatus("other", "EPHEMERAL")
			*r.TypedSpec() = *s.Items[0].(*block.VolumeStatus).TypedSpec()
			s.Items[0] = r
		},
		"tearing down":      func(s, _ *cosiresource.List) { s.Items[0].Metadata().SetPhase(cosiresource.PhaseTearingDown) },
		"changed partition": func(s, _ *cosiresource.List) { s.Items[0].(*block.VolumeStatus).TypedSpec().UUID = "replacement" },
		"unencrypted source": func(s, _ *cosiresource.List) {
			s.Items[0].(*block.VolumeStatus).TypedSpec().EncryptionProvider = block.EncryptionProviderNone
		},
		"unlocked to state": func(s, _ *cosiresource.List) {
			s.Items[0].(*block.VolumeStatus).TypedSpec().EncryptionLockedToState = false
		},
		"different physical source": func(_, d *cosiresource.List) { d.Items[0].(*block.DiscoveredVolume).TypedSpec().DevPath = "/dev/sdb6" },
		"raw allocated": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().Phase = block.VolumePhaseReady
		},
		"raw location while waiting": func(s, _ *cosiresource.List) { s.Items[1].(*block.VolumeStatus).TypedSpec().Location = "/dev/sda7" },
		"raw missing phase": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().Phase = block.VolumePhaseMissing
		},
		"foreign pending volume": func(s, _ *cosiresource.List) {
			s.Items = append(s.Items, block.NewVolumeStatus(block.NamespaceName, "r-other"))
		},
		"raw physical partition": func(_, d *cosiresource.List) {
			r := block.NewDiscoveredVolume(block.NamespaceName, "sda7")
			*r.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/sda7", ParentDevPath: "/dev/sda", PartitionLabel: "r-longhorn", Size: 96 << 30}
			d.Items = append(d.Items, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, d, uuid := temporaryPartitionFixture()
			mutate(&s, &d)
			if verifyUnusedTemporaryPartition(s, d, uuid) == nil {
				t.Fatal("unknown or existing storage was accepted as empty")
			}
		})
	}
}
