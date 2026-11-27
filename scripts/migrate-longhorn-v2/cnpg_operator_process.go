package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"
)

type cnpgOperatorProcess struct {
	Pod, PodUID, ReplicaSetUID, DeploymentUID string
	Node, NodeUID, BootID, Architecture       string
	Image, Container, StartedAt, SpecSHA      string
	ControllerSHA                             string
	Restarts, ReplicaSetGeneration            uint64
}

type cnpgControllerProfile struct {
	deployment, container, image string
	replicas                     uint64
	command, args, env           any
	runtimeImage                 func(string, string) bool
}

// bindCNPGOperatorProcess binds one ready manager through its native controller chain.
func bindCNPGOperatorProcess(pod, deployment, rs, node object) (cnpgOperatorProcess, error) {
	expectedEnv := []any{object{"name": "OPERATOR_IMAGE_NAME", "value": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2"}, object{"name": "OPERATOR_NAMESPACE", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.namespace"}}}, object{"name": "MONITORING_QUERIES_CONFIGMAP", "value": "cnpg-default-monitoring"}}
	return bindCNPGControllerProcess(pod, deployment, rs, node, cnpgControllerProfile{
		deployment: "cloudnative-pg", container: "manager", image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2", replicas: 2,
		command: []any{"/manager"}, args: []any{"controller", "--leader-elect", "--max-concurrent-reconciles=10", "--config-map-name=cnpg-controller-manager-config", "--webhook-port=9443"}, env: expectedEnv, runtimeImage: cnpgReviewedManagerImage,
	})
}

// bindCNPGControllerProcess shares identity and health checks between compiled release profiles.
func bindCNPGControllerProcess(pod, deployment, rs, node object, profile cnpgControllerProfile) (cnpgOperatorProcess, error) {
	refused := errors.New("operator process lacks its healthy reviewed image and native controller chain")
	for _, resource := range []object{pod, deployment, rs, node} {
		if !kubeName.MatchString(text(resource, "metadata", "name")) || text(resource, "metadata", "uid") == "" || text(resource, "metadata", "resourceVersion") == "" || value(resource, "metadata", "deletionTimestamp") != nil {
			return cnpgOperatorProcess{}, refused
		}
	}
	if text(deployment, "apiVersion") != "apps/v1" || text(deployment, "kind") != "Deployment" || text(deployment, "metadata", "name") != profile.deployment || text(rs, "apiVersion") != "apps/v1" || text(rs, "kind") != "ReplicaSet" || text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || text(node, "apiVersion") != "v1" || text(node, "kind") != "Node" {
		return cnpgOperatorProcess{}, refused
	}
	for _, resource := range []object{pod, deployment, rs} {
		if text(resource, "metadata", "namespace") != "cnpg-system" || !kubeName.MatchString(text(resource, "metadata", "name")) {
			return cnpgOperatorProcess{}, refused
		}
	}
	if !cnpgOperatorOwner(pod, "ReplicaSet", rs) || !cnpgOperatorOwner(rs, "Deployment", deployment) {
		return cnpgOperatorProcess{}, refused
	}
	for _, resource := range []object{deployment, rs} {
		generation, ok := nativeObservationUint(value(resource, "metadata", "generation"))
		observed, observedOK := nativeObservationUint(value(resource, "status", "observedGeneration"))
		if !ok || !observedOK || generation == 0 || generation > 2147483647 || generation != observed {
			return cnpgOperatorProcess{}, refused
		}
		for _, field := range []string{"replicas", "readyReplicas", "availableReplicas"} {
			if count, valid := nativeObservationUint(value(resource, "status", field)); !valid || count != profile.replicas {
				return cnpgOperatorProcess{}, refused
			}
		}
		if count, valid := nativeObservationUint(value(resource, "spec", "replicas")); !valid || count != profile.replicas {
			return cnpgOperatorProcess{}, refused
		}
	}
	if count, ok := nativeObservationUint(value(deployment, "status", "updatedReplicas")); !ok || count != profile.replicas {
		return cnpgOperatorProcess{}, refused
	}
	podStatus, _ := value(pod, "status").(object)
	nodeStatus, _ := value(node, "status").(object)
	if text(pod, "status", "phase") != "Running" || !ready(podStatus) || !ready(nodeStatus) || text(pod, "spec", "nodeName") != text(node, "metadata", "name") || value(node, "spec", "unschedulable") == true || text(node, "status", "nodeInfo", "operatingSystem") != "linux" || text(node, "status", "nodeInfo", "bootID") == "" {
		return cnpgOperatorProcess{}, refused
	}
	for _, field := range []string{"initContainers", "ephemeralContainers"} {
		if value(pod, "spec", field) != nil {
			return cnpgOperatorProcess{}, refused
		}
	}
	for _, field := range []string{"hostPID", "hostIPC", "hostNetwork"} {
		if value(pod, "spec", field) == true {
			return cnpgOperatorProcess{}, refused
		}
	}
	containers, ok := value(pod, "spec", "containers").([]any)
	template, templateOK := value(deployment, "spec", "template", "spec", "containers").([]any)
	if !ok || !templateOK || len(containers) != 1 || len(template) != 1 {
		return cnpgOperatorProcess{}, refused
	}
	container, ok := containers[0].(object)
	proposed, templateOK := template[0].(object)
	if !ok || !templateOK {
		return cnpgOperatorProcess{}, refused
	}
	for _, field := range []string{"name", "image", "command", "args", "env", "envFrom", "workingDir", "lifecycle", "volumeDevices"} {
		if !recoveryJSONEqual(container[field], proposed[field]) {
			return cnpgOperatorProcess{}, refused
		}
	}
	if text(container, "name") != profile.container || text(container, "image") != profile.image || !recoveryJSONEqual(container["command"], profile.command) || !recoveryJSONEqual(container["args"], profile.args) || container["envFrom"] != nil || container["lifecycle"] != nil || container["volumeDevices"] != nil || text(container, "workingDir") != "" {
		return cnpgOperatorProcess{}, refused
	}
	if !recoveryJSONEqual(container["env"], profile.env) {
		return cnpgOperatorProcess{}, refused
	}
	if mounts := container["volumeMounts"]; mounts != nil {
		list, valid := mounts.([]any)
		if !valid {
			return cnpgOperatorProcess{}, refused
		}
		for _, raw := range list {
			mount, valid := raw.(object)
			mountPath := text(mount, "mountPath")
			if !valid || !path.IsAbs(mountPath) || path.Clean(mountPath) == "/" || path.Clean(mountPath) == "/manager" {
				return cnpgOperatorProcess{}, refused
			}
		}
	}
	statuses, ok := value(pod, "status", "containerStatuses").([]any)
	if !ok || len(statuses) != 1 {
		return cnpgOperatorProcess{}, refused
	}
	status, ok := statuses[0].(object)
	restarts, restartOK := nativeObservationUint(value(status, "restartCount"))
	architecture := text(node, "status", "nodeInfo", "architecture")
	started := text(status, "state", "running", "startedAt")
	at, err := time.Parse(time.RFC3339Nano, started)
	if !ok || !restartOK || restarts > 2147483647 || text(status, "name") != profile.container || value(status, "ready") != true || !profile.runtimeImage(text(status, "imageID"), architecture) || !postgresContainerID.MatchString(text(status, "containerID")) || err != nil || at.IsZero() {
		return cnpgOperatorProcess{}, refused
	}
	spec, err := json.Marshal(value(pod, "spec"))
	if err != nil {
		return cnpgOperatorProcess{}, refused
	}
	controllers, err := json.Marshal(object{"deployment": value(deployment, "spec"), "replicaSet": value(rs, "spec")})
	if err != nil {
		return cnpgOperatorProcess{}, refused
	}
	parentGeneration, _ := nativeObservationUint(value(rs, "metadata", "generation"))
	return cnpgOperatorProcess{
		Pod: text(pod, "metadata", "name"), PodUID: text(pod, "metadata", "uid"),
		ReplicaSetUID: text(rs, "metadata", "uid"), ReplicaSetGeneration: parentGeneration,
		DeploymentUID: text(deployment, "metadata", "uid"),
		Node:          text(node, "metadata", "name"), NodeUID: text(node, "metadata", "uid"),
		BootID: text(node, "status", "nodeInfo", "bootID"), Architecture: architecture,
		Image: text(status, "imageID"), Container: text(status, "containerID"),
		StartedAt: started, Restarts: restarts,
		SpecSHA:       fmt.Sprintf("%x", sha256.Sum256(spec)),
		ControllerSHA: fmt.Sprintf("%x", sha256.Sum256(controllers)),
	}, nil
}

// cnpgOperatorOwner requires the single native controlling parent, not labels.
func cnpgOperatorOwner(child object, kind string, parent object) bool {
	owners, ok := value(child, "metadata", "ownerReferences").([]any)
	if !ok || len(owners) != 1 {
		return false
	}
	owner, ok := owners[0].(object)
	return ok && text(owner, "apiVersion") == "apps/v1" && text(owner, "kind") == kind && text(owner, "name") == text(parent, "metadata", "name") && text(owner, "uid") == text(parent, "metadata", "uid") && value(owner, "controller") == true
}

// cnpgReviewedManagerImage uses digest-checked public 1.30.2 index/platform manifests.
func cnpgReviewedManagerImage(image, architecture string) bool {
	platform := map[string]string{"amd64": "sha256:7172ea6acc90b0af204e513ee7b5f05fb04a44dcf96a8666655de88511758d7f", "arm64": "sha256:9be85561bd93beb1dfac8c1976562b4acb6585c9b58c0dfd5169fba8d6b85807"}[architecture]
	if platform == "" {
		return false
	}
	const repository = "ghcr.io/cloudnative-pg/cloudnative-pg@"
	return image == repository+"sha256:f96f6be3d81ec93819b443c665c575c609342fca840df90d4d87dae314a5b913" || image == repository+platform
}
