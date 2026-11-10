package longhorninventory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	resource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type blockLayoutFixture struct {
	state.CoreStateReader
	t        *testing.T
	calls    int
	mode     string
	rawPhase *block.VolumePhase
	retained bool
	closed   bool
	shifted  bool
}

func (f *blockLayoutFixture) List(ctx context.Context, kind resource.Kind, options ...state.ListOption) (resource.List, error) {
	f.t.Helper()
	f.calls++
	deadline, bounded := ctx.Deadline()
	md, _ := metadata.FromOutgoingContext(ctx)
	if !bounded || time.Until(deadline) > 15*time.Second || len(options) != 0 || kind.Namespace() != block.NamespaceName || len(md.Get("node")) != 1 || md.Get("node")[0] != "10.0.0.8" || len(md.Get("nodes")) != 0 {
		f.t.Fatal("block observation escaped its bounded fixed-node scope")
	}
	var result resource.List
	if f.calls%2 == 1 {
		if kind.Type() != block.VolumeStatusType {
			f.t.Fatal("unexpected status type")
		}
		eph := block.NewVolumeStatus(block.NamespaceName, "EPHEMERAL")
		*eph.TypedSpec() = block.VolumeStatusSpec{Phase: block.VolumePhaseReady, Type: block.VolumeTypePartition, UUID: "private-ephemeral-canary", Location: "/dev/sda6", ParentLocation: "/dev/sda", MountLocation: "/dev/dm-0", Size: 150 << 30, EncryptionProvider: block.EncryptionProviderLUKS2, EncryptionLockedToState: true, ConfiguredEncryptionKeys: []string{"nodeID"}}
		raw := block.NewVolumeStatus(block.NamespaceName, "r-longhorn")
		raw.TypedSpec().Phase = block.VolumePhaseWaiting
		if f.rawPhase != nil {
			raw.TypedSpec().Phase = *f.rawPhase
		}
		if f.mode == "changed" && f.calls > 2 {
			raw.TypedSpec().Phase = block.VolumePhaseReady
		}
		if f.retained {
			*raw.TypedSpec() = block.VolumeStatusSpec{
				Phase: block.VolumePhaseReady, Type: block.VolumeTypePartition, Location: "/dev/sda3",
				ParentLocation: "/dev/sda", PartitionUUID: "private-raw-uuid-canary", PartitionIndex: 3,
				Size: 96 << 30, EncryptionProvider: block.EncryptionProviderLUKS2,
				EncryptionLockedToState: true, ConfiguredEncryptionKeys: []string{"nodeID"},
			}
		}
		result.Items = []resource.Resource{eph, raw}
		if f.closed {
			result.Items = []resource.Resource{eph}
		}
		if f.mode == "duplicate" {
			result.Items = append(result.Items, eph)
		}
		if f.mode == "missing ephemeral" {
			result.Items = result.Items[1:]
		}
		if f.mode == "tearing down" {
			eph.Metadata().SetPhase(resource.PhaseTearingDown)
		}
	} else {
		if kind.Type() != block.DiscoveredVolumeType {
			f.t.Fatal("unexpected discovery type")
		}
		v := block.NewDiscoveredVolume(block.NamespaceName, "private-device-canary")
		*v.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/sda6", ParentDevPath: "/dev/sda", PartitionLabel: "EPHEMERAL", PartitionUUID: "private-partition-canary", Size: 150 << 30}
		result.Items = []resource.Resource{v}
		if f.retained {
			rawID, rawIndex := "sda3", uint(3)
			if f.shifted {
				rawID, rawIndex = "sda4", 4
			}
			raw := block.NewDiscoveredVolume(block.NamespaceName, rawID)
			state := block.NewDiscoveredVolume(block.NamespaceName, "sda2")
			*state.TypedSpec() = block.DiscoveredVolumeSpec{
				Type: "partition", DevPath: "/dev/sda2", ParentDevPath: "/dev/sda", PartitionLabel: "STATE",
				PartitionUUID: "private-state-uuid-canary", PartitionIndex: 2, Size: 100 << 20, Offset: 500 << 20,
			}
			*raw.TypedSpec() = block.DiscoveredVolumeSpec{
				Type: "partition", DevPath: "/dev/" + rawID, ParentDevPath: "/dev/sda", PartitionLabel: "r-longhorn",
				Name: "luks", UUID: "private-luks-canary",
				PartitionUUID: "private-raw-uuid-canary", PartitionIndex: rawIndex, Size: 96 << 30, Offset: 1 << 30,
			}
			switch f.mode {
			case "offset missing":
				raw.TypedSpec().Offset = 0
			case "offset changed":
				if f.calls > 2 {
					raw.TypedSpec().Offset += 512
				}
			case "wrong identity":
				raw.TypedSpec().PartitionUUID = "other"
			case "wrong parent":
				raw.TypedSpec().ParentDevPath = "/dev/sdb"
			case "wrong size":
				raw.TypedSpec().Size = 144 << 30
			case "LUKS header missing":
				raw.TypedSpec().UUID = ""
			case "LUKS header changed":
				if f.calls > 2 {
					raw.TypedSpec().UUID = "other"
				}
			case "duplicate raw label":
				v.TypedSpec().PartitionLabel = "r-longhorn"
			case "STATE changed":
				if f.calls > 2 {
					state.TypedSpec().Offset += 512
				}
			case "STATE overlaps raw":
				state.TypedSpec().Offset = raw.TypedSpec().Offset
			case "STATE on other disk":
				state.TypedSpec().ParentDevPath = "/dev/sdb"
			}
			result.Items = append(result.Items, raw)
			if f.mode != "STATE missing" {
				result.Items = append(result.Items, state)
			}
		}
	}
	if f.mode == "partial" && f.calls == 2 {
		return result, errors.New("private-provider-canary")
	}
	return result, nil
}

func TestNativeRetainedBlockLayoutRequiresStablePhysicalExtent(t *testing.T) {
	for _, mode := range []string{"stable", "offset missing", "offset changed", "wrong identity", "wrong parent", "wrong size", "LUKS header missing", "LUKS header changed", "duplicate raw label", "STATE changed", "STATE overlaps raw", "STATE on other disk", "STATE missing"} {
		t.Run(mode, func(t *testing.T) {
			f := &blockLayoutFixture{t: t, mode: mode, retained: true}
			report, identity, err := (Native{API: fileAPI{}, Target: "10.0.0.8", Blocks: f}).ListRetainedBlockLayout(context.Background(), false)
			if mode != "stable" {
				if err == nil || report != (BlockLayoutReport{}) || identity != (RetainedLayoutIdentity{}) || strings.Contains(err.Error(), "canary") {
					t.Fatal("unverified physical extent retained success or leaked identity")
				}
				return
			}
			encoded, marshalErr := json.Marshal(report)
			if err != nil || marshalErr != nil || !report.RawExtentObserved || identity.PartitionUUID == "" || len(identity.ExtentDigest) != 64 || f.calls != 4 ||
				strings.Contains(string(encoded), "canary") || strings.Contains(string(encoded), "/dev/") ||
				strings.Contains(string(encoded), "1073741824") || strings.Contains(string(encoded), identity.ExtentDigest) {
				t.Fatalf("physical extent observation was incomplete or exposed private geometry: %v", err)
			}
		})
	}
}

func TestRetainedLayoutDigestSurvivesOnlyAnUnchangedPhysicalExtent(t *testing.T) {
	observe := func(f *blockLayoutFixture, closed bool) (RetainedLayoutIdentity, error) {
		api := fileAPI{list: func(ctx context.Context, req *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
			const root = "/sys/class/block/sda4/holders"
			md, _ := metadata.FromOutgoingContext(ctx)
			if !closed || req.GetRoot() != root || req.GetRecursionDepth() != 1 || len(md.Get("node")) != 1 || md.Get("node")[0] != "10.0.0.8" {
				t.Fatal("retained-volume inspection escaped the selected partition")
			}
			items := []*machine.FileInfo{{Name: root, RelativeName: ".", Mode: uint32(os.ModeDir), IsDir: true}}
			end := io.EOF
			if f.mode == "live holder" {
				items = append(items, &machine.FileInfo{Name: root + "/dm-2", RelativeName: "dm-2", Link: "../../dm-2"})
			}
			if f.mode == "incomplete holders" {
				end = errors.Join(io.EOF, errors.New("private-transport"))
			}
			return &fileStream{items: items, err: end}, nil
		}}
		_, identity, err := (Native{API: api, Target: "10.0.0.8", Blocks: f}).ListRetainedBlockLayout(t.Context(), closed)
		return identity, err
	}
	open, err := observe(&blockLayoutFixture{t: t, retained: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := observe(&blockLayoutFixture{t: t, retained: true, closed: true, shifted: true}, true)
	if err != nil || closed != open {
		t.Fatal("closing the mapping and changing its GPT index lost the retained identity:", err)
	}
	if _, err = observe(&blockLayoutFixture{t: t, retained: true}, true); err == nil {
		t.Fatal("an open mapper qualified as closed")
	}
	for _, mode := range []string{"live holder", "incomplete holders"} {
		if _, err = observe(&blockLayoutFixture{t: t, retained: true, closed: true, shifted: true, mode: mode}, true); err == nil || strings.Contains(err.Error(), "private-transport") {
			t.Fatalf("unproven holders %s qualified or leaked provider data", mode)
		}
	}
}

func TestNativeBlockLayoutReportsObservedStateWithoutPrivateIdentities(t *testing.T) {
	for _, mode := range []string{"stable", "partial", "changed", "duplicate", "missing ephemeral", "tearing down", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := &blockLayoutFixture{t: t, mode: mode}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			report, err := (Native{API: fileAPI{}, Target: "10.0.0.8", Blocks: f}).ListBlockLayout(ctx)
			if (err == nil) != (mode == "stable") {
				t.Fatal("incomplete or changing block inventory qualified:", err)
			}
			if mode == "canceled" && f.calls != 0 {
				t.Fatal("canceled observer reached native state")
			}
			if err != nil {
				if report.Digest != "" || strings.Contains(err.Error(), "canary") {
					t.Fatal("failed observation leaked or retained success")
				}
				return
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "canary") || strings.Contains(string(encoded), "/dev/") || f.calls != 4 || report.EphemeralPhase != "ready" || !report.EphemeralLUKS2 || !report.EphemeralNodeIDOnly || report.EphemeralSizeBytes != 150<<30 || !report.RawPresent || report.RawPhase != "waiting" || report.RawSizeBytes != 0 || report.RawDiscoveredPartitions != 0 || len(report.Digest) != 64 {
				t.Fatal("observed block state lost scope, leaked identity or invented allocation:", report)
			}
		})
	}
}

func TestNativeBlockLayoutDistinguishesKnownAllocationPhases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase block.VolumePhase
		want  string
	}{
		{"waiting", block.VolumePhaseWaiting, "waiting"},
		{"failed", block.VolumePhaseFailed, "failed"},
		{"missing", block.VolumePhaseMissing, "missing"},
		{"located", block.VolumePhaseLocated, "located"},
		{"provisioned", block.VolumePhaseProvisioned, "provisioned"},
		{"prepared", block.VolumePhasePrepared, "prepared"},
		{"ready", block.VolumePhaseReady, "ready"},
		{"closed", block.VolumePhaseClosed, "closed"},
		{"unknown negative", block.VolumePhase(-1), "other"},
		{"unknown future", block.VolumePhase(99), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &blockLayoutFixture{t: t, mode: "stable", rawPhase: &tc.phase}
			report, err := (Native{API: fileAPI{}, Target: "10.0.0.8", Blocks: f}).ListBlockLayout(context.Background())
			if err != nil || report.RawPhase != tc.want || f.calls != 4 || report.RawSizeBytes != 0 || report.RawDiscoveredPartitions != 0 {
				t.Fatalf("known state was hidden or allocation was invented: phase=%s error=%v", report.RawPhase, err)
			}
		})
	}
}
