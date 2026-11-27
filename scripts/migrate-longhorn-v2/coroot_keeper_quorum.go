package main

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	corootKeeperProbe = "curl -fsS --max-time 4 http://127.0.0.1:9182/ready >/dev/null; " +
		"exec 3<>/dev/tcp/127.0.0.1/9181; printf mntr >&3; timeout 6 cat <&3"

	observeCorootKeeperOperation = "observe-coroot-v2-keeper"
	corootKeeperNamespace        = "observability"
	corootKeeperName             = "coroot-clickhouse-keeper"
	corootKeeperContainer        = "clickhouse-keeper"
	corootKeeperImage            = "ghcr.io/coroot/clickhouse:26.10.3-ubi9-0"
	corootKeeperImageID          = "ghcr.io/coroot/clickhouse@sha256:7a7bced745cb7975ce8b249da1e08192faf8adbcc24aae45060ccc6d7c9187a3"
)

var errCorootKeeperQuorum = errors.New("the Coroot Keeper quorum and exact serving members are unverified")

type corootKeeperMember struct {
	Pod, UID, Node, ContainerID string
	Leader                      bool
}

func authorizeCorootKeeperObservation(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != observeCorootKeeperOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errCorootKeeperQuorum
	}
	return authorizeWorkflow(getenv, observeCorootKeeperOperation)
}

func observedKeeperLeader(raw []byte, expectedFollowers int) (leader bool, err error) {
	if len(raw) == 0 || len(raw) > 16<<10 || !strings.HasSuffix(string(raw), "\n") {
		return false, errCorootKeeperQuorum
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		key, field, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(key, "zk_") || field == "" || strings.ContainsAny(field, "\t\r\x00") || values[key] != "" {
			return false, errCorootKeeperQuorum
		}
		values[key] = field
	}
	switch values["zk_server_state"] {
	case "leader":
		if values["zk_followers"] != strconv.Itoa(expectedFollowers) || values["zk_synced_followers"] != strconv.Itoa(expectedFollowers) {
			return false, errCorootKeeperQuorum
		}
		return true, nil
	case "follower":
		if values["zk_followers"] != "" || values["zk_synced_followers"] != "" {
			return false, errCorootKeeperQuorum
		}
		return false, nil
	default:
		return false, errCorootKeeperQuorum
	}
}

func corootKeeperSelector() object {
	return object{"app.kubernetes.io/managed-by": "coroot-operator", "app.kubernetes.io/part-of": "coroot",
		"app.kubernetes.io/component": corootKeeperContainer}
}

func corootKeeperSelected(pod object) bool {
	for key, expected := range corootKeeperSelector() {
		if value(pod, "metadata", "labels", key) != expected {
			return false
		}
	}
	return true
}

func corootKeeperControllerReady(stateful, budget object) bool {
	selector := corootKeeperSelector()
	return recoveredNativeObject(stateful, "apps/v1", "StatefulSet", corootKeeperNamespace) &&
		text(stateful, "metadata", "name") == corootKeeperName &&
		value(stateful, "metadata", "generation") == value(stateful, "status", "observedGeneration") &&
		value(stateful, "spec", "replicas") == float64(3) && value(stateful, "status", "readyReplicas") == float64(3) &&
		value(stateful, "status", "updatedReplicas") == float64(3) &&
		text(stateful, "status", "currentRevision") != "" &&
		text(stateful, "status", "currentRevision") == text(stateful, "status", "updateRevision") &&
		reflect.DeepEqual(value(stateful, "spec", "selector"), object{"matchLabels": selector}) &&
		text(stateful, "spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted") == "Retain" &&
		text(stateful, "spec", "persistentVolumeClaimRetentionPolicy", "whenScaled") == "Retain" &&
		recoveredNativeObject(budget, "policy/v1", "PodDisruptionBudget", corootKeeperNamespace) &&
		text(budget, "metadata", "name") == corootKeeperName &&
		reflect.DeepEqual(value(budget, "spec", "selector"), object{"matchLabels": selector}) &&
		value(budget, "spec", "maxUnavailable") == float64(1) &&
		value(budget, "metadata", "generation") == value(budget, "status", "observedGeneration") &&
		value(budget, "status", "currentHealthy") == float64(3) && value(budget, "status", "desiredHealthy") == float64(2) &&
		value(budget, "status", "expectedPods") == float64(3) && value(budget, "status", "disruptionsAllowed") == float64(1) &&
		absentOrEmptyMap(value(budget, "status", "disruptedPods"))
}

func bindCorootKeeperMember(pod, stateful object, ordinal int) (corootKeeperMember, error) {
	var member corootKeeperMember
	name := corootKeeperName + "-" + strconv.Itoa(ordinal)
	if !recoveredNativeObject(pod, "v1", "Pod", corootKeeperNamespace) || text(pod, "metadata", "name") != name ||
		text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
		text(pod, "spec", "nodeName") == "" || text(pod, "metadata", "labels", "controller-revision-hash") != text(stateful, "status", "currentRevision") {
		return member, errCorootKeeperQuorum
	}
	if !corootKeeperSelected(pod) {
		return member, errCorootKeeperQuorum
	}
	owners, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 {
		return member, errCorootKeeperQuorum
	}
	owner, _ := owners[0].(object)
	if text(owner, "apiVersion") != "apps/v1" || text(owner, "kind") != "StatefulSet" || text(owner, "name") != corootKeeperName ||
		text(owner, "uid") != text(stateful, "metadata", "uid") || value(owner, "controller") != true {
		return member, errCorootKeeperQuorum
	}
	volumes, _ := value(pod, "spec", "volumes").([]any)
	data := 0
	for _, raw := range volumes {
		volume, _ := raw.(object)
		if value(volume, "persistentVolumeClaim") == nil {
			continue
		}
		if text(volume, "name") != "data" || text(volume, "persistentVolumeClaim", "claimName") != "data-"+name ||
			value(volume, "persistentVolumeClaim", "readOnly") == true {
			return member, errCorootKeeperQuorum
		}
		data++
	}
	containers, _ := value(pod, "spec", "containers").([]any)
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if data != 1 || len(containers) != 1 || len(statuses) != 1 {
		return member, errCorootKeeperQuorum
	}
	spec, _ := containers[0].(object)
	status, _ := statuses[0].(object)
	containerID := text(status, "containerID")
	if text(spec, "name") != corootKeeperContainer || text(spec, "image") != corootKeeperImage ||
		text(status, "name") != corootKeeperContainer || text(status, "imageID") != corootKeeperImageID ||
		value(status, "ready") != true || text(status, "state", "running", "startedAt") == "" ||
		!postgresContainerID.MatchString(containerID) {
		return member, errCorootKeeperQuorum
	}
	return corootKeeperMember{Pod: name, UID: text(pod, "metadata", "uid"), Node: text(pod, "spec", "nodeName"), ContainerID: containerID}, nil
}

type corootKeeperQuorumReceipt struct {
	Controller, Budget benchmarkResource
	Members            [3]corootKeeperMember
	Disks              [3]corootKeeperProviderDisk
	V2Claims           [3]benchmarkResource
}

func observeCorootKeeperQuorum(ctx context.Context, c client, source, run string) (corootKeeperQuorumReceipt, error) {
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	capacity, err := activation.recoveryClaimStorage(ctx, corootKeeperDataSize)
	if err != nil {
		return corootKeeperQuorumReceipt{}, err
	}
	return readCorootKeeperQuorum(ctx, c, source, capacity.Disks)
}

func readCorootKeeperQuorum(ctx context.Context, c client, source string, qualified []cnpgRecoveryDiskBinding) (corootKeeperQuorumReceipt, error) {
	if c.context != "admin@prod" {
		return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return corootKeeperQuorumReceipt{}, err
	}
	// Both endpoints are read-only and are bound to the same reviewed process.
	var previous [3]corootKeeperMember
	var previousDisks [3]corootKeeperProviderDisk
	var previousV2 [3]benchmarkResource
	var controllers [2]benchmarkResource
	for pass := range 2 {
		var current [3]corootKeeperMember
		var disks [3]corootKeeperProviderDisk
		var v2 [3]benchmarkResource
		stateful, err := c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
		if err != nil {
			return corootKeeperQuorumReceipt{}, err
		}
		budget, err := c.get(ctx, "poddisruptionbudget", corootKeeperNamespace, corootKeeperName, false)
		if err != nil || !corootKeeperControllerReady(stateful, budget) {
			return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
		}
		ids := [2]benchmarkResource{benchmarkIdentity(stateful), benchmarkIdentity(budget)}
		if pass == 1 && controllers != ids {
			return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
		}
		controllers = ids
		pods, err := c.list(ctx, "pods", corootKeeperNamespace)
		if err != nil {
			return corootKeeperQuorumReceipt{}, err
		}
		selected := map[string]object{}
		for _, pod := range pods {
			if corootKeeperSelected(pod) {
				name := text(pod, "metadata", "name")
				if selected[name] != nil {
					return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
				}
				selected[name] = pod
			}
		}
		if len(selected) != len(current) {
			return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
		}
		attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
		if err != nil {
			return corootKeeperQuorumReceipt{}, err
		}
		nodes := map[string]bool{}
		handles := map[string]bool{}
		leaders := 0
		for id := range current {
			name := corootKeeperName + "-" + strconv.Itoa(id)
			member, err := bindCorootKeeperMember(selected[name], stateful, id)
			if err != nil || nodes[member.Node] {
				return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
			}
			nodes[member.Node] = true
			claim, err := c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, "data-"+name, false)
			if err != nil {
				return corootKeeperQuorumReceipt{}, err
			}
			if text(claim, "spec", "storageClassName") == "longhorn-v2" {
				if err := verifyExistingKeeperV2Member(ctx, c, qualified, id, member, claim); err != nil {
					return corootKeeperQuorumReceipt{}, err
				}
				v2[id] = benchmarkIdentity(claim)
			} else {
				pv, err := c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
				if err != nil {
					return corootKeeperQuorumReceipt{}, err
				}
				disk, err := bindCorootKeeperProviderDisk(member, stateful, claim, pv, attachments)
				if err != nil || handles[disk.Handle] {
					return corootKeeperQuorumReceipt{}, errCorootKeeperStorage
				}
				handles[disk.Handle] = true
				disks[id] = disk
			}
			member.Leader, err = probeCorootKeeperMember(ctx, c, member, stateful, id, 2)
			if err != nil {
				return corootKeeperQuorumReceipt{}, err
			}
			if member.Leader {
				leaders++
			}
			current[id] = member
		}
		if leaders != 1 || pass == 1 && (previous != current || previousDisks != disks || previousV2 != v2) {
			return corootKeeperQuorumReceipt{}, errCorootKeeperQuorum
		}
		previous, previousDisks, previousV2 = current, disks, v2
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return corootKeeperQuorumReceipt{}, err
	}
	return corootKeeperQuorumReceipt{Controller: controllers[0], Budget: controllers[1], Members: previous, Disks: previousDisks, V2Claims: previousV2}, nil
}
