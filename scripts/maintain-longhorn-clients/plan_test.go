package main

import (
	"encoding/json"
	"testing"
	"time"
)

func clone(o object) object {
	b, _ := json.Marshal(o)
	var n object
	_ = json.Unmarshal(b, &n)
	return n
}
func resource(kind, ns, name string) object {
	return object{"kind": kind, "metadata": object{"name": name, "namespace": ns, "uid": ns + "-" + name, "resourceVersion": "1", "annotations": object{}}}
}
func fixture() inventory {
	s := inventory{clusters: []object{}, pods: []object{}, claims: []object{}, pvs: []object{}, volumes: []object{}, backups: []object{}}
	for _, target := range clusterTargets {
		c := resource("Cluster", target.ns, target.name)
		c["spec"] = object{"instances": target.instances, "storage": object{"storageClass": "longhorn-wffc"}}
		c["status"] = object{"readyInstances": target.instances, "phase": "Cluster in healthy state", "currentPrimary": target.name + "-1", "targetPrimary": target.name + "-1", "systemID": "123", "conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}}
		s.clusters = append(s.clusters, c)
		p := resource("Pod", target.ns, target.name+"-1")
		p["metadata"].(object)["ownerReferences"] = []any{object{"kind": "Cluster", "uid": str(c, "metadata", "uid"), "name": target.name, "controller": true}}
		p["spec"] = object{"volumes": []any{object{"persistentVolumeClaim": object{"claimName": target.name + "-1"}}}}
		p["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
		s.pods = append(s.pods, p)
		claim := resource("PersistentVolumeClaim", target.ns, target.name+"-1")
		claim["spec"] = object{"volumeName": "pv-" + target.name}
		claim["status"] = object{"phase": "Bound"}
		s.claims = append(s.claims, claim)
		pv := resource("PersistentVolume", "", "pv-"+target.name)
		pv["spec"] = object{"csi": object{"driver": "driver.longhorn.io", "volumeHandle": "volume-" + target.name}, "claimRef": object{"uid": str(claim, "metadata", "uid"), "namespace": target.ns, "name": target.name + "-1"}}
		pv["status"] = object{"phase": "Bound"}
		s.pvs = append(s.pvs, pv)
		volume := resource("Volume", "longhorn-system", "volume-"+target.name)
		volume["status"] = object{"state": "attached", "robustness": "healthy"}
		s.volumes = append(s.volumes, volume)
		if target.name != "backstage-db" {
			c["spec"].(object)["plugins"] = []any{object{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "enabled": true}}
			b := resource("Backup", target.ns, "backup-"+target.name)
			b["spec"] = object{"cluster": object{"name": target.name}, "method": "plugin"}
			b["status"] = object{"phase": "completed", "stoppedAt": "2026-10-10T03:30:00Z", "pluginMetadata": object{"clusterUID": str(c, "metadata", "uid")}}
			s.backups = append(s.backups, b)
		}
	}
	s.release = resource("HelmRelease", "crossview", "crossview")
	s.release["metadata"].(object)["labels"] = object{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
	s.release["spec"] = object{"suspend": false}
	s.deployment = resource("Deployment", "crossview", "crossview-postgres")
	s.deployment["metadata"].(object)["annotations"] = object{"meta.helm.sh/release-name": "crossview", "meta.helm.sh/release-namespace": "crossview"}
	s.deployment["spec"] = object{"replicas": 1}
	s.deployment["status"] = object{"readyReplicas": 1}
	for _, target := range extraTargets {
		r := resource("HelmRelease", target.ns, target.release)
		r["metadata"].(object)["labels"] = object{"kustomize.toolkit.fluxcd.io/name": target.layer, "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
		r["spec"] = object{"suspend": false}
		d := resource("Deployment", target.ns, target.deployment)
		d["metadata"].(object)["annotations"] = object{"meta.helm.sh/release-name": target.release, "meta.helm.sh/release-namespace": target.ns}
		d["spec"] = object{"replicas": 1}
		d["status"] = object{"readyReplicas": 1}
		s.extra = append(s.extra, workload{r, d})
	}
	return s
}

func TestPlanPreservesDatabaseAndStorageIdentity(t *testing.T) {
	s := fixture()
	p, err := buildPlan(s, time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.clusters) != 4 || len(p.claims) != 4 || len(p.volumes) != 4 {
		t.Fatal("incomplete retained inventory")
	}
	if str(p.clusters[3], "status", "currentPrimary") != "wedding-db-1" {
		t.Fatal("primary identity lost")
	}
}

func TestPlanFailsBeforeAnyWriteOnUnknownOrUnsafeScope(t *testing.T) {
	cases := map[string]func(*inventory){
		"missing cluster":    func(s *inventory) { s.clusters = s.clusters[:3] },
		"unexpected cluster": func(s *inventory) { s.clusters = append(s.clusters, resource("Cluster", "other", "database")) },
		"foreign pause": func(s *inventory) {
			s.clusters[0]["metadata"].(object)["annotations"] = object{"cnpg.io/hibernation": "on"}
		},
		"foreign reconcile disable": func(s *inventory) {
			s.clusters[0]["metadata"].(object)["annotations"] = object{"kustomize.toolkit.fluxcd.io/reconcile": "disabled"}
		},
		"primary switch": func(s *inventory) { s.clusters[0]["status"].(object)["targetPrimary"] = "different" },
		"missing archive": func(s *inventory) {
			s.clusters[1]["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}
		},
		"unready cluster": func(s *inventory) {
			s.clusters[0]["status"].(object)["conditions"] = []any{object{"type": "ContinuousArchiving", "status": "True"}}
		},
		"missing replica": func(s *inventory) { s.clusters[0]["status"].(object)["readyInstances"] = 1 },
		"unhealthy phase": func(s *inventory) {
			s.clusters[0]["status"].(object)["phase"] = "Waiting for the instances to become active"
		},
		"unready primary":  func(s *inventory) { s.pods[0]["status"].(object)["conditions"] = []any{} },
		"active backup":    func(s *inventory) { s.backups[0]["status"].(object)["phase"] = "running" },
		"wrong backup UID": func(s *inventory) { s.backups[0]["status"].(object)["pluginMetadata"] = object{"clusterUID": "wrong"} },
		"stale backup":     func(s *inventory) { s.backups[0]["status"].(object)["stoppedAt"] = "2026-10-08T03:00:00Z" },
		"faulted backing":  func(s *inventory) { s.volumes[0]["status"].(object)["robustness"] = "faulted" },
		"unknown backing":  func(s *inventory) { s.volumes[0]["status"].(object)["state"] = "mystery" },
		"foreign claim":    func(s *inventory) { s.pvs[0]["spec"].(object)["claimRef"].(object)["uid"] = "other" },
		"unknown consumer": func(s *inventory) {
			s.pods[0]["metadata"].(object)["ownerReferences"] = []any{object{"kind": "Job", "uid": "unexpected", "controller": true}}
		},
		"empty volume coverage": func(s *inventory) { s.volumes = nil },
		"duplicate backing identity": func(s *inventory) {
			s.pvs[1]["spec"].(object)["csi"].(object)["volumeHandle"] = str(s.pvs[0], "spec", "csi", "volumeHandle")
		},
		"missing additional consumer":   func(s *inventory) { s.extra = s.extra[:1] },
		"foreign additional Helm owner": func(s *inventory) { s.extra[0].Release["spec"].(object)["suspend"] = true },
		"foreign helm suspension":       func(s *inventory) { s.release["spec"].(object)["suspend"] = true },
		"unready bundled database":      func(s *inventory) { s.deployment["status"].(object)["readyReplicas"] = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixture()
			mutate(&s)
			if _, err := buildPlan(s, time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("accepted unsafe maintenance")
			}
		})
	}
}

func TestRestorationCannotAdoptForeignOrReplacedResources(t *testing.T) {
	before := resource("Cluster", "test", "db")
	before["spec"] = object{"instances": 3}
	before = clone(before)
	current := clone(before)
	current["metadata"].(object)["annotations"] = object{ownerKey: "run.1", hibernateKey: "on", reconcileKey: "disabled"}
	if _, err := restorePatch(before, current, "run.1", false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(object){
		func(o object) { o["metadata"].(object)["uid"] = "replacement" },
		func(o object) { o["metadata"].(object)["annotations"].(object)[ownerKey] = "foreign" },
		func(o object) { o["metadata"].(object)["annotations"].(object)[hibernateKey] = "off" },
		func(o object) { o["spec"].(object)["instances"] = 2 },
	} {
		o := clone(current)
		mutate(o)
		if _, err := restorePatch(before, o, "run.1", false); err == nil {
			t.Fatal("restoration adopted drift")
		}
	}
}

func TestReleasedRetainedBackingSurvivesClaimNameReuse(t *testing.T) {
	s := fixture()
	pv := clone(s.pvs[0])
	pv["metadata"].(object)["name"] = "retired-pv"
	pv["metadata"].(object)["uid"] = "retired-pv-uid"
	pv["spec"].(object)["claimRef"].(object)["uid"] = "retired-claim-uid"
	pv["spec"].(object)["persistentVolumeReclaimPolicy"] = "Retain"
	pv["spec"].(object)["csi"].(object)["volumeHandle"] = "retired-backing"
	pv["status"] = object{"phase": "Released"}
	v := clone(s.volumes[0])
	v["metadata"].(object)["name"] = "retired-backing"
	v["metadata"].(object)["uid"] = "retired-backing-uid"
	v["status"].(object)["state"] = "detached"
	s.pvs = append(s.pvs, pv)
	s.volumes = append(s.volumes, v)
	if _, err := buildPlan(s, time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(){func() { pv["status"].(object)["phase"] = "Bound" }, func() { pv["spec"].(object)["persistentVolumeReclaimPolicy"] = "Delete" }, func() { v["status"].(object)["state"] = "attached" }} {
		mutate()
		if _, err := buildPlan(s, time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)); err == nil {
			t.Fatal("retired backing protection weakened")
		}
		pv["status"].(object)["phase"] = "Released"
		pv["spec"].(object)["persistentVolumeReclaimPolicy"] = "Retain"
		v["status"].(object)["state"] = "detached"
	}
}
