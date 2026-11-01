package main

import (
	"errors"
	"net/netip"
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
