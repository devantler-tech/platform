package main

import (
	"testing"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func TestNativeRestoreRefusesHardwareReplacementBehindTheOriginalKubeIdentity(t *testing.T) {
	for _, fault := range []string{"provider", "machine", "storage", "disk"} {
		t.Run(fault, func(t *testing.T) {
			targets, nodes, storage := nativeRestoreTargetsFixture()
			switch fault {
			case "provider":
				valueObject(nodes[1], "spec")["providerID"] = "hcloud://999"
			case "machine":
				valueObject(nodes[1], "status", "nodeInfo")["systemUUID"] = "replacement-machine"
			case "storage":
				valueObject(storage[1], "metadata")["uid"] = "replacement-storage"
			case "disk":
				valueObject(storage[1], "status", "diskStatus", "longhorn-v2")["diskUUID"] = "replacement-disk"
			}
			if got, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture()); err == nil || got != nil {
				t.Fatal("hardware replacement accepted behind an unchanged Kubernetes UID")
			}
		})
	}
}

func TestNativeRestoreRequiresTheFixedTemporaryRosterAndLiveProtection(t *testing.T) {
	for _, fault := range []string{"worker 3", "permanent worker", "missing pin", "foreign owner", "missing enable", "wrong source", "foreign activation", "changed activation fingerprint"} {
		t.Run(fault, func(t *testing.T) {
			targets, nodes, storage := nativeRestoreTargetsFixture()
			config := poolConfigFixture()
			switch fault {
			case "worker 3":
				targets[2].Name = "prod-worker-3"
			case "permanent worker":
				targets[0].Name = "prod-worker-1"
			case "missing pin":
				delete(valueObject(nodes[0], "metadata", "annotations"), longhornmigration.ScaleDownDisabled)
			case "foreign owner":
				valueObject(nodes[0], "metadata", "annotations")[longhornmigration.OwnerAnnotation] = "foreign"
			case "missing enable":
				delete(valueObject(storage[0], "metadata", "labels"), longhornmigration.EnabledLabel)
			case "wrong source":
				config = []byte("spec: {}")
			case "foreign activation":
				valueObject(storage[0], "metadata", "annotations")[temporaryActivationOwner] = "foreign"
			case "changed activation fingerprint":
				targets[1].Preparation.ActivationSHA256 = "changed"
			}
			if got, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, config); err == nil || got != nil {
				t.Fatal("unqualified mixed-roster target accepted")
			}
		})
	}
}
