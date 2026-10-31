package longhornmigration

import (
	"encoding/json"
	"testing"
)

func sourceFixture() []byte {
	return []byte(`{"spec":{"cluster":{"controlPlanes":3,"workers":3,"autoscaler":{"node":{"maxNodesTotal":9,"pools":[{"name":"autoscale-longhorn-v1","serverType":"cpx42","location":"fsn1","min":2,"max":2,"labels":{"storage.platform.devantler.tech/migration-pool":"v1"}},{"name":"autoscale-longhorn-v2","serverType":"cpx42","location":"fsn1","min":1,"max":1,"labels":{"storage.platform.devantler.tech/migration-pool":"v2"}}]}}},"provider":{"hetzner":{"serverLimit":10}}}}`)
}

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

func TestOnlyReviewedPinnedMigrationPoolsAreStorageExceptions(t *testing.T) {
	for _, role := range []string{"v1", "v2"} {
		n, lh := eligibilityFixture(role)
		if ok, err := Eligible(sourceFixture(), n, lh); !ok || err != nil {
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
		"foreign disk": func(_, lh Object) {
			lh["spec"].(Object)["disks"] = Object{"other": Object{"diskType": "filesystem", "path": "/other"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, lh := eligibilityFixture("v1")
			mutate(n, lh)
			if ok, err := Eligible(sourceFixture(), n, lh); ok || err == nil {
				t.Fatal("unverified storage exception accepted")
			}
		})
	}
}

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
	if ok, err := Eligible(b, n, lh); ok || err == nil {
		t.Fatal("a zero-minimum pool retained its storage exception")
	}
	delete(lh["metadata"].(Object)["labels"].(Object), EnabledLabel)
	if ok, err := Eligible(nil, n, lh); ok || err != nil {
		t.Fatal("ordinary compute classification depends on temporary migration source")
	}
}
