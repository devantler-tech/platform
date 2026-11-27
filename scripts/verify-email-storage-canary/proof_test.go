package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

func TestDispatchAuthorization(t *testing.T) {
	valid := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "40783170105", "GITHUB_SHA": strings.Repeat("a", 40), "EMAIL_CANARY_CONFIRM": "verify-email-storage-canary", "RUNNER_ENVIRONMENT": "github-hosted"}
	if run, engine, err := authorize(func(k string) string { return valid[k] }); err != nil || run != valid["GITHUB_RUN_ID"] || engine != "v1" {
		t.Fatal("the original dispatch lost its V1 profile", err)
	}
	if run, engine, err := authorize(func(k string) string {
		if k == "EMAIL_CANARY_CONFIRM" {
			return "verify-email-storage-v2-canary"
		}
		return valid[k]
	}); err != nil || run != valid["GITHUB_RUN_ID"] || engine != "v2" {
		t.Fatal("the explicit V2 canary dispatch was refused:", err)
	}
	for key, bad := range map[string]string{"GITHUB_REPOSITORY": "other/platform", "GITHUB_REF": "refs/pull/1/merge", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_RUN_ID": "1/../../private-mail", "GITHUB_SHA": "main", "EMAIL_CANARY_CONFIRM": "yes", "RUNNER_ENVIRONMENT": "self-hosted"} {
		t.Run(key, func(t *testing.T) {
			clone := map[string]string{}
			for k, v := range valid {
				clone[k] = v
			}
			clone[key] = bad
			if _, _, err := authorize(func(k string) string { return clone[k] }); err == nil {
				t.Fatal("unsafe dispatch accepted")
			}
		})
	}
}

func validVolume() object {
	return object{"metadata": object{"name": "pvc-test", "uid": "volume-uid"}, "spec": object{"encrypted": true, "numberOfReplicas": float64(3), "dataEngine": "v1", "nodeID": ""}, "status": object{"state": "detached"}}
}
func TestVolumeProofCannotSubstituteDesiredConfiguration(t *testing.T) {
	good := validVolume()
	if err := checkVolume(good, "pvc-test", "volume-uid", "v1", true); err != nil {
		t.Fatal(err)
	}
	child(good, "spec")["dataEngine"] = "v2"
	if err := checkVolume(good, "pvc-test", "volume-uid", "v2", true); err != nil {
		t.Fatal(err)
	}
	if checkVolume(validVolume(), "pvc-test", "volume-uid", "v2", true) == nil {
		t.Fatal("a V1 backend satisfied the V2 proof")
	}
	for _, test := range []struct {
		name   string
		mutate func(object)
	}{
		{"unencrypted", func(o object) { child(o, "spec")["encrypted"] = false }},
		{"replacement", func(o object) { child(o, "metadata")["uid"] = "other-uid" }},
		{"different-volume", func(o object) { child(o, "metadata")["name"] = "private-mail-data" }},
		{"still-attached", func(o object) { child(o, "status")["state"] = "attached" }},
		{"still-selected", func(o object) { child(o, "spec")["nodeID"] = "node" }},
		{"different-replication", func(o object) { child(o, "spec")["numberOfReplicas"] = float64(1) }},
		{"different-engine", func(o object) { child(o, "spec")["dataEngine"] = "v2" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := validVolume()
			test.mutate(o)
			if checkVolume(o, "pvc-test", "volume-uid", "v1", true) == nil {
				t.Fatal("unproven encrypted detach accepted")
			}
		})
	}
}

func TestV2ReplicaQuorumRequiresThreeIndependentNativeCopies(t *testing.T) {
	volume := validVolume()
	newReplicas := func() []object {
		var replicas []object
		for i := range 3 {
			suffix := strconv.Itoa(i)
			owners := []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": "pvc-test", "uid": "volume-uid"}}
			replicas = append(replicas, object{
				"metadata": object{"uid": "replica-" + suffix, "namespace": "longhorn-system", "ownerReferences": owners},
				"spec": object{"volumeName": "pvc-test", "dataEngine": "v2", "nodeID": "node-" + suffix,
					"diskID": "disk-" + suffix, "healthyAt": "2026-11-26T10:00:00Z"},
				"status": object{"currentState": "running"},
			})
		}
		return replicas
	}
	if replicas := newReplicas(); !longhorninventory.V2ReplicaQuorum(volume, replicas) || longhorninventory.V2ReplicaQuorum(volume, replicas[:2]) {
		t.Fatal("expected exactly three healthy copies")
	}
	for _, test := range []struct {
		name   string
		change func(object)
	}{
		{"shared node", func(o object) { child(o, "spec")["nodeID"] = "node-1" }},
		{"shared disk", func(o object) { child(o, "spec")["diskID"] = "disk-1" }},
		{"wrong owner", func(o object) { items(child(o, "metadata"), "ownerReferences")[0]["uid"] = "replacement" }},
		{"wrong engine", func(o object) { child(o, "spec")["dataEngine"] = "v1" }},
		{"failed", func(o object) { child(o, "spec")["failedAt"] = "2026-11-26T10:01:00Z" }},
		{"inactive", func(o object) { child(o, "status")["currentState"] = "stopped" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			replicas := newReplicas()
			test.change(replicas[0])
			if longhorninventory.V2ReplicaQuorum(volume, replicas) {
				t.Fatal("unproven replica quorum accepted")
			}
		})
	}
}
func pendingPod() object {
	return object{"metadata": object{"uid": "pod-uid"}, "status": object{"phase": "Pending", "containerStatuses": []any{object{"name": "probe", "state": object{"waiting": object{"reason": "ContainerCreating"}}}}}}
}
func mountEvent(message string) object {
	return object{"involvedObject": object{"uid": "pod-uid", "kind": "Pod"}, "reason": "FailedMount", "source": object{"component": "kubelet"}, "message": message}
}
func TestNegativeProofRequiresSpecificMountFailureOnTheCurrentPod(t *testing.T) {
	missing := mountEvent("MountVolume.SetUp failed: secrets \"encryption-key\" not found")
	wrong := mountEvent("MountVolume.MountDevice failed for volume \"pvc-test\": cryptsetup: No key available with this passphrase.")
	if !negativeMount(pendingPod(), []object{missing}, "pod-uid", "pvc-test", "missing") {
		t.Fatal("real missing-key evidence refused")
	}
	if !negativeMount(pendingPod(), []object{wrong}, "pod-uid", "pvc-test", "wrong") {
		t.Fatal("real wrong-key evidence refused")
	}
	for _, test := range []struct {
		name  string
		pod   object
		event object
		mode  string
	}{
		{"generic-pending", pendingPod(), mountEvent("waiting for attachment"), "wrong"},
		{"other-volume", pendingPod(), mountEvent("volume pvc-other: No key available with this passphrase."), "wrong"},
		{"missing-is-not-wrong", pendingPod(), missing, "wrong"},
		{"wrong-is-not-missing", pendingPod(), wrong, "missing"},
		{"other-pod", pendingPod(), object{"involvedObject": object{"uid": "old-pod", "kind": "Pod"}, "reason": "FailedMount", "source": object{"component": "kubelet"}, "message": wrong["message"]}, "wrong"},
		{"container-started", object{"metadata": object{"uid": "pod-uid"}, "status": object{"phase": "Pending", "containerStatuses": []any{object{"name": "probe", "state": object{"terminated": object{"exitCode": float64(0)}}}}}}, wrong, "wrong"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if negativeMount(test.pod, []object{test.event}, "pod-uid", "pvc-test", test.mode) {
				t.Fatal("non-proving mount observation accepted")
			}
		})
	}
}
func TestSuccessfulProbeNeedsExitAndReceipt(t *testing.T) {
	pod := object{"metadata": object{"uid": "pod-uid"}, "status": object{"phase": "Succeeded", "containerStatuses": []any{object{"name": "probe", "imageID": "docker-pullable://image@sha256:abc", "state": object{"terminated": object{"exitCode": float64(0)}}}}}}
	if !probeSucceeded(pod, "pod-uid", "EMAIL_VOLUME_PROBE=PASS phase=read\n", "read") {
		t.Fatal("complete native probe refused")
	}
	if probeSucceeded(pod, "pod-uid", "EMAIL_VOLUME_PROBE=PASS phase=write\n", "read") {
		t.Fatal("wrong receipt accepted")
	}
	if probeSucceeded(pod, "new-pod", "EMAIL_VOLUME_PROBE=PASS phase=read\n", "read") {
		t.Fatal("replacement pod accepted")
	}
	child(pod, "status")["phase"] = "Pending"
	if probeSucceeded(pod, "pod-uid", "EMAIL_VOLUME_PROBE=PASS phase=read\n", "read") {
		t.Fatal("log alone accepted")
	}
}
func TestDeletionBindsUIDAtTheServer(t *testing.T) {
	b, err := deletionBody("owned-uid")
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if json.Unmarshal(b, &o) != nil || text(child(o, "preconditions"), "uid") != "owned-uid" || text(o, "propagationPolicy") != "Foreground" {
		t.Fatal("delete is not atomically bound to the created identity")
	}
	if _, err = deletionBody(""); err == nil {
		t.Fatal("unbound cleanup accepted")
	}
}
