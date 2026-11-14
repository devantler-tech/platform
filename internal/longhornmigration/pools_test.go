package longhornmigration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func sourceFixture() []byte {
	return []byte(`{"spec":{"cluster":{"controlPlanes":3,"workers":3,"autoscaler":{"node":{"maxNodesTotal":9,"pools":[{"name":"autoscale-longhorn-v1","serverType":"cpx42","location":"fsn1","min":2,"max":2,"labels":{"storage.platform.devantler.tech/migration-pool":"v1"}},{"name":"autoscale-longhorn-v2","serverType":"cpx42","location":"fsn1","min":1,"max":1,"labels":{"storage.platform.devantler.tech/migration-pool":"v2"}}]}}},"provider":{"hetzner":{"serverLimit":10}}}}`)
}

func TestStagedRawDiskRequiresItsCompletedRegistration(t *testing.T) {
	n, lh := eligibilityFixture("v2")
	value(lh, "spec", "disks", "longhorn-v2").(Object)["path"] = "/dev/mapper/luks2-" + RetainedStageRawID
	lh["metadata"].(Object)["annotations"] = Object{ActivationOwner: "owned-registration"}
	record := ActivationRecord{Version: 1, Source: strings.Repeat("a", 40), OwnerRun: "12", Completed: true, Targets: map[string]ActivationBinding{}}
	for i, selection := range ActivationSelections() {
		role := strings.Split(selection, "-")[1]
		record.Targets[selection] = ActivationBinding{
			Selection: selection, Name: fmt.Sprintf("autoscale-longhorn-%s-%016x", role, i),
			NodeUID: fmt.Sprintf("node-%d", i), ProviderID: fmt.Sprintf("hcloud://%d", i+1),
			MachineUUID: fmt.Sprintf("machine-%d", i), BootID: "12345678-1234-4234-8234-000000000099", OSImage: "Talos fixture",
			LonghornUID: fmt.Sprintf("longhorn-%d", i), EphemeralUUID: fmt.Sprintf("ephemeral-%d", i),
			RawPartitionUUID: fmt.Sprintf("12345678-1234-4234-8234-%012x", i), PreparationUID: fmt.Sprintf("prep-%d", i),
		}
	}
	stage := record.Targets["temporary-v2-1"]
	stage.Name, stage.NodeUID, stage.ProviderID = text(n, "metadata", "name"), text(n, "metadata", "uid"), text(n, "spec", "providerID")
	stage.MachineUUID, stage.LonghornUID = text(n, "status", "nodeInfo", "systemUUID"), text(lh, "metadata", "uid")
	record.Targets[stage.Selection] = stage
	cm := Object{"metadata": Object{"name": ActivationJournal, "namespace": "longhorn-system", "uid": "owned-registration"}}
	for _, complete := range []bool{true, false} {
		record.Completed = complete
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		cm["data"] = Object{"record": string(encoded)}
		ok, err := Eligible(sourceFixture(), n, lh, cm)
		if ok != complete || (err == nil) != complete {
			t.Fatalf("staged raw eligibility completed=%t got %t: %v", complete, ok, err)
		}
	}
}

// eligibilityFixture binds one fixed pool member to owned scale-down protection
// and its role-specific storage, independently of the activation writer.
func eligibilityFixture(role string) (Object, Object) {
	name := "autoscale-longhorn-" + role + "-0123456789abcdef"
	n := Object{"metadata": Object{"name": name, "uid": "kube-uid", "resourceVersion": "1", "labels": Object{PoolLabel: role, "ksail.io/autoscaled": "true", "node.kubernetes.io/instance-type": "cpx42"}, "annotations": Object{ScaleDownDisabled: "true", OwnerAnnotation: OwnerValue}}, "spec": Object{"providerID": "hcloud://42"}, "status": Object{"nodeInfo": Object{"systemUUID": "machine"}, "conditions": []any{Object{"type": "Ready", "status": "True"}}}}
	diskName := "migration-v1"
	disk := Object{"diskType": "filesystem", "path": "/var/lib/longhorn", "allowScheduling": true, "evictionRequested": false}
	if role == "v2" {
		diskName = "longhorn-v2"
		disk["diskType"] = "block"
		disk["path"] = "/dev/mapper/luks2-r-longhorn"
		disk["diskDriver"] = "aio"
	}
	lh := Object{"metadata": Object{"name": name, "uid": "longhorn-uid", "labels": Object{EnabledLabel: "true"}}, "spec": Object{"allowScheduling": true, "evictionRequested": false, "disks": Object{diskName: disk}}}
	return n, lh
}

// Only fixed reviewed pool members with intact ownership and storage can escape
// ordinary compute-node exclusion; missing or altered authorization holds.
func TestOnlyReviewedPinnedMigrationPoolsAreStorageExceptions(t *testing.T) {
	for _, role := range []string{"v1", "v2"} {
		n, lh := eligibilityFixture(role)
		if ok, err := Eligible(sourceFixture(), n, lh, nil); !ok || err != nil {
			t.Fatalf("verified %s pool rejected: %v", role, err)
		}
	}
	for name, mutate := range map[string]func(Object, Object){
		"ordinary compute name": func(n, _ Object) { n["metadata"].(Object)["name"] = "autoscale-cx43-0123456789abcdef" },
		"near-miss pool name":   func(n, _ Object) { n["metadata"].(Object)["name"] = "autoscale-longhorn-v10-0123456789abcdef" },
		"wrong machine type": func(n, _ Object) {
			n["metadata"].(Object)["labels"].(Object)["node.kubernetes.io/instance-type"] = "cax21"
		},
		"missing protection": func(n, _ Object) { delete(n["metadata"].(Object)["annotations"].(Object), ScaleDownDisabled) },
		"foreign owner": func(n, _ Object) {
			n["metadata"].(Object)["annotations"].(Object)[OwnerAnnotation] = "another-migration"
		},
		"unready worker":        func(n, _ Object) { n["status"].(Object)["conditions"] = []any{} },
		"foreign Longhorn node": func(_, lh Object) { lh["metadata"].(Object)["name"] = "other-worker" },
		"evacuating storage":    func(_, lh Object) { lh["spec"].(Object)["evictionRequested"] = true },
		"malformed authorization": func(_, lh Object) {
			lh["metadata"].(Object)["labels"].(Object)[EnabledLabel] = "yes"
		},
		"foreign disk": func(_, lh Object) {
			lh["spec"].(Object)["disks"] = Object{"other": Object{"diskType": "filesystem", "path": "/other"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, lh := eligibilityFixture("v1")
			mutate(n, lh)
			if ok, err := Eligible(sourceFixture(), n, lh, nil); ok || err == nil {
				t.Fatal("unverified storage exception accepted")
			}
		})
	}
}

// Fixed pool capacity cannot be reduced while its storage exception is active.
func TestFixedPoolMinimumCannotBeRemovedWhileStorageIsAuthorized(t *testing.T) {
	n, lh := eligibilityFixture("v1")
	var cfg Object
	if err := json.Unmarshal(sourceFixture(), &cfg); err != nil {
		t.Fatal(err)
	}
	pools := value(cfg, "spec", "cluster", "autoscaler", "node", "pools").([]any)
	pools[0].(Object)["min"] = float64(0)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Eligible(b, n, lh, nil); ok || err == nil {
		t.Fatal("a zero-minimum pool retained its storage exception")
	}
	delete(lh["metadata"].(Object)["labels"].(Object), EnabledLabel)
	if ok, err := Eligible(nil, n, lh, nil); ok || err != nil {
		t.Fatal("ordinary compute classification depends on temporary migration source")
	}
}

func TestConfiguredPoolsRefuseChangedHardwareOrLocation(t *testing.T) {
	for _, change := range [][2]string{{`"serverType":"cpx42"`, `"serverType":"cpx52"`}, {`"location":"fsn1"`, `"location":"nbg1"`}} {
		for _, name := range []string{"autoscale-longhorn-v1", "autoscale-longhorn-v2"} {
			prefix := `"name":"` + name + `",`
			source := strings.Replace(string(sourceFixture()), prefix+`"serverType":"cpx42","location":"fsn1"`, prefix+strings.Replace(`"serverType":"cpx42","location":"fsn1"`, change[0], change[1], 1), 1)
			if source == string(sourceFixture()) {
				t.Fatalf("fixture for %s did not change", name)
			}
			if _, err := ConfiguredPools([]byte(source)); err == nil {
				t.Fatalf("%s accepted %s", name, change[1])
			}
		}
	}
}
