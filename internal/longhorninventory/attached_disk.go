package longhorninventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// attachedDiskCandidate is identification evidence, not permission to prepare a
// disk. Its caller must independently prove provider ownership, complete source
// exclusions, stable native observations, empty holders and recovery keys.
type attachedDiskCandidate struct {
	DiskID, DevPath, SystemDiskID, Serial, WWID, Digest string
	Size                                                uint64
	SectorSize                                          uint
}

func qualifyAttachedDiskCandidate(providerID string, sources []string, minimum uint64, disks, systems, discovered resource.List) (attachedDiskCandidate, error) {
	if !attachedProviderID(providerID) || sources == nil || len(sources) > 512 || minimum == 0 ||
		len(disks.Items) == 0 || len(disks.Items) > 128 || len(systems.Items) != 1 || len(discovered.Items) == 0 || len(discovered.Items) > 512 {
		return attachedDiskCandidate{}, errNative
	}
	exclusions := slices.Clone(sources)
	slices.Sort(exclusions)
	for i, id := range exclusions {
		if !attachedProviderID(id) || id == providerID || (i > 0 && exclusions[i-1] == id) {
			return attachedDiskCandidate{}, errNative
		}
	}
	rows := [][]byte{}
	metadataBytes := 0
	appendRow := func(kind string, m *resource.Metadata, spec any) bool {
		if m == nil || m.Namespace() != block.NamespaceName || m.Type() != kind || m.Phase() != resource.PhaseRunning ||
			m.ID() == "" || len(m.ID()) > 256 || m.Version().Equal(resource.VersionUndefined) || m.Version().Value() > math.MaxInt64 {
			return false
		}
		encoded, err := json.Marshal([]any{kind, m.ID(), m.Version().String(), spec})
		metadataBytes += len(encoded)
		if err != nil || metadataBytes > MaxMetadataBytes {
			return false
		}
		rows = append(rows, encoded)
		return true
	}
	byID, byPath := map[string]*block.Disk{}, map[string]bool{}
	const providerPrefix = "/dev/disk/by-id/scsi-0HC_Volume_"
	link := providerPrefix + providerID
	aliases := map[string]bool{}
	var selected *block.Disk
	for _, item := range disks.Items {
		disk, ok := item.(*block.Disk)
		if !ok || disk == nil || disk.TypedSpec() == nil || !appendRow(block.DiskType, disk.Metadata(), disk.TypedSpec()) {
			return attachedDiskCandidate{}, errNative
		}
		id, s := disk.Metadata().ID(), disk.TypedSpec()
		if byID[id] != nil || byPath[s.DevPath] || s.DevPath != "/dev/"+id || path.Base(id) != id || id == "." || id == ".." {
			return attachedDiskCandidate{}, errNative
		}
		byID[id], byPath[s.DevPath] = disk, true
		providerAliases := 0
		seenLinks := map[string]bool{}
		for _, symlink := range s.Symlinks {
			if seenLinks[symlink] {
				return attachedDiskCandidate{}, errNative
			}
			seenLinks[symlink] = true
			if alias, found := strings.CutPrefix(symlink, providerPrefix); found {
				providerAliases++
				if !attachedProviderID(alias) || aliases[alias] || providerAliases > 1 {
					return attachedDiskCandidate{}, errNative
				}
				aliases[alias] = true
			}
			if symlink == link {
				if selected != nil {
					return attachedDiskCandidate{}, errNative
				}
				selected = disk
			}
		}
	}
	system, ok := systems.Items[0].(*block.SystemDisk)
	if !ok || system == nil || system.TypedSpec() == nil || !appendRow(block.SystemDiskType, system.Metadata(), system.TypedSpec()) ||
		system.Metadata().ID() != block.SystemDiskID || selected == nil {
		return attachedDiskCandidate{}, errNative
	}
	sys, id, disk := system.TypedSpec(), selected.Metadata().ID(), selected.TypedSpec()
	root := byID[sys.DiskID]
	if root == nil || root.TypedSpec().DevPath != sys.DevPath || id == sys.DiskID || disk.DevPath == sys.DevPath ||
		!kernelPartitionName.MatchString(id) || disk.Readonly || disk.CDROM || len(disk.SecondaryDisks) != 0 ||
		(disk.Serial == "" && disk.WWID == "") || !attachedOpaqueID(disk.Serial) || !attachedOpaqueID(disk.WWID) ||
		disk.Size < minimum || (disk.SectorSize != 512 && disk.SectorSize != 4096) || disk.Size%uint64(disk.SectorSize) != 0 ||
		disk.IOSize < disk.SectorSize || disk.IOSize&(disk.IOSize-1) != 0 {
		return attachedDiskCandidate{}, errNative
	}
	for _, other := range byID {
		if other.Metadata().ID() != id && ((disk.WWID != "" && other.TypedSpec().WWID == disk.WWID) ||
			(disk.WWID == "" && other.TypedSpec().Serial == disk.Serial)) {
			return attachedDiskCandidate{}, errNative
		}
		if slices.Contains(other.TypedSpec().SecondaryDisks, id) || slices.Contains(other.TypedSpec().SecondaryDisks, disk.DevPath) {
			return attachedDiskCandidate{}, errNative
		}
	}
	seen, paths := map[string]bool{}, map[string]bool{}
	var whole *block.DiscoveredVolumeSpec
	for _, item := range discovered.Items {
		volume, valid := item.(*block.DiscoveredVolume)
		if !valid || volume == nil || volume.TypedSpec() == nil || !appendRow(block.DiscoveredVolumeType, volume.Metadata(), volume.TypedSpec()) {
			return attachedDiskCandidate{}, errNative
		}
		vid, s := volume.Metadata().ID(), volume.TypedSpec()
		if seen[vid] || paths[s.DevPath] || s.DevPath != "/dev/"+vid || path.Base(vid) != vid || vid == "." || vid == ".." || s.Size == 0 {
			return attachedDiskCandidate{}, errNative
		}
		seen[vid], paths[s.DevPath] = true, true
		if vid == id {
			whole = s
		} else if s.Parent == id || s.ParentDevPath == disk.DevPath {
			return attachedDiskCandidate{}, errNative
		}
	}
	if whole == nil || whole.Type != "disk" || !strings.HasPrefix(whole.DevicePath, "/sys/devices/") ||
		path.Clean(whole.DevicePath) != whole.DevicePath || !strings.HasSuffix(whole.DevicePath, "/block/"+id) ||
		whole.DevPath != disk.DevPath || whole.Size != disk.Size || whole.SectorSize != disk.SectorSize || whole.IOSize != disk.IOSize ||
		whole.Parent != "" || whole.ParentDevPath != "" || whole.Offset != 0 || whole.Name != "" || whole.UUID != "" || whole.Label != "" ||
		whole.BlockSize != 0 || whole.FilesystemBlockSize != 0 || whole.ProbedSize != 0 || whole.PartitionUUID != "" ||
		whole.PartitionType != "" || whole.PartitionLabel != "" || whole.PartitionIndex != 0 {
		return attachedDiskCandidate{}, errNative
	}
	for _, item := range discovered.Items {
		if s := item.(*block.DiscoveredVolume).TypedSpec(); item.Metadata().ID() != id &&
			(s.DevicePath == whole.DevicePath || strings.HasPrefix(s.DevicePath, whole.DevicePath+"/")) {
			return attachedDiskCandidate{}, errNative
		}
	}
	slices.SortFunc(rows, bytes.Compare)
	encoded, err := json.Marshal([]any{providerID, exclusions, minimum, rows})
	if err != nil || len(encoded) > MaxMetadataBytes {
		return attachedDiskCandidate{}, errNative
	}
	digest := sha256.Sum256(encoded)
	return attachedDiskCandidate{DiskID: id, DevPath: disk.DevPath, SystemDiskID: sys.DiskID, Size: disk.Size,
		SectorSize: disk.SectorSize, Serial: disk.Serial, WWID: disk.WWID, Digest: hex.EncodeToString(digest[:])}, nil
}

func attachedProviderID(id string) bool {
	parsed, err := strconv.ParseInt(id, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatInt(parsed, 10) == id
}

func attachedOpaqueID(value string) bool {
	if len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}
