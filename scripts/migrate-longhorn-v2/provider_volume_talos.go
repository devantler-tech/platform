package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/devantler-tech/platform/internal/strictjson"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

const providerDeviceLink = "/dev/disk/by-id/scsi-0HC_Volume_"

var providerDiskPath = regexp.MustCompile(`^/dev/[a-z][a-z0-9-]+$`)

type providerTalosMountAPI interface {
	Mounts(context.Context, *emptypb.Empty, ...grpc.CallOption) (*machine.MountsResponse, error)
}

func readProviderDevices(ctx context.Context, c client, address string) (map[string]string, error) {
	ctx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	raw, err := c.run(ctx, nil, "talosctl", "--nodes="+address, "get", "disks", "--output=json")
	if err != nil {
		return nil, errors.New("native Talos disk inventory is unavailable")
	}
	return parseProviderDevices(raw)
}

func parseProviderDevices(raw []byte) (map[string]string, error) {
	unverified := errors.New("native Talos disk identity is incomplete or ambiguous")
	if len(raw) == 0 || len(raw) > 4<<20 {
		return nil, unverified
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	devices, paths, ids := map[string]string{}, map[string]bool{}, map[string]bool{}
	for {
		var entry json.RawMessage
		err := decoder.Decode(&entry)
		if err == io.EOF && len(ids) != 0 {
			return devices, nil
		}
		var disk object
		if err != nil || strictjson.Validate(entry, nil) != nil || json.Unmarshal(entry, &disk) != nil {
			return nil, unverified
		}
		id, device := text(disk, "metadata", "id"), text(disk, "spec", "dev_path")
		if text(disk, "metadata", "namespace") != "runtime" || text(disk, "metadata", "type") != "Disks.block.talos.dev" ||
			text(disk, "metadata", "phase") != "running" || id == "" || ids[id] || !providerDiskPath.MatchString(device) || paths[device] {
			return nil, unverified
		}
		ids[id], paths[device] = true, true
		links, _ := value(disk, "spec", "symlinks").([]any)
		found := false
		for _, rawLink := range links {
			link, _ := rawLink.(string)
			handle, provider := strings.CutPrefix(link, providerDeviceLink)
			if !provider {
				continue
			}
			parsed, err := strconv.ParseInt(handle, 10, 64)
			if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != handle || devices[handle] != "" || found {
				return nil, unverified
			}
			devices[handle] = device
			found = true
		}
	}
}

func readProviderMounts(ctx context.Context, api providerTalosMountAPI, address string, mounts []providerPodMount, devices map[string]string) (map[string]uint64, error) {
	ctx, stop := context.WithTimeout(talos.WithNode(ctx, address), 15*time.Second)
	defer stop()
	response, err := api.Mounts(ctx, &emptypb.Empty{}, grpc.MaxCallRecvMsgSize(4<<20))
	messages := response.GetMessages()
	if err != nil || ctx.Err() != nil || len(messages) != 1 || messages[0] == nil ||
		!longhorninventory.ValidNodeMetadata(address, messages[0].GetMetadata()) {
		return nil, errors.New("native Talos mount statistics came from an unverified host")
	}
	return matchProviderMounts(messages[0].GetStats(), mounts, devices)
}

func matchProviderMounts(stats []*machine.MountStat, mounts []providerPodMount, devices map[string]string) (map[string]uint64, error) {
	unverified := errors.New("native Talos filesystem usage does not match the original provider mount")
	byPath := make(map[string]*machine.MountStat, len(stats))
	duplicates := map[string]bool{}
	for _, entry := range stats {
		path := entry.GetMountedOn()
		duplicates[path] = byPath[path] != nil
		byPath[path] = entry
	}
	used := make(map[string]uint64, len(mounts))
	for _, mount := range mounts {
		path := "/var/lib/kubelet/pods/" + mount.PodUID + "/volumes/kubernetes.io~csi/" + mount.Claim.PV + "/mount"
		device := devices[mount.Claim.Handle]
		entry := byPath[path]
		if duplicates[path] || device == "" ||
			(entry.GetFilesystem() != device && entry.GetFilesystem() != providerDeviceLink+mount.Claim.Handle) ||
			entry.GetSize() == 0 || entry.GetSize() > mount.Claim.RequestedBytes || entry.GetAvailable() > entry.GetSize() {
			return nil, unverified
		}
		used[mount.Claim.ClaimUID] = entry.GetSize() - entry.GetAvailable()
	}
	return used, nil
}
