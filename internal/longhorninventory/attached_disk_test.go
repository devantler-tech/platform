package longhorninventory

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func attachedDiskFixture() (resource.List, resource.List, resource.List) {
	disks, discovered := resource.List{}, resource.List{}
	for _, entry := range []struct {
		id, volume, signature string
		size                  uint64
	}{{"sda", "", "gpt", 160 << 30}, {"sdb", "1234", "", 40 << 30}, {"sdc", "5678", "xfs", 10 << 30}} {
		disk := block.NewDisk(block.NamespaceName, entry.id)
		disk.Metadata().SetVersion(resource.VersionUndefined.Next())
		disk.TypedSpec().DevPath, disk.TypedSpec().Serial = "/dev/"+entry.id, "serial-"+entry.id
		disk.TypedSpec().Size, disk.TypedSpec().SectorSize, disk.TypedSpec().IOSize = entry.size, 512, 512
		if entry.volume != "" {
			disk.TypedSpec().Symlinks = []string{"/dev/disk/by-id/scsi-0HC_Volume_" + entry.volume}
		}
		disks.Items = append(disks.Items, disk)
		volume := block.NewDiscoveredVolume(block.NamespaceName, entry.id)
		volume.Metadata().SetVersion(resource.VersionUndefined.Next())
		volume.TypedSpec().DevPath, volume.TypedSpec().DevicePath, volume.TypedSpec().Type = "/dev/"+entry.id, "/sys/devices/pci0000:00/block/"+entry.id, "disk"
		volume.TypedSpec().Size, volume.TypedSpec().SectorSize, volume.TypedSpec().IOSize, volume.TypedSpec().Name = entry.size, 512, 512, entry.signature
		discovered.Items = append(discovered.Items, volume)
	}
	// Existing boot partitions and a formatted source disk are not candidates.
	discovered.Items = append(discovered.Items, attachedChild("sda1", "sda", "/dev/sda", "/sys/devices/pci0000:00/block/sda/sda1"))
	system := block.NewSystemDisk(block.NamespaceName, block.SystemDiskID)
	system.Metadata().SetVersion(resource.VersionUndefined.Next())
	system.TypedSpec().DiskID, system.TypedSpec().DevPath = "sda", "/dev/sda"
	return disks, resource.List{Items: []resource.Resource{system}}, discovered
}

func attachedChild(id, parent, parentPath, physicalPath string) *block.DiscoveredVolume {
	child := block.NewDiscoveredVolume(block.NamespaceName, id)
	child.Metadata().SetVersion(resource.VersionUndefined.Next())
	*child.TypedSpec() = block.DiscoveredVolumeSpec{DevPath: "/dev/" + id, DevicePath: physicalPath, Type: "partition",
		Parent: parent, ParentDevPath: parentPath, Size: 4 << 30, SectorSize: 512, IOSize: 512, Name: "luks2", PartitionIndex: 1}
	return child
}

func TestAttachedDiskCandidateBindsProviderAndPhysicalGeometry(t *testing.T) {
	disks, systems, discovered := attachedDiskFixture()
	before, err := qualifyAttachedDiskCandidate("1234", []string{"5678"}, 4<<30, disks, systems, discovered)
	if err != nil || before.DiskID != "sdb" || before.DevPath != "/dev/sdb" || before.SystemDiskID != "sda" || before.Size != 42949672960 || before.SectorSize != 512 || before.Serial != "serial-sdb" || before.Digest == "" {
		t.Fatal("provider and physical disk identity were not bound:", before, err)
	}
	disks.Items[1].Metadata().SetVersion(disks.Items[1].Metadata().Version().Next())
	after, err := qualifyAttachedDiskCandidate("1234", []string{"5678"}, 4<<30, disks, systems, discovered)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("changed disk observation was invisible:", after, err)
	}
}

func TestAttachedDiskCandidateRejectsUnsafeOrAmbiguousMetadata(t *testing.T) {
	for _, fault := range []string{"source", "unknown sources", "bad provider id", "bad source id", "duplicate source", "missing system", "wrong system", "system candidate", "duplicate disk", "duplicate link", "competing alias", "repeated alias", "wrong link", "undefined version", "readonly", "cdrom", "secondary", "holder", "serial", "size", "sector", "unaligned", "minimum", "missing discovery", "duplicate discovery", "gpt", "filesystem", "partition", "child", "contradictory child", "sysfs child", "geometry drift", "physical path", "signature uuid", "parent", "offset", "label", "probed size", "io size", "nil disk", "nil system", "nil discovery"} {
		t.Run(fault, func(t *testing.T) {
			disks, systems, discovered := attachedDiskFixture()
			candidate := disks.Items[1].(*block.Disk)
			probe := discovered.Items[1].(*block.DiscoveredVolume)
			id, sources, minimum := "1234", []string{"5678"}, uint64(4<<30)
			switch fault {
			case "source":
				sources = []string{"1234"}
			case "unknown sources":
				sources = nil
			case "bad provider id":
				id = "01234"
			case "bad source id":
				sources = []string{"05678"}
			case "duplicate source":
				sources = []string{"5678", "5678"}
			case "missing system":
				systems.Items = nil
			case "wrong system":
				systems.Items[0].(*block.SystemDisk).TypedSpec().DevPath = "/dev/sdz"
			case "system candidate":
				systems.Items[0].(*block.SystemDisk).TypedSpec().DiskID, systems.Items[0].(*block.SystemDisk).TypedSpec().DevPath = "sdb", "/dev/sdb"
			case "duplicate disk":
				disks.Items = append(disks.Items, candidate)
			case "duplicate link":
				disks.Items[2].(*block.Disk).TypedSpec().Symlinks = candidate.TypedSpec().Symlinks
			case "competing alias":
				candidate.TypedSpec().Symlinks = append(candidate.TypedSpec().Symlinks, "/dev/disk/by-id/scsi-0HC_Volume_9999")
			case "repeated alias":
				candidate.TypedSpec().Symlinks = append(candidate.TypedSpec().Symlinks, candidate.TypedSpec().Symlinks[0])
			case "wrong link":
				candidate.TypedSpec().Symlinks = []string{"/dev/disk/by-id/scsi-0HC_Volume_9999"}
			case "undefined version":
				candidate.Metadata().SetVersion(resource.VersionUndefined)
			case "readonly":
				candidate.TypedSpec().Readonly = true
			case "cdrom":
				candidate.TypedSpec().CDROM = true
			case "secondary":
				candidate.TypedSpec().SecondaryDisks = []string{"sda"}
			case "holder":
				disks.Items[2].(*block.Disk).TypedSpec().SecondaryDisks = []string{"sdb"}
			case "serial":
				candidate.TypedSpec().Serial = ""
			case "size":
				candidate.TypedSpec().Size = 0
			case "sector":
				candidate.TypedSpec().SectorSize = 1024
			case "unaligned":
				candidate.TypedSpec().Size++
			case "minimum":
				minimum = (40 << 30) + 1
			case "missing discovery":
				discovered.Items = append(discovered.Items[:1], discovered.Items[2:]...)
			case "duplicate discovery":
				discovered.Items = append(discovered.Items, probe)
			case "gpt":
				probe.TypedSpec().Name = "gpt"
			case "filesystem":
				probe.TypedSpec().Name = "xfs"
			case "partition":
				probe.TypedSpec().PartitionIndex = 1
			case "child":
				discovered.Items = append(discovered.Items, attachedChild("sdb1", "sdb", "/dev/sdb", probe.TypedSpec().DevicePath+"/sdb1"))
			case "contradictory child":
				discovered.Items = append(discovered.Items, attachedChild("sdc1", "sdc", "/dev/sdb", "/sys/devices/pci0000:00/block/sdc/sdc1"))
			case "sysfs child":
				discovered.Items = append(discovered.Items, attachedChild("sdc1", "sdc", "/dev/sdc", probe.TypedSpec().DevicePath+"/sdc1"))
			case "geometry drift":
				probe.TypedSpec().Size--
			case "physical path":
				probe.TypedSpec().DevicePath = "/sys/devices/other/block/sdz"
			case "signature uuid":
				probe.TypedSpec().UUID = "old-filesystem"
			case "parent":
				probe.TypedSpec().Parent = "sda"
			case "offset":
				probe.TypedSpec().Offset = 512
			case "label":
				probe.TypedSpec().Label = "old-data"
			case "probed size":
				probe.TypedSpec().ProbedSize = 4096
			case "io size":
				candidate.TypedSpec().IOSize, probe.TypedSpec().IOSize = 1536, 1536
			case "nil disk":
				disks.Items[1] = (*block.Disk)(nil)
			case "nil system":
				systems.Items[0] = (*block.SystemDisk)(nil)
			case "nil discovery":
				discovered.Items[1] = (*block.DiscoveredVolume)(nil)
			}
			if got, err := qualifyAttachedDiskCandidate(id, sources, minimum, disks, systems, discovered); err == nil || got != (attachedDiskCandidate{}) {
				t.Fatal("unsafe disk evidence was accepted:", fault, got, err)
			}
		})
	}
}

func TestAttachedDiskCandidatePreservesNativeIOAndIndependentVersions(t *testing.T) {
	disks, systems, discovered := attachedDiskFixture()
	disk, probe := disks.Items[1].(*block.Disk), discovered.Items[1].(*block.DiscoveredVolume)
	disk.TypedSpec().IOSize, probe.TypedSpec().IOSize = 4096, 4096
	disk.TypedSpec().Serial, disk.TypedSpec().WWID, disk.TypedSpec().UUID = "", "hardware-wwid", "hardware-uuid"
	systems.Items[0].Metadata().SetVersion(resource.VersionUndefined.Next().Next())
	probe.Metadata().SetVersion(resource.VersionUndefined.Next().Next().Next())
	before, err := qualifyAttachedDiskCandidate("1234", []string{"5678", "9999"}, 4<<30, disks, systems, discovered)
	if err != nil || before.WWID != "hardware-wwid" {
		t.Fatal("valid native identity or optimal IO rejected:", before, err)
	}
	slices.Reverse(disks.Items)
	slices.Reverse(discovered.Items)
	after, err := qualifyAttachedDiskCandidate("1234", []string{"9999", "5678"}, 4<<30, disks, systems, discovered)
	if err != nil || before != after {
		t.Fatal("inventory order changed disk identity:", after, err)
	}
}

func TestAttachedDiskCandidateRejectsPhysicalIdentityAliases(t *testing.T) {
	for _, mode := range []string{"sysfs", "serial", "wwid"} {
		t.Run(mode, func(t *testing.T) {
			disks, systems, discovered := attachedDiskFixture()
			candidate, other := disks.Items[1].(*block.Disk), disks.Items[2].(*block.Disk)
			switch mode {
			case "sysfs":
				discovered.Items[2].(*block.DiscoveredVolume).TypedSpec().DevicePath = discovered.Items[1].(*block.DiscoveredVolume).TypedSpec().DevicePath
			case "serial":
				other.TypedSpec().Serial = candidate.TypedSpec().Serial
			case "wwid":
				candidate.TypedSpec().WWID, other.TypedSpec().WWID = "duplicate-hardware-identity", "duplicate-hardware-identity"
			}
			if got, err := qualifyAttachedDiskCandidate("1234", []string{"5678"}, 4<<30, disks, systems, discovered); err == nil || got != (attachedDiskCandidate{}) {
				t.Fatal("aliased physical disk was accepted:", mode, got, err)
			}
		})
	}
}
