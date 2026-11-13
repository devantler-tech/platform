package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc/metadata"
)

func temporaryPartitionFixture() (cosiresource.List, cosiresource.List, string) {
	eph := block.NewVolumeStatus(block.NamespaceName, "EPHEMERAL")
	*eph.TypedSpec() = block.VolumeStatusSpec{Phase: block.VolumePhaseReady, Type: block.VolumeTypePartition,
		UUID: "ephemeral-original", Location: "/dev/sda6", ParentLocation: "/dev/sda", MountLocation: "/dev/dm-0", Size: 150 << 30,
		EncryptionProvider: block.EncryptionProviderLUKS2, EncryptionLockedToState: true, ConfiguredEncryptionKeys: []string{"nodeID"}}
	raw := block.NewVolumeStatus(block.NamespaceName, "r-longhorn")
	raw.TypedSpec().Phase = block.VolumePhaseWaiting
	raw.TypedSpec().Type = block.VolumeTypePartition
	physical := block.NewDiscoveredVolume(block.NamespaceName, "sda6")
	*physical.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/sda6", ParentDevPath: "/dev/sda", PartitionLabel: "EPHEMERAL", PartitionUUID: "partition-original", Size: 150 << 30}
	return cosiresource.List{Items: []cosiresource.Resource{eph, raw}}, cosiresource.List{Items: []cosiresource.Resource{physical}}, eph.TypedSpec().UUID
}

func TestFailedTemporaryAllocationRequiresNeverLocatedStateAndNoPhysicalPartition(t *testing.T) {
	for _, prior := range []block.VolumePhase{block.VolumePhaseWaiting, block.VolumePhaseMissing} {
		s, d, uuid := temporaryPartitionFixture()
		raw := s.Items[1].(*block.VolumeStatus).TypedSpec()
		raw.Phase, raw.PreFailPhase = block.VolumePhaseFailed, prior
		if err := verifyUnusedTemporaryPartition(s, d, uuid); err != nil {
			t.Fatalf("complete never-allocated failure was rejected: prior=%v err=%v", prior, err)
		}
	}
	for name, change := range map[string]func(*cosiresource.List, *cosiresource.List){
		"previously located": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PreFailPhase = block.VolumePhaseLocated
		},
		"previously provisioned": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PreFailPhase = block.VolumePhaseProvisioned
		},
		"previously prepared": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PreFailPhase = block.VolumePhasePrepared
		},
		"previously ready": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PreFailPhase = block.VolumePhaseReady
		},
		"unknown prior phase": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PreFailPhase = block.VolumePhase(99)
		},
		"wrong raw type": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().Type = block.VolumeTypeDisk
		},
		"allocated size":  func(s, _ *cosiresource.List) { s.Items[1].(*block.VolumeStatus).TypedSpec().Size = 96 << 30 },
		"device location": func(s, _ *cosiresource.List) { s.Items[1].(*block.VolumeStatus).TypedSpec().Location = "/dev/sda7" },
		"parent device": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().ParentLocation = "/dev/sda"
		},
		"mount location": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().MountLocation = "/dev/dm-1"
		},
		"filesystem identity": func(s, _ *cosiresource.List) { s.Items[1].(*block.VolumeStatus).TypedSpec().UUID = "existing-data" },
		"partition identity": func(s, _ *cosiresource.List) {
			s.Items[1].(*block.VolumeStatus).TypedSpec().PartitionUUID = "existing-partition"
		},
		"partition index": func(s, _ *cosiresource.List) { s.Items[1].(*block.VolumeStatus).TypedSpec().PartitionIndex = 7 },
		"discovered raw partition": func(_, d *cosiresource.List) {
			v := block.NewDiscoveredVolume(block.NamespaceName, "raw-device")
			*v.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/sda7", ParentDevPath: "/dev/sda", PartitionLabel: "r-longhorn", Size: 96 << 30}
			d.Items = append(d.Items, v)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, d, uuid := temporaryPartitionFixture()
			raw := s.Items[1].(*block.VolumeStatus).TypedSpec()
			raw.Phase, raw.PreFailPhase = block.VolumePhaseFailed, block.VolumePhaseWaiting
			change(&s, &d)
			if verifyUnusedTemporaryPartition(s, d, uuid) == nil {
				t.Fatal("failed allocation hid a previous device, unknown state or discovered data")
			}
		})
	}
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

type temporaryBlockReaderFixture struct {
	t                    *testing.T
	statuses, discovered cosiresource.List
	calls, failAt        int
	allocateAfterFirst   bool
}

func (r *temporaryBlockReaderFixture) Get(context.Context, cosiresource.Pointer, ...state.GetOption) (cosiresource.Resource, error) {
	r.t.Fatal("temporary storage proof attempted an unreviewed resource request")
	return nil, errInventory
}

func (r *temporaryBlockReaderFixture) List(ctx context.Context, kind cosiresource.Kind, options ...state.ListOption) (cosiresource.List, error) {
	r.t.Helper()
	r.calls++
	deadline, bounded := ctx.Deadline()
	md, _ := metadata.FromOutgoingContext(ctx)
	if !bounded || time.Until(deadline) > 20*time.Second || len(md.Get("node")) != 1 || md.Get("node")[0] != "10.0.99.4" || len(md.Get("nodes")) != 0 || kind.Namespace() != block.NamespaceName || len(options) != 0 {
		r.t.Fatal("native resource proof lost its bounded exact-node scope")
	}
	var result cosiresource.List
	if r.calls%2 == 1 {
		if kind.Type() != block.VolumeStatusType {
			r.t.Fatal("unexpected volume resource query")
		}
		if r.allocateAfterFirst && r.calls > 2 {
			r.statuses.Items[1].(*block.VolumeStatus).TypedSpec().Phase = block.VolumePhaseReady
		}
		result = r.statuses
	} else {
		if kind.Type() != block.DiscoveredVolumeType {
			r.t.Fatal("unexpected physical discovery query")
		}
		result = r.discovered
	}
	if r.calls == r.failAt {
		return result, errors.New("partial-native-list-private-detail")
	}
	return result, nil
}

type temporaryFileReaderFixture struct{ calls int }

func (*temporaryFileReaderFixture) BootID(context.Context) (string, error) { return "unused", nil }
func (r *temporaryFileReaderFixture) ListReplicas(context.Context) (longhorninventory.Listing, error) {
	r.calls++
	return longhorninventory.Listing{Complete: true}, nil
}

func (r *temporaryFileReaderFixture) ListStorageRoot(context.Context) (longhorninventory.Listing, error) {
	r.calls++
	return longhorninventory.Listing{Complete: true}, nil
}

func TestTemporaryParentReadRequiresTheOriginalPhysicalPartition(t *testing.T) {
	for _, scenario := range []string{"complete", "partial status", "partial discovery", "raw allocated", "original replaced"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, uuid := temporaryPartitionFixture()
			blocks := &temporaryBlockReaderFixture{t: t, statuses: s, discovered: d}
			files := &temporaryFileReaderFixture{}
			switch scenario {
			case "partial status":
				blocks.failAt = 1
			case "partial discovery":
				blocks.failAt = 2
			case "raw allocated":
				s.Items[1].(*block.VolumeStatus).TypedSpec().Phase = block.VolumePhaseReady
			case "original replaced":
				uuid = "another-partition"
			}
			reader := unusedTemporaryReader{nativeReplicaReader: files, blocks: blocks, address: "10.0.99.4", ephemeralUUID: uuid}
			listing, err := reader.ListStorageRoot(context.Background())
			if scenario == "complete" {
				if err != nil || !listing.Complete || files.calls != 1 || blocks.calls != 2 {
					t.Fatal("complete physical proof did not reach the parent read:", err)
				}
			} else if err == nil || listing.Complete || files.calls != 0 {
				t.Fatal("unproven physical partition reached the parent read")
			}
		})
	}
}

func TestTemporaryNativeProofStopsBeforeFilesOnPartialOrChangedStorage(t *testing.T) {
	for _, scenario := range []string{"complete", "partial status", "partial discovery", "raw allocated between observations", "canceled", "missing original binding"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, uuid := temporaryPartitionFixture()
			blocks := &temporaryBlockReaderFixture{t: t, statuses: s, discovered: d}
			files := &temporaryFileReaderFixture{}
			reader := unusedTemporaryReader{nativeReplicaReader: files, blocks: blocks, address: "10.0.99.4", ephemeralUUID: uuid}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "partial status":
				blocks.failAt = 1
			case "partial discovery":
				blocks.failAt = 2
			case "raw allocated between observations":
				blocks.allocateAfterFirst = true
			case "canceled":
				cancel()
			case "missing original binding":
				reader.ephemeralUUID = ""
			}
			first, err := reader.ListReplicas(ctx)
			if scenario == "complete" || scenario == "raw allocated between observations" {
				if err != nil || !first.Complete || files.calls != 1 {
					t.Fatal("complete physical proof did not reach file inspection:", err)
				}
				second, secondErr := reader.ListReplicas(ctx)
				if scenario == "complete" {
					if secondErr != nil || !second.Complete || files.calls != 2 {
						t.Fatal("second independent physical proof did not complete")
					}
				} else if secondErr == nil || second.Complete || files.calls != 1 {
					t.Fatal("a raw partition appeared without invalidating the second observation")
				}
				return
			}
			if err == nil || first.Complete || files.calls != 0 || strings.Contains(err.Error(), "private-detail") {
				t.Fatal("incomplete physical proof reached file inspection or leaked details")
			}
			if (scenario == "canceled" || scenario == "missing original binding") && blocks.calls != 0 {
				t.Fatal("an unauthorized or expired observation started an API request")
			}
		})
	}
}
