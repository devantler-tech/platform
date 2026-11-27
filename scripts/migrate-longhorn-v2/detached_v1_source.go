package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	k8sresource "k8s.io/apimachinery/pkg/api/resource"
)

const detachedV1SourceOperation = "observe-detached-v1-source"

var errDetachedV1Source = errors.New("the exact retained and detached V1 source is unverified")

type detachedV1Source struct {
	Digest, Phase string
	Bytes         int64
}

func authorizeDetachedV1Source(getenv func(string) string) (string, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	name, valid := strings.CutPrefix(confirmation, detachedV1SourceOperation+":pvc-")
	if !valid || !isolationUID.MatchString(name) || getenv("LONGHORN_V2_OPERATION") != detachedV1SourceOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return "", errDetachedV1Source
	}
	return "pvc-" + name, authorizeWorkflow(getenv, confirmation)
}

func detachedV1SourceReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	sourceRead := program == "git" && runtimeObservationReadAllowed(program, args...)
	clusterRead := program == "kubectl" && len(args) >= 5 && args[0] == "--context=admin@prod" &&
		args[1] == "--request-timeout=20s" && args[2] == "get"
	if input != nil || (!sourceRead && !clusterRead) || !commandContextActive(ctx) {
		return nil, errors.New("detached V1 qualification permits only source and Kubernetes reads")
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

func bindDetachedV1Source(pv, claim, volume, tickets object, pods, attachments []object) (detachedV1Source, error) {
	var zero detachedV1Source
	claimUID := text(pv, "spec", "claimRef", "uid")
	namespace := text(pv, "spec", "claimRef", "namespace")
	claimName := text(pv, "spec", "claimRef", "name")
	name := "pvc-" + claimUID
	phase := text(pv, "status", "phase")
	class := text(pv, "spec", "storageClassName")
	if !isolationUID.MatchString(claimUID) || !kubeNamespace.MatchString(namespace) || !kubeName.MatchString(claimName) ||
		!recoveredNativeObject(pv, "v1", "PersistentVolume", "") || text(pv, "metadata", "name") != name ||
		text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" || !kubeName.MatchString(class) ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != name {
		return zero, errDetachedV1Source
	}
	if !idleDetachedV1Claim(claim, pods, phase, namespace, claimName, claimUID, name, class) ||
		!idleDetachedV1Backend(volume, tickets, attachments, name) {
		return zero, errDetachedV1Source
	}
	size, err := k8sresource.ParseQuantity(text(pv, "spec", "capacity", "storage"))
	pvBytes, pvExact := size.AsInt64()
	volumeBytes, volumeExact := v1VolumeBytes(text(volume, "spec", "size"))
	if err != nil || !pvExact || !volumeExact || pvBytes != volumeBytes {
		return zero, errDetachedV1Source
	}
	identity := strings.Join([]string{namespace, claimName, claimUID, name, text(pv, "metadata", "uid"),
		text(volume, "metadata", "uid"), text(tickets, "metadata", "uid"), phase, class, size.String()}, ":")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return detachedV1Source{Digest: digest, Phase: phase, Bytes: volumeBytes}, nil
}

func idleDetachedV1Claim(claim object, pods []object, phase, namespace, claimName, claimUID, pvName, class string) bool {
	matchingClaim := recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", namespace) &&
		text(claim, "metadata", "name") == claimName && text(claim, "status", "phase") == "Bound" && !hasDataMoverOwner(claim)
	switch phase {
	case "Released":
		return claim == nil || matchingClaim && text(claim, "metadata", "uid") != claimUID &&
			text(claim, "spec", "volumeName") != "" && text(claim, "spec", "volumeName") != pvName
	case "Bound":
		if !matchingClaim || text(claim, "metadata", "uid") != claimUID || text(claim, "spec", "volumeName") != pvName ||
			text(claim, "spec", "storageClassName") != class || pods == nil {
			return false
		}
		for _, pod := range pods {
			mounts, _ := value(pod, "spec", "volumes").([]any)
			for _, mount := range mounts {
				volume, _ := mount.(object)
				if text(volume, "persistentVolumeClaim", "claimName") == claimName {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func idleDetachedV1Backend(volume, tickets object, attachments []object, name string) bool {
	const api = "longhorn.io/v1beta2"
	const namespace = "longhorn-system"
	if !recoveredNativeObject(volume, api, "Volume", namespace) || text(volume, "metadata", "name") != name ||
		text(volume, "spec", "dataEngine") != "v1" || text(volume, "status", "state") != "detached" ||
		!absentOrEmptyString(value(volume, "spec", "nodeID")) ||
		!absentOrEmptyString(value(volume, "status", "currentNodeID")) ||
		!absentOrEmptyString(value(volume, "status", "pendingNodeID")) ||
		(value(volume, "status", "restoreRequired") != nil && value(volume, "status", "restoreRequired") != false) ||
		!recoveredNativeObject(tickets, api, "VolumeAttachment", namespace) || text(tickets, "metadata", "name") != name ||
		text(tickets, "spec", "volume") != name || !emptyDetachedV1Tickets(value(tickets, "spec", "attachmentTickets")) ||
		!emptyDetachedV1Tickets(value(tickets, "status", "attachmentTicketStatuses")) || attachments == nil {
		return false
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == name ||
			text(attachment, "spec", "source", "inlineVolumeSpec", "csi", "volumeHandle") == name {
			return false
		}
	}
	return true
}

func emptyDetachedV1Tickets(raw any) bool {
	tickets, isMap := raw.(object)
	return raw == nil || isMap && len(tickets) == 0
}

func (c client) readDetachedV1Source(ctx context.Context, pvName string) (detachedV1Source, error) {
	var zero detachedV1Source
	pv, err := c.get(ctx, "persistentvolume", "", pvName, false)
	if err != nil || text(pv, "metadata", "name") != pvName {
		return zero, errDetachedV1Source
	}
	namespace := text(pv, "spec", "claimRef", "namespace")
	claimName := text(pv, "spec", "claimRef", "name")
	if !kubeNamespace.MatchString(namespace) || !kubeName.MatchString(claimName) {
		return zero, errDetachedV1Source
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", namespace, claimName, true)
	if err != nil {
		return zero, err
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", pvName, false)
	if err != nil {
		return zero, err
	}
	tickets, err := c.get(ctx, "volumeattachments.longhorn.io", "longhorn-system", pvName, false)
	if err != nil {
		return zero, err
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return zero, err
	}
	var pods []object
	if text(pv, "status", "phase") == "Bound" {
		pods, err = c.list(ctx, "pods", namespace)
		if err != nil {
			return zero, err
		}
	}
	return bindDetachedV1Source(pv, claim, volume, tickets, pods, attachments)
}

func (c client) observeDetachedV1Source(ctx context.Context, source, pvName string) (detachedV1Source, error) {
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return detachedV1Source{}, err
	}
	before, err := c.readDetachedV1Source(ctx, pvName)
	if err != nil {
		return detachedV1Source{}, err
	}
	after, err := c.readDetachedV1Source(ctx, pvName)
	if err != nil || before != after {
		return detachedV1Source{}, errDetachedV1Source
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return detachedV1Source{}, err
	}
	return after, nil
}
