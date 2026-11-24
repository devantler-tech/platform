package longhorninventory

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cosi-project/runtime/api/v1alpha1"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func nativeDiskWire(t *testing.T, item resource.Resource) *v1alpha1.ListResponse {
	t.Helper()
	encoded, err := protobuf.FromResource(item)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encoded.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return &v1alpha1.ListResponse{Resource: wire}
}

func TestBoundedDiskWireReadsTheNativeMetadataTypes(t *testing.T) {
	for _, kind := range []string{"disk", "system"} {
		t.Run(kind, func(t *testing.T) {
			var item resource.Resource
			if kind == "disk" {
				disk := block.NewDisk(block.NamespaceName, "sdb")
				disk.TypedSpec().DevPath, disk.TypedSpec().Serial = "/dev/sdb", "HC_Volume_1234"
				disk.TypedSpec().Size, disk.TypedSpec().SectorSize = 40<<30, 512
				item = disk
			} else {
				system := block.NewSystemDisk(block.NamespaceName, block.SystemDiskID)
				system.TypedSpec().DiskID, system.TypedSpec().DevPath = "sda", "/dev/sda"
				item = system
			}
			ctx, cancel := context.WithTimeout(talos.WithNode(context.Background(), "10.0.0.8"), 15*time.Second)
			defer cancel()
			stream := &blockWireStream{response: nativeDiskWire(t, item), end: io.EOF}
			got, err := (BoundedBlocks{API: blockListAPIFixture{stream: stream}, Target: "10.0.0.8"}).List(ctx, item.Metadata())
			if err != nil || len(got.Items) != 1 {
				t.Fatal("the native disk metadata was not decoded:", err)
			}
			if kind == "disk" {
				disk, ok := got.Items[0].(*block.Disk)
				if !ok || disk.TypedSpec().DevPath != "/dev/sdb" || disk.TypedSpec().Serial != "HC_Volume_1234" || disk.TypedSpec().Size != 42949672960 || disk.TypedSpec().SectorSize != 512 {
					t.Fatal("native disk identity or geometry changed")
				}
			} else {
				system, ok := got.Items[0].(*block.SystemDisk)
				if !ok || system.TypedSpec().DiskID != "sda" || system.TypedSpec().DevPath != "/dev/sda" {
					t.Fatal("native system disk identity changed")
				}
			}
		})
	}
}

func TestBoundedDiskWireKeepsTargetAndCompletionGuards(t *testing.T) {
	for _, kind := range []string{"disk", "system"} {
		for _, fault := range []string{"node", "deadline", "joined eof", "resource type", "namespace", "entry bound"} {
			if kind == "system" && fault == "entry bound" {
				continue // The repeated fixture changes the singleton's canonical ID.
			}
			t.Run(kind+"/"+fault, func(t *testing.T) {
				var item resource.Resource = block.NewDisk(block.NamespaceName, "sdb")
				if kind == "system" {
					item = block.NewSystemDisk(block.NamespaceName, block.SystemDiskID)
				}
				request := item.Metadata()
				response := nativeDiskWire(t, item)
				stream := &blockWireStream{response: response, end: io.EOF}
				address, timeout := "10.0.0.8", 15*time.Second
				switch fault {
				case "node":
					address = "10.0.0.9"
				case "deadline":
					timeout = 30 * time.Second
				case "joined eof":
					stream.end = errors.Join(io.EOF, errors.New("unverified"))
				case "resource type":
					stream.response = nativeDiskWire(t, block.NewVolumeStatus(block.NamespaceName, "EPHEMERAL"))
				case "namespace":
					response.Resource.Metadata.Namespace = "unreviewed"
				case "entry bound":
					stream.repeat = true
				}
				ctx, cancel := context.WithTimeout(talos.WithNode(context.Background(), address), timeout)
				defer cancel()
				got, err := (BoundedBlocks{API: blockListAPIFixture{stream: stream}, Target: "10.0.0.8"}).List(ctx, request)
				if err == nil || len(got.Items) != 0 {
					t.Fatal("unqualified metadata escaped the bounded reader:", err)
				}
				if fault == "entry bound" && stream.calls != 129 {
					t.Fatal("the disk entry limit was not exercised while streaming")
				}
			})
		}
	}
}

func TestBoundedSystemDiskWireRequiresItsSingleCanonicalIdentity(t *testing.T) {
	item := block.NewSystemDisk(block.NamespaceName, "unexpected-system-disk")
	ctx, cancel := context.WithTimeout(talos.WithNode(context.Background(), "10.0.0.8"), 15*time.Second)
	defer cancel()
	stream := &blockWireStream{response: nativeDiskWire(t, item), end: io.EOF}
	got, err := (BoundedBlocks{API: blockListAPIFixture{stream: stream}, Target: "10.0.0.8"}).List(ctx, item.Metadata())
	if err == nil || len(got.Items) != 0 {
		t.Fatal("a noncanonical system disk identity was accepted:", err)
	}
}
