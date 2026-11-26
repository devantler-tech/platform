package main

import (
	"errors"
	"time"
)

// Published quay.io/cilium/cilium v1.20.5 OCI index and its Linux children.
// Native runtime/version output alone cannot authorize an arbitrary executable.
const recoveryAgentIndex = "sha256:aa72499f075ad729947ea91b026218aceba24f1a8e4534bf36b8368c7c561526"
const recoveryAgentImage = "quay.io/cilium/cilium:v1.20.5@" + recoveryAgentIndex

func recoveryAgentExecutable(imageID, os, arch string) bool {
	if os != "linux" {
		return false
	}
	var child string
	switch arch {
	case "amd64":
		child = "sha256:a54ced05354670afb7fecd075e91130dff5cca16518da7ff3d1ef093c5c2d0ae"
	case "arm64":
		child = "sha256:dc2a1862e29de053e588980e121acb8b2e534206e9b3047ef5386f8c1eb13c29"
	default:
		return false
	}
	return imageID == "quay.io/cilium/cilium@"+recoveryAgentIndex || imageID == "quay.io/cilium/cilium@"+child
}

// Compare the complete value before and after every native evidence collection
// and live probe. A Pod UID alone does not identify a container process.
type recoveryCiliumAgent struct {
	Pod, PodUID, Node, NodeUID, BootID, DaemonSetUID string
	DeclaredImage, ImageID, ContainerID, StartedAt   string
	DaemonSetGeneration, Restarts                    uint64
}

// Payload binding only. The caller must bind the DaemonSet to its reviewed
// deployment, locate exactly one owned agent on the recovery Pod's Node, and
// collect all native runtime/policy/endpoint observations from this process.
// The native build and configuration are checked separately. This performs no
// exec, proves no policy import, and authorizes no recovery mutation.
func bindRecoveryCiliumAgent(pod, ds, node object) (recoveryCiliumAgent, error) {
	var proof recoveryCiliumAgent
	refused := errors.New("recovery policy evidence lacks one current owned Cilium agent process")
	for _, o := range []object{pod, ds, node} {
		if !isolationUID.MatchString(text(o, "metadata", "uid")) || value(o, "metadata", "deletionTimestamp") != nil {
			return proof, refused
		}
	}
	if text(ds, "apiVersion") != "apps/v1" || text(ds, "kind") != "DaemonSet" || text(ds, "metadata", "namespace") != "kube-system" || text(ds, "metadata", "name") != "cilium" || value(ds, "spec", "template", "spec", "hostNetwork") != true ||
		text(node, "apiVersion") != "v1" || text(node, "kind") != "Node" || !isolationUID.MatchString(text(node, "status", "nodeInfo", "bootID")) || !recoveryAgentReady(node) ||
		text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || text(pod, "metadata", "name") == "" || text(pod, "metadata", "namespace") != "kube-system" || text(pod, "metadata", "labels", "k8s-app") != "cilium" || text(pod, "spec", "nodeName") == "" || text(pod, "spec", "nodeName") != text(node, "metadata", "name") || value(pod, "spec", "hostNetwork") != true || text(pod, "status", "phase") != "Running" || !recoveryAgentReady(pod) {
		return proof, refused
	}
	generation, genOK := nativeObservationUint(value(ds, "metadata", "generation"))
	observed, observedOK := nativeObservationUint(value(ds, "status", "observedGeneration"))
	desired, desiredOK := nativeObservationUint(value(ds, "status", "desiredNumberScheduled"))
	if !genOK || generation == 0 || generation > 2147483647 || !observedOK || generation != observed || !desiredOK || desired == 0 || desired > 2147483647 {
		return proof, refused
	}
	for _, field := range []string{"currentNumberScheduled", "updatedNumberScheduled", "numberReady"} {
		actual, valid := nativeObservationUint(value(ds, "status", field))
		if !valid || actual != desired {
			return proof, refused
		}
	}
	dsStatus, _ := value(ds, "status").(object)
	for _, field := range []string{"numberUnavailable", "numberMisscheduled"} {
		if raw, present := dsStatus[field]; present {
			count, valid := nativeObservationUint(raw)
			if !valid || count != 0 {
				return proof, refused
			}
		} else if field == "numberMisscheduled" {
			return proof, refused
		}
	}
	owners, ok := value(pod, "metadata", "ownerReferences").([]any)
	if !ok || len(owners) != 1 {
		return proof, refused
	}
	owner, _ := owners[0].(object)
	if text(owner, "apiVersion") != "apps/v1" || text(owner, "kind") != "DaemonSet" || text(owner, "name") != "cilium" || text(owner, "uid") != text(ds, "metadata", "uid") || value(owner, "controller") != true {
		return proof, refused
	}
	var image string
	for _, raw := range []any{value(ds, "spec", "template", "spec", "containers"), value(pod, "spec", "containers")} {
		containers, valid := raw.([]any)
		if !valid || len(containers) != 1 {
			return proof, refused
		}
		c, _ := containers[0].(object)
		declared := text(c, "image")
		if text(c, "name") != "cilium-agent" || declared != recoveryAgentImage || image != "" && declared != image {
			return proof, refused
		}
		image = declared
	}
	statuses, ok := value(pod, "status", "containerStatuses").([]any)
	if !ok || len(statuses) != 1 {
		return proof, refused
	}
	status, _ := statuses[0].(object)
	restarts, restartsOK := nativeObservationUint(status["restartCount"])
	startedAt := text(status, "state", "running", "startedAt")
	started, timeErr := time.Parse(time.RFC3339Nano, startedAt)
	if text(status, "name") != "cilium-agent" || value(status, "ready") != true || !restartsOK || restarts > 2147483647 || !recoveryAgentExecutable(text(status, "imageID"), text(node, "status", "nodeInfo", "operatingSystem"), text(node, "status", "nodeInfo", "architecture")) || !postgresContainerID.MatchString(text(status, "containerID")) || timeErr != nil || started.IsZero() || value(status, "state", "waiting") != nil || value(status, "state", "terminated") != nil {
		return proof, refused
	}
	proof = recoveryCiliumAgent{Pod: text(pod, "metadata", "name"), PodUID: text(pod, "metadata", "uid"), Node: text(node, "metadata", "name"), NodeUID: text(node, "metadata", "uid"), BootID: text(node, "status", "nodeInfo", "bootID"), DaemonSetUID: text(ds, "metadata", "uid"), DaemonSetGeneration: generation,
		DeclaredImage: image, ImageID: text(status, "imageID"), ContainerID: text(status, "containerID"), StartedAt: startedAt, Restarts: restarts}
	return proof, nil
}

func recoveryAgentReady(o object) bool {
	status, _ := value(o, "status").(object)
	return ready(status)
}
