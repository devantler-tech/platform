package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func seedPodFixture(t *testing.T) object {
	t.Helper()
	assets, err := benchmarkScratchAssets(client{nativeSeed: true}, "v1", "1234", benchmarkNamespace)
	if err != nil {
		t.Fatal(err)
	}
	pod := assets[len(assets)-1]
	pod["metadata"].(object)["uid"] = "source-pod-uid"
	pod["metadata"].(object)["resourceVersion"] = "17"
	pod["spec"].(object)["nodeName"] = "prod-worker-3"
	pod["status"] = object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}, "containerStatuses": []any{object{
		"name": "fio", "imageID": "docker-pullable://" + fioImage, "ready": true, "restartCount": float64(0), "state": object{"running": object{"startedAt": "2026-11-15T06:00:00Z"}}}}}
	body, err := json.Marshal(pod)
	if err != nil || json.Unmarshal(body, &pod) != nil {
		t.Fatal("invalid native pod fixture", err)
	}
	return pod
}

func TestNativeSeedChecksTheOriginalReadyExecutableBeforePayloadAccess(t *testing.T) {
	for _, fault := range []string{"", "replacement pod", "other workspace", "wrong client", "deleting", "not ready", "benchmark script", "wrong image", "wrong running image", "restart", "unbounded deadline", "injected init container", "automatic token", "different mounted claim", "different data mount", "extra volume", "injected environment"} {
		t.Run(fault, func(t *testing.T) {
			pod := seedPodFixture(t)
			switch fault {
			case "replacement pod":
				pod["metadata"].(object)["uid"] = "replacement-uid"
			case "other workspace":
				pod["metadata"].(object)["namespace"] = "foreign"
			case "wrong client":
				pod["spec"].(object)["nodeName"] = "prod-worker-1"
			case "deleting":
				pod["metadata"].(object)["deletionTimestamp"] = "2026-11-15T06:10:00Z"
			case "not ready":
				pod["status"].(object)["conditions"] = []any{}
			case "benchmark script":
				value(pod, "spec", "containers").([]any)[0].(object)["args"] = []any{fioScript}
			case "wrong image":
				value(pod, "spec", "containers").([]any)[0].(object)["image"] = "unreviewed"
			case "wrong running image":
				value(pod, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "docker-pullable://other@sha256:" + strings.Repeat("a", 64)
			case "restart":
				value(pod, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
			case "unbounded deadline":
				delete(pod["spec"].(object), "activeDeadlineSeconds")
			case "injected init container":
				pod["spec"].(object)["initContainers"] = []any{object{"name": "foreign"}}
			case "automatic token":
				pod["spec"].(object)["automountServiceAccountToken"] = true
			case "different mounted claim":
				value(pod, "spec", "volumes").([]any)[0].(object)["persistentVolumeClaim"] = object{"claimName": "other-data"}
			case "different data mount":
				value(pod, "spec", "containers").([]any)[0].(object)["volumeMounts"] = []any{object{"name": "scratch", "mountPath": "/data"}}
			case "extra volume":
				pod["spec"].(object)["volumes"] = append(value(pod, "spec", "volumes").([]any), object{"name": "foreign", "secret": object{"secretName": "other"}})
			case "injected environment":
				value(pod, "spec", "containers").([]any)[0].(object)["envFrom"] = []any{object{"secretRef": object{"name": "other"}}}
			}
			if err := verifyNativeSeedPod(pod, "source-pod-uid"); (err == nil) != (fault == "") {
				t.Fatal("payload access did not require its original bounded executable", err)
			}
		})
	}
}

func TestNativeSeedPayloadReceiptContainsTheExactVerifiedFile(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, output := range []string{digest + "  payload.bin\n", digest + "  other.bin\n", "bad  payload.bin", digest + "  payload.bin\n" + digest + "  payload.bin\n", digest + "\n"} {
		got, err := decodeSeedPayload(output)
		valid := output == digest+"  payload.bin\n"
		if (err == nil) != valid || valid && got != digest {
			t.Fatal("the payload receipt was not bound to the one verified source file", err)
		}
	}
}
