package main

import (
	"reflect"
	"strings"
	"testing"
)

func nativeReattachFixture() nativeReattachRequest {
	return nativeReattachRequest{Run: "5678", Token: strings.Repeat("d", 64), FirstNode: "prod-worker-1", Target: "prod-worker-2",
		Targets: map[string]string{"prod-worker-1": "node-uid-a", "prod-worker-2": "node-uid-b", "prod-worker-longhorn-v2-1": "node-uid-c"}}
}

func TestNativeReattachmentUsesANewConsumerOnAnotherOriginalTarget(t *testing.T) {
	r, seed := seedStateFixture(t, "source-cleaned")
	request := nativeReattachFixture()
	pod, err := nativeRestoreReattachPod(r, seed, request)
	if err != nil {
		t.Fatal(err)
	}
	if text(pod, "kind") != "Pod" || text(pod, "metadata", "name") != "fio" || text(pod, "metadata", "namespace") != benchmarkNamespace ||
		text(pod, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != request.Run ||
		text(pod, "metadata", "labels", "velero.io/exclude-from-backup") != "true" ||
		!reflect.DeepEqual(value(pod, "spec", "nodeSelector"), object{"kubernetes.io/hostname": request.Target}) ||
		value(pod, "spec", "initContainers") != nil || value(pod, "spec", "ephemeralContainers") != nil || value(pod, "metadata", "ownerReferences") != nil ||
		value(pod, "metadata", "annotations") != nil || value(pod, "spec", "nodeName") != nil {
		t.Fatal("reattachment can repeat a restore, escape its target roster or inherit the first consumer's identity")
	}
	containers, _ := value(pod, "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatal("reattachment has an extra consumer")
	}
	container := containers[0].(object)
	verify, err := nativeRestoreVerifyScript(seed.PayloadSHA256, request.Token)
	if err != nil || !reflect.DeepEqual(value(container, "args"), []any{verify + nativeReattachWait}) || text(container, "image") != fioImage {
		t.Fatal("reattachment can write over restored data or skip the original/new payload checks", err)
	}
	if value(pod, "spec", "automountServiceAccountToken") != false || value(pod, "spec", "hostUsers") != false ||
		text(pod, "spec", "restartPolicy") != "Never" || value(pod, "spec", "activeDeadlineSeconds") != 2700 {
		t.Fatal("the new consumer lost the bounded restricted execution contract")
	}
	assets, err := benchmarkResources("v2", request.Run, benchmarkNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var original object
	for _, asset := range assets {
		if text(asset, "kind") == "Pod" {
			original = asset
		}
	}
	for _, field := range []string{"securityContext", "volumes", "enableServiceLinks"} {
		if !reflect.DeepEqual(value(pod, "spec", field), value(original, "spec", field)) {
			t.Fatal("reattachment changed the fixed owned mount or Pod restrictions:", field)
		}
	}
	originalContainer := value(original, "spec", "containers").([]any)[0].(object)
	for _, field := range []string{"securityContext", "volumeMounts", "resources", "command", "imagePullPolicy"} {
		if !reflect.DeepEqual(value(container, field), value(originalContainer, field)) {
			t.Fatal("reattachment changed the fixed owned mount or container restrictions:", field)
		}
	}
}

func TestNativeReattachmentCannotReuseTheFirstNodeOrBorrowOtherTargets(t *testing.T) {
	for _, fault := range []string{"same node", "unlisted first node", "unlisted second node", "missing node UID", "duplicate node UID", "missing target", "extra target", "invalid node", "old run", "invalid run", "invalid token", "unfinished source"} {
		t.Run(fault, func(t *testing.T) {
			r, seed := seedStateFixture(t, "source-cleaned")
			request := nativeReattachFixture()
			switch fault {
			case "same node":
				request.Target = request.FirstNode
			case "unlisted first node":
				request.FirstNode = "other"
			case "unlisted second node":
				request.Target = "other"
			case "missing node UID":
				request.Targets[request.Target] = ""
			case "duplicate node UID":
				request.Targets[request.Target] = request.Targets[request.FirstNode]
			case "missing target":
				delete(request.Targets, "prod-worker-longhorn-v2-1")
			case "extra target":
				request.Targets["other"] = "node-uid-d"
			case "invalid node":
				delete(request.Targets, request.Target)
				request.Target = "../other"
				request.Targets[request.Target] = "node-uid-b"
			case "old run":
				request.Run = r.Run
			case "invalid run":
				request.Run = "../5678"
			case "invalid token":
				request.Token = "$(touch /data/payload.bin)"
			case "unfinished source":
				r, seed = seedStateFixture(t, "backup-verified")
			}
			if pod, err := nativeRestoreReattachPod(r, seed, request); err == nil || pod != nil {
				t.Fatal("an incomplete source or foreign target produced a consumer")
			}
		})
	}
}
