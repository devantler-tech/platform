package main

import (
	"context"
	"errors"
	"maps"
)

var errCopyPodTopology = errors.New("the copy Pod's binding topology does not match the selected node")

func (c client) expectedCopyPodTopology(ctx context.Context, pod, desired object, hostname string) (object, error) {
	assigned := text(pod, "spec", "nodeName")
	if assigned == "" {
		return desired, nil
	}
	if assigned != hostname {
		return nil, errCopyPodTopology
	}
	node, err := c.get(ctx, "node", "", hostname, false)
	if err != nil {
		return nil, err
	}
	return copyPodDesiredTopology(pod, desired, node, hostname)
}

// Kubernetes adds these two labels during binding. Confirm their values
// against the assigned node and keep the reviewed shape checks for all labels.
func copyPodDesiredTopology(pod, desired, node object, hostname string) (object, error) {
	assigned := text(pod, "spec", "nodeName")
	if assigned == "" {
		return desired, nil
	}
	if assigned != hostname || !recoveredNativeObject(node, "v1", "Node", "") ||
		text(node, "metadata", "name") != hostname || text(node, "metadata", "labels", "kubernetes.io/hostname") != hostname {
		return nil, errCopyPodTopology
	}
	labels := maps.Clone(valueObject(desired, "metadata", "labels"))
	if labels == nil {
		return nil, errCopyPodTopology
	}
	for _, key := range [...]string{"topology.kubernetes.io/region", "topology.kubernetes.io/zone"} {
		value, present := valueObject(pod, "metadata", "labels")[key]
		if !present {
			continue
		}
		actual, ok := value.(string)
		if !ok || actual == "" || actual != text(node, "metadata", "labels", key) {
			return nil, errCopyPodTopology
		}
		labels[key] = actual
	}
	expected := maps.Clone(desired)
	metadata := maps.Clone(valueObject(desired, "metadata"))
	metadata["labels"] = labels
	expected["metadata"] = metadata
	return expected, nil
}

func (c client) matchBoundKeeperVolumePod(ctx context.Context, pod, created, desired object, hostname string) bool {
	expected, err := c.expectedCopyPodTopology(ctx, pod, desired, hostname)
	return err == nil && matchCorootKeeperVolumePod(pod, created, expected)
}
