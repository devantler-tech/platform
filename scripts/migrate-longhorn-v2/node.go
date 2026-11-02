package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
)

const fenceAnnotation = "storage.platform.devantler.tech/longhorn-v2-preparation"

func verifyNode(n object, j journal, original bool) (string, error) {
	if text(n, "metadata", "name") != j.Target || text(n, "spec", "providerID") != j.ProviderID ||
		text(n, "status", "nodeInfo", "systemUUID") != j.MachineUUID || original && text(n, "metadata", "uid") != j.NodeUID ||
		value(n, "metadata", "deletionTimestamp") != nil {
		return "", errors.New("worker hardware or Kubernetes identity changed")
	}
	addresses, _ := value(n, "status", "addresses").([]any)
	address := ""
	for _, raw := range addresses {
		a, ok := raw.(map[string]any)
		if !ok || text(a, "type") != "InternalIP" {
			continue
		}
		ip, err := netip.ParseAddr(text(a, "address"))
		if err != nil || !netip.MustParsePrefix("10.0.0.0/16").Contains(ip) || address != "" {
			return "", errors.New("worker does not have exactly one platform private address")
		}
		address = ip.String()
	}
	if address == "" {
		return "", errors.New("worker private address is missing")
	}
	return address, nil
}

func blockDisks() object {
	return object{"longhorn-v2": object{"path": "/dev/mapper/luks2-r-longhorn", "diskType": "block", "diskDriver": "aio", "allowScheduling": true, "evictionRequested": false, "storageReserved": float64(4 * 1024 * 1024 * 1024), "tags": []any{}}}
}

func verifyDiskDefaults(n object) error {
	var disks []object
	if text(n, "metadata", "labels", "node.longhorn.io/create-default-disk") != "config" ||
		json.Unmarshal([]byte(text(n, "metadata", "annotations", "node.longhorn.io/default-disks-config")), &disks) != nil || len(disks) != 1 {
		return errors.New("worker has not adopted the reviewed raw-disk defaults")
	}
	d := disks[0]
	if len(d) != 6 || text(d, "name") != "longhorn-v2" || text(d, "path") != "/dev/mapper/luks2-r-longhorn" ||
		text(d, "diskType") != "block" || text(d, "diskDriver") != "aio" || value(d, "allowScheduling") != true || value(d, "storageReserved") != float64(4*1024*1024*1024) {
		return errors.New("worker default disk differs from the reviewed raw-disk contract")
	}
	return nil
}

func reviewedBlockDisks(raw any) bool {
	disks, ok := raw.(map[string]any)
	if !ok || len(disks) != 1 {
		return false
	}
	d, ok := disks["longhorn-v2"].(map[string]any)
	expected := blockDisks()["longhorn-v2"].(object)
	if !ok || len(d) != len(expected) {
		return false
	}
	for key, want := range expected {
		if key == "tags" {
			if d[key] == nil {
				continue
			}
			tags, ok := d[key].([]any)
			if !ok || len(tags) != 0 {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(d[key], want) {
			return false
		}
	}
	return true
}
