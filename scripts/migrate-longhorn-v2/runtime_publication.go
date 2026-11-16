package main

import (
	"encoding/hex"
	"errors"
	"strings"
)

const runtimeStockImage = "docker.io/longhornio/longhorn-instance-manager:v1.13.0"

var errRuntimePublication = errors.New("longhorn runtime publication lacks its required capacity evidence")

// The native observer establishes the epoch. A first selection needs complete
// stock-image overlap capacity; subsequent unchanged selections do not create
// another stock-to-reviewed transition. Neither outcome proves adoption.
func runtimePublicationVerdict(image string, receipt runtimeInventoryReceipt) (string, error) {
	if image == runtimeStockImage {
		return "NOT_REQUESTED", nil
	}
	if image != runtimeRetirementImage || receipt.Nodes <= 0 || receipt.KubernetesNodes < receipt.Nodes ||
		receipt.Pods <= 0 || receipt.InstanceManagers <= 0 || receipt.V1Volumes < 0 || receipt.V2Volumes < 0 ||
		len(receipt.EpochSHA) != 64 || receipt.EpochSHA != strings.ToLower(receipt.EpochSHA) {
		return "", errRuntimePublication
	}
	if _, err := hex.DecodeString(receipt.EpochSHA); err != nil {
		return "", errRuntimePublication
	}
	if receipt.Default == "reviewed" {
		return "ALREADY_SELECTED", nil
	}
	if receipt.Default != "stock" || receipt.DeclaredOverlap != "FIT" || receipt.OverlapNodes != receipt.Nodes {
		return "", errRuntimePublication
	}
	return "FIT", nil
}
