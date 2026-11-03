package main

import (
	"strings"
	"testing"
)

func canaryPodFixture() object {
	assets, err := canaryProbeResources("scratch", "class", "1234", "v1")
	if err != nil {
		panic(err)
	}
	o := assets[4]
	o["metadata"].(object)["uid"] = "probe"
	o["spec"].(object)["nodeName"] = "prod-worker-3"
	o["status"] = object{"conditions": readyConditions(), "containerStatuses": []any{object{"name": "fio", "imageID": strings.Split(fioImage, "@")[1]}}}
	return o
}

func TestCanaryCannotExecuteAgainstAReplacedOrUnreadyPod(t *testing.T) {
	if err := verifyCanaryPod(canaryPodFixture(), "probe", "prod-worker-3"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(object){
		"unready":          func(p object) { p["status"].(object)["conditions"] = []any{} },
		"replaced":         func(p object) { p["metadata"].(object)["uid"] = "replacement" },
		"wrong client":     func(p object) { p["spec"].(object)["nodeName"] = "other" },
		"changed script":   func(p object) { value(p, "spec", "containers").([]any)[0].(object)["args"] = []any{"other"} },
		"wrong executable": func(p object) { value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "unpinned" },
		"host users":       func(p object) { p["spec"].(object)["hostUsers"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := canaryPodFixture()
			change(p)
			if verifyCanaryPod(p, "probe", "prod-worker-3") == nil {
				t.Fatal("canary may execute against an unverified consumer")
			}
		})
	}
}

func TestCanaryBindingCannotAdoptAnUnrelatedClassOrClaim(t *testing.T) {
	for _, phase := range []string{"v1", "v2"} {
		group := "source"
		if phase == "v2" {
			group = "target"
		}
		class, claim := resource("class"), resource("fio-data")
		claim["metadata"].(object)["labels"] = object{"velero.io/restore-name": "longhorn-v2-canary-1234", "velero.io/backup-name": "longhorn-v2-canary-1234"}
		r := canaryRecord{Run: "1234", Resources: map[string]benchmarkResource{group + "/StorageClass": benchmarkIdentity(class), "source/PersistentVolumeClaim": benchmarkIdentity(claim)}}
		if err := verifyCanaryBindingIdentity(phase, r, class, claim); err != nil {
			t.Fatal(err)
		}
		class["metadata"].(object)["uid"] = "replacement"
		if verifyCanaryBindingIdentity(phase, r, class, claim) == nil {
			t.Fatal("foreign storage class adopted")
		}
		class["metadata"].(object)["uid"] = "uid-class"
		if phase == "v1" {
			claim["metadata"].(object)["uid"] = "replacement"
		} else {
			claim["metadata"].(object)["labels"] = object{}
		}
		if verifyCanaryBindingIdentity(phase, r, class, claim) == nil {
			t.Fatal("unrelated source or restored claim adopted")
		}
	}
}
