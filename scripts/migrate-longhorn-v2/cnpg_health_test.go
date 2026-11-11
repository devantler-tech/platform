package main

import (
	"fmt"
	"testing"
)

// cnpgFixture supplies three native-shaped instances and their original claims.
func cnpgFixture(classes ...string) (object, []object, []object, []object) {
	cluster := resource("umami-db")
	cluster["metadata"].(object)["namespace"] = "umami"
	cluster["spec"] = object{"instances": float64(3), "imageName": "ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie", "storage": object{"storageClass": "longhorn-v2"}}
	cluster["status"] = object{"instances": float64(3), "readyInstances": float64(3), "currentPrimary": "umami-db-1", "targetPrimary": "umami-db-1", "conditions": append(readyConditions(), object{"type": "ContinuousArchiving", "status": "True"}), "instancesStatus": object{"healthy": []any{"umami-db-1", "umami-db-2", "umami-db-3"}}}
	owned := func(o object) {
		o["metadata"].(object)["namespace"] = "umami"
		o["metadata"].(object)["labels"] = object{"cnpg.io/cluster": "umami-db"}
		o["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "umami-db", "uid": text(cluster, "metadata", "uid"), "controller": true}}
	}
	pods, claims, pvs := []object{}, []object{}, []object{}
	for i, class := range classes {
		name := fmt.Sprintf("umami-db-%d", i+1)
		pod, claim, pv := resource(name), resource(name), resource("pv-"+name)
		owned(pod)
		owned(claim)
		role := "replica"
		if i == 0 {
			role = "primary"
		}
		pod["metadata"].(object)["labels"].(object)["cnpg.io/instanceRole"] = role
		pod["spec"] = object{"volumes": []any{object{"name": "pgdata", "persistentVolumeClaim": object{"claimName": name}}}, "containers": []any{object{"name": "postgres", "image": text(cluster, "spec", "imageName")}}}
		pod["status"] = object{"phase": "Running", "conditions": readyConditions()}
		claim["spec"] = object{"storageClassName": class, "volumeName": text(pv, "metadata", "name"), "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem"}
		claim["status"] = object{"phase": "Bound"}
		pv["spec"] = object{"persistentVolumeReclaimPolicy": "Retain", "claimRef": object{"namespace": "umami", "name": name, "uid": text(claim, "metadata", "uid")}, "storageClassName": class, "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "backend-" + name}}
		pv["status"] = object{"phase": "Bound"}
		pods = append(pods, pod)
		claims = append(claims, claim)
		pvs = append(pvs, pv)
	}
	return cluster, pods, claims, pvs
}

// TestHealthyCNPGPodsBindsSourceInstancesAcrossTheCompleteInventory covers newer instance serials.
func TestHealthyCNPGPodsBindsSourceInstancesAcrossTheCompleteInventory(t *testing.T) {
	c, pods, _, _ := cnpgFixture("longhorn-wffc", "longhorn-wffc", "longhorn-wffc")
	c["spec"].(object)["storage"].(object)["storageClass"] = "longhorn-wffc"
	for i, id := range []int{9, 10, 11} {
		pods[i]["metadata"].(object)["name"] = fmt.Sprintf("umami-db-%d", id)
	}
	c["status"].(object)["currentPrimary"], c["status"].(object)["targetPrimary"] = "umami-db-9", "umami-db-9"
	c["status"].(object)["instancesStatus"] = object{"healthy": []any{"umami-db-11", "umami-db-9", "umami-db-10"}}
	foreign := resource("unrelated")
	foreign["metadata"].(object)["namespace"] = "another-namespace"
	foreign["metadata"].(object)["labels"] = object{"cnpg.io/cluster": "umami-db"}
	got, err := healthyCNPGPods(c, []object{pods[2], foreign, pods[0], pods[1]})
	if err != nil || len(got) != 3 || text(got[0], "metadata", "name") != "umami-db-9" || text(got[1], "metadata", "name") != "umami-db-10" || text(got[2], "metadata", "name") != "umami-db-11" {
		t.Fatalf("source peer inventory is not stable and canonical: %v", err)
	}
}
