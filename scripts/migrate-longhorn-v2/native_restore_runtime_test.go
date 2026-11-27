package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func nativeRestoreVeleroFixture() (object, []object, []object) {
	labels := object{"app.kubernetes.io/name": "velero", "app.kubernetes.io/instance": "velero"}
	container := object{"name": "velero", "image": "velero/velero:v1.18.4"}
	deployment := object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": object{"name": "velero", "namespace": "velero", "uid": "deployment-uid", "resourceVersion": "10", "generation": float64(3),
		"annotations": object{"meta.helm.sh/release-name": "velero", "meta.helm.sh/release-namespace": "velero"}},
		"spec":   object{"replicas": float64(1), "selector": object{"matchLabels": labels}, "template": object{"metadata": object{"labels": labels}, "spec": object{"containers": []any{container}}}},
		"status": object{"observedGeneration": float64(3), "replicas": float64(1), "updatedReplicas": float64(1), "readyReplicas": float64(1), "availableReplicas": float64(1)}}
	replicaSet := object{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": object{"name": "velero-hash", "namespace": "velero", "uid": "rs-uid", "resourceVersion": "11",
		"ownerReferences": []any{object{"apiVersion": "apps/v1", "kind": "Deployment", "name": "velero", "uid": "deployment-uid", "controller": true}}},
		"spec":   object{"replicas": float64(1), "template": value(deployment, "spec", "template")},
		"status": object{"replicas": float64(1), "readyReplicas": float64(1), "availableReplicas": float64(1)}}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": "velero-hash-pod", "namespace": "velero", "uid": "pod-uid", "resourceVersion": "12", "labels": labels,
		"ownerReferences": []any{object{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "velero-hash", "uid": "rs-uid", "controller": true}}},
		"spec": object{"nodeName": "fixture-worker", "containers": []any{container}},
		"status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}, "containerStatuses": []any{object{"name": "velero", "ready": true, "restartCount": float64(0),
			"imageID": "docker.io/velero/velero@sha256:" + strings.Repeat("a", 64), "containerID": "containerd://" + strings.Repeat("b", 64),
			"state": object{"running": object{"startedAt": "2026-11-15T15:00:00Z"}}}}}}
	return deployment, []object{replicaSet}, []object{pod}
}

func TestNativeRestoreVeleroRuntimeObservationRefusesChangingEpoch(t *testing.T) {
	for _, fault := range []string{"", "process changed", "source changed", "inventory failed"} {
		t.Run(fault, func(t *testing.T) {
			deployment, replicas, pods := nativeRestoreVeleroFixture()
			reads, sources := 0, 0
			source := strings.Repeat("c", 40)
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("a runtime read attempted to write")
				}
				if program == "git" && reflect.DeepEqual(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"}) {
					sources++
					if fault == "source changed" && sources > 1 {
						return fmt.Appendf(nil, "%s\trefs/heads/main\n", strings.Repeat("d", 40)), nil
					}
					return fmt.Appendf(nil, "%s\trefs/heads/main\n", source), nil
				}
				prefix := []string{"--context=admin@prod", "--request-timeout=20s", "get"}
				if program != "kubectl" || len(args) < 3 || !reflect.DeepEqual(args[:3], prefix) {
					t.Fatal("runtime observation escaped its read boundary", program, args)
				}
				reads++
				if fault == "inventory failed" {
					return nil, fmt.Errorf("fixture inventory unavailable")
				}
				if fault == "process changed" && reads > 3 {
					value(pods[0], "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("e", 64)
				}
				var result any
				switch strings.Join(args[3:], " ") {
				case "deployment velero --output=json --namespace=velero":
					result = deployment
				case "replicasets --output=json --namespace=velero":
					result = object{"items": replicas}
				case "pods --output=json --namespace=velero":
					result = object{"items": pods}
				default:
					t.Fatal("runtime observation selected an unexpected target", args)
				}
				return json.Marshal(result)
			}}
			epoch, err := captureNativeRestoreVelero(t.Context(), c, source)
			if (err == nil) != (fault == "") || err != nil && epoch.Digest != "" || fault == "" && (reads != 6 || sources != 2) {
				t.Fatal("changing or incomplete native helper observation was admitted", fault, reads, sources, err)
			}
		})
	}
}

func TestNativeRestoreVeleroRuntimeBindsOriginalDeploymentAndProcess(t *testing.T) {
	deployment, replicas, pods := nativeRestoreVeleroFixture()
	before, err := qualifyNativeRestoreVelero(deployment, replicas, pods)
	if err != nil || before.Digest != strings.Repeat("a", 64) || before.Deployment.UID != "deployment-uid" || before.ReplicaSet.UID != "rs-uid" || before.Pod.UID != "pod-uid" {
		t.Fatal("the live helper image was not joined to its reviewed server:", err)
	}
	valueObject(pods[0], "metadata")["resourceVersion"] = "22"
	after, err := qualifyNativeRestoreVelero(deployment, replicas, pods)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("an incidental status revision replaced the original process epoch:", err)
	}
	value(pods[0], "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("c", 64)
	restarted, err := qualifyNativeRestoreVelero(deployment, replicas, pods)
	if err != nil || reflect.DeepEqual(before, restarted) {
		t.Fatal("a changed executable epoch was not distinguished for the executor:", err)
	}
}

func TestNativeRestoreVeleroRuntimeRejectsUnqualifiedHelper(t *testing.T) {
	for _, fault := range []string{"generation", "rollout", "deployment ownership", "version", "foreign replica set", "duplicate replica set", "replica rollout", "foreign pod", "duplicate pod", "terminating", "not ready", "restarted", "no process", "no start", "no digest", "sidecar", "selector mismatch"} {
		t.Run(fault, func(t *testing.T) {
			deployment, replicas, pods := nativeRestoreVeleroFixture()
			status := value(pods[0], "status", "containerStatuses").([]any)[0].(object)
			switch fault {
			case "generation":
				valueObject(deployment, "status")["observedGeneration"] = float64(2)
			case "rollout":
				valueObject(deployment, "status")["updatedReplicas"] = float64(0)
			case "deployment ownership":
				delete(valueObject(deployment, "metadata"), "annotations")
			case "version":
				value(deployment, "spec", "template", "spec", "containers").([]any)[0].(object)["image"] = "velero/velero:latest"
			case "foreign replica set":
				value(replicas[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "other"
			case "duplicate replica set":
				replicas = append(replicas, replicas[0])
			case "replica rollout":
				valueObject(replicas[0], "status")["readyReplicas"] = float64(0)
			case "foreign pod":
				value(pods[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "other"
			case "duplicate pod":
				pods = append(pods, pods[0])
			case "terminating":
				valueObject(pods[0], "metadata")["deletionTimestamp"] = "2026-11-15T16:00:00Z"
			case "not ready":
				status["ready"] = false
			case "restarted":
				status["restartCount"] = float64(1)
			case "no process":
				delete(status, "containerID")
			case "no start":
				delete(status, "state")
			case "no digest":
				status["imageID"] = "velero/velero:v1.18.4"
			case "sidecar":
				valueObject(pods[0], "spec")["containers"] = append(value(pods[0], "spec", "containers").([]any), object{"name": "other"})
			case "selector mismatch":
				valueObject(pods[0], "metadata")["labels"] = object{"app.kubernetes.io/name": "unrelated"}
			}
			if got, err := qualifyNativeRestoreVelero(deployment, replicas, pods); err == nil || got.Digest != "" {
				t.Fatal("an unqualified or replaced helper runtime was admitted:", fault)
			}
		})
	}
}
