package longhorninventory

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	common "github.com/siderolabs/talos/pkg/machinery/api/common"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type attachedNativeFixture struct {
	t                                  *testing.T
	mode                               string
	blockCalls, bootCalls, holderCalls int
	cancel                             context.CancelFunc
}

func (f *attachedNativeFixture) check(ctx context.Context) {
	f.t.Helper()
	deadline, bounded := ctx.Deadline()
	md, _ := metadata.FromOutgoingContext(ctx)
	if !bounded || time.Until(deadline) > 15*time.Second || ctx.Err() != nil || len(md.Get("node")) != 1 || md.Get("node")[0] != "10.0.0.8" || len(md.Get("nodes")) != 0 {
		f.t.Fatal("attached observation escaped fixed node or bounded deadline")
	}
}

func (f *attachedNativeFixture) List(ctx context.Context, kind resource.Kind, options ...state.ListOption) (resource.List, error) {
	f.check(ctx)
	f.blockCalls++
	disks, systems, discovered := attachedDiskFixture()
	if len(options) != 0 || kind.Namespace() != block.NamespaceName {
		f.t.Fatal("unexpected block collection scope")
	}
	var result resource.List
	switch (f.blockCalls - 1) % 3 {
	case 0:
		if kind.Type() != block.DiskType {
			f.t.Fatal("unexpected disk kind")
		}
		result = disks
		if f.mode == "revision changed" && f.blockCalls == 4 {
			disks.Items[1].Metadata().SetVersion(resource.VersionUndefined.Next().Next())
		}
	case 1:
		if kind.Type() != block.SystemDiskType {
			f.t.Fatal("unexpected system kind")
		}
		result = systems
	case 2:
		if kind.Type() != block.DiscoveredVolumeType {
			f.t.Fatal("unexpected discovery kind")
		}
		result = discovered
		if f.mode == "new signature" && f.blockCalls == 6 {
			discovered.Items[1].(*block.DiscoveredVolume).TypedSpec().Name = "xfs"
		}
	}
	if (f.mode == "partial first" && f.blockCalls == 2) || (f.mode == "partial second" && f.blockCalls == 6) {
		return result, errors.New("partial block stream")
	}
	if f.mode == "cancelled" && f.blockCalls == 6 {
		f.cancel()
	}
	return result, nil
}

func (f *attachedNativeFixture) files() fileAPI {
	return fileAPI{
		read: func(ctx context.Context, req *machine.ReadRequest) (grpc.ServerStreamingClient[common.Data], error) {
			f.check(ctx)
			f.bootCalls++
			if req.GetPath() != "/proc/sys/kernel/random/boot_id" {
				f.t.Fatal("unexpected boot file")
			}
			boot := "12345678-1234-4234-8234-123456789abc\n"
			if f.mode == "reboot" && f.bootCalls == 2 {
				boot = "22345678-1234-4234-8234-123456789abc\n"
			}
			end := io.EOF
			if f.mode == "boot partial" && f.bootCalls == 2 {
				end = errors.Join(io.EOF, errors.New("partial"))
			}
			return &dataStream{items: []*common.Data{{Bytes: []byte(boot)}}, err: end}, nil
		},
		list: func(ctx context.Context, req *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
			f.check(ctx)
			f.holderCalls++
			const root = "/sys/class/block/sdb/holders"
			if req.GetRoot() != root || req.GetRecursionDepth() != 1 || req.GetRecurse() || len(req.GetTypes()) != 0 || req.GetReportXattrs() {
				f.t.Fatal("unexpected holder scope")
			}
			items := []*machine.FileInfo{{Name: root, RelativeName: ".", Mode: uint32(os.ModeDir | 0700), IsDir: true}}
			if f.mode == "new holder" && f.holderCalls == 2 {
				items = append(items, &machine.FileInfo{Name: root + "/dm-1", RelativeName: "dm-1", Link: "/sys/class/block/dm-1"})
			}
			end := io.EOF
			if f.mode == "holder partial" {
				end = errors.Join(io.EOF, errors.New("partial"))
			}
			return &fileStream{items: items, err: end}, nil
		},
	}
}

func TestAttachedNativeObservationBracketsMetadataAndHoldersAcrossOneBoot(t *testing.T) {
	f := &attachedNativeFixture{t: t}
	n := Native{Target: "10.0.0.8", API: f.files(), Blocks: f}
	report, identity, err := n.ListAttachedDisk(context.Background(), "1234", []string{"5678"}, 4<<30)
	if err != nil || report.SizeBytes != 40<<30 || report.SectorSize != 512 || report.Digest == "" || identity.DiskID != "sdb" || identity.DevPath != "/dev/sdb" || identity.BootID != "12345678-1234-4234-8234-123456789abc" || f.blockCalls != 6 || f.bootCalls != 2 || f.holderCalls != 2 {
		t.Fatal("stable native observation was not fully bound:", report, identity, err, f.blockCalls, f.bootCalls, f.holderCalls)
	}
}

func TestAttachedNativeObservationReturnsNoPrefixAfterFailure(t *testing.T) {
	for _, mode := range []string{"partial first", "partial second", "revision changed", "new signature", "reboot", "boot partial", "new holder", "holder partial", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &attachedNativeFixture{t: t, mode: mode, cancel: cancel}
			n := Native{Target: "10.0.0.8", API: f.files(), Blocks: f}
			report, identity, err := n.ListAttachedDisk(ctx, "1234", []string{"5678"}, 4<<30)
			if err == nil || report != (AttachedDiskReport{}) || identity != (AttachedDiskIdentity{}) {
				t.Fatal("incomplete observation returned evidence:", report, identity, err)
			}
		})
	}
}
