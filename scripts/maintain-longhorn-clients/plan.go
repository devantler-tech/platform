package main

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"
)

type object = map[string]any

const ownerKey = "platform.devantler.tech/longhorn-client-maintenance"
const hibernateKey = "cnpg.io/hibernation"
const reconcileKey = "kustomize.toolkit.fluxcd.io/reconcile"

var clusterTargets = []struct {
	ns, name  string
	instances int
}{
	{"backstage", "backstage-db", 2}, {"observability", "coroot-db", 3},
	{"umami", "umami-db", 3}, {"wedding-app", "wedding-db", 3},
}

type inventory struct {
	clusters, pods, claims, pvs, volumes, backups, replicasets []object
	schedules                                                  []object
	release, deployment                                        object
	backupRelease                                              object
	extra                                                      []workload
}
type workload struct{ Release, Deployment object }

var extraTargets = []struct{ ns, release, deployment, layer string }{{"actual-budget", "actual-budget", "actual-budget-actualbudget", "apps"}, {"kubescape", "kubescape", "storage", "infrastructure-controllers"}}

type plan struct {
	clusters, claims, pvs, volumes []object
	schedules                      []object
	release, deployment            object
	backupRelease                  object
	extra                          []workload
}

func val(o object, keys ...string) any {
	var v any = o
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
func str(o object, keys ...string) string { s, _ := val(o, keys...).(string); return s }
func num(o object, keys ...string) int {
	switch n := val(o, keys...).(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}
func items(o object, keys ...string) []object {
	a, _ := val(o, keys...).([]any)
	r := []object{}
	for _, v := range a {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		r = append(r, m)
	}
	return r
}
func key(o object) string { return str(o, "metadata", "namespace") + "/" + str(o, "metadata", "name") }
func meta(o object) bool {
	return str(o, "metadata", "name") != "" && str(o, "metadata", "uid") != "" && str(o, "metadata", "resourceVersion") != "" && val(o, "metadata", "deletionTimestamp") == nil
}
func condition(o object, kind string) bool {
	for _, c := range items(o, "status", "conditions") {
		if str(c, "type") == kind {
			return str(c, "status") == "True"
		}
	}
	return false
}
func index(list []object) (map[string]object, error) {
	m := map[string]object{}
	for _, o := range list {
		if !meta(o) || m[key(o)] != nil {
			return nil, errors.New("incomplete or duplicate resource identity")
		}
		m[key(o)] = o
	}
	return m, nil
}
func owner(o object, kind, uid string) bool {
	refs := items(o, "metadata", "ownerReferences")
	return len(refs) == 1 && str(refs[0], "kind") == kind && str(refs[0], "uid") == uid && val(refs[0], "controller") == true
}
func unpaused(o object) bool {
	return str(o, "metadata", "annotations", ownerKey) == "" && str(o, "metadata", "annotations", hibernateKey) != "on" && str(o, "metadata", "annotations", "cnpg.io/reconciliationLoop") == "" && str(o, "metadata", "annotations", reconcileKey) == ""
}

func archiveHealthy(c object) bool {
	if val(c, "spec", "backup", "barmanObjectStore") != nil {
		return condition(c, "ContinuousArchiving")
	}
	for _, plugin := range items(c, "spec", "plugins") {
		if val(plugin, "isWALArchiver") == true && val(plugin, "enabled") != false {
			return condition(c, "ContinuousArchiving")
		}
	}
	return true // No WAL archiver is configured; this is not backup evidence.
}

func scheduleInScope(o object, clusters []object) bool {
	if meta(o) && str(o, "kind") == "Schedule" {
		return str(o, "apiVersion") == "velero.io/v1" && key(o) == "velero/velero-daily-full"
	}
	if !meta(o) || str(o, "kind") != "ScheduledBackup" {
		return false
	}
	for _, c := range clusters {
		if str(o, "metadata", "namespace") == str(c, "metadata", "namespace") && str(o, "spec", "cluster", "name") == str(c, "metadata", "name") {
			return true
		}
	}
	return false
}

func suspensionField(o object) string {
	if str(o, "kind") == "Schedule" {
		return "paused"
	}
	return "suspend"
}

func buildPlan(s inventory, now time.Time) (plan, error) {
	p := plan{clusters: s.clusters, schedules: s.schedules, release: s.release, deployment: s.deployment, backupRelease: s.backupRelease, extra: s.extra}
	clusters, err := index(s.clusters)
	if err != nil {
		return p, err
	}
	if len(clusters) != len(clusterTargets) || len(s.volumes) == 0 {
		return p, errors.New("maintenance scope or volume coverage is unknown")
	}
	for _, t := range clusterTargets {
		c := clusters[t.ns+"/"+t.name]
		if c == nil || !unpaused(c) || num(c, "spec", "instances") != t.instances || num(c, "status", "readyInstances") != t.instances || !condition(c, "Ready") || str(c, "status", "phase") != "Cluster in healthy state" || str(c, "status", "systemID") == "" || str(c, "status", "currentPrimary") == "" || str(c, "status", "currentPrimary") != str(c, "status", "targetPrimary") || !archiveHealthy(c) {
			return p, errors.New("database primary, archiving or maintenance ownership is unproven")
		}
		ready := false
		for _, pod := range s.pods {
			if str(pod, "metadata", "namespace") == t.ns && str(pod, "metadata", "name") == str(c, "status", "currentPrimary") && meta(pod) && owner(pod, "Cluster", str(c, "metadata", "uid")) && condition(pod, "Ready") {
				ready = true
			}
		}
		if !ready {
			return p, errors.New("database primary is not ready")
		}
		// Backstage currently has no backup plugin. Require fresh identity-bound
		// successful backups for every cluster that does declare one; never invent
		// a backup or count another cluster incarnation's catalogue as protection.
		if len(items(c, "spec", "plugins")) > 0 {
			if !condition(c, "LastBackupSucceeded") {
				return p, errors.New("latest database backup success is unproven")
			}
			fresh := false
			for _, b := range s.backups {
				if str(b, "metadata", "namespace") != t.ns || str(b, "spec", "cluster", "name") != t.name {
					continue
				}
				phase := str(b, "status", "phase")
				if phase != "completed" && phase != "failed" {
					return p, errors.New("database backup is active or unknown")
				}
				when, e := time.Parse(time.RFC3339, str(b, "status", "stoppedAt"))
				if meta(b) && phase == "completed" && str(b, "spec", "method") == "plugin" && str(b, "status", "pluginMetadata", "clusterUID") == str(c, "metadata", "uid") && e == nil && !when.After(now) && now.Sub(when) <= 24*time.Hour {
					fresh = true
				}
			}
			if !fresh {
				return p, errors.New("fresh database backup is unproven")
			}
		}
	}
	if _, err := index(s.schedules); err != nil {
		return p, err
	}
	for _, schedule := range s.schedules {
		if !scheduleInScope(schedule, s.clusters) || !unpaused(schedule) || val(schedule, "spec", suspensionField(schedule)) == true {
			return p, errors.New("scheduled backup scope or pause ownership is unproven")
		}
	}
	if !meta(s.backupRelease) || str(s.backupRelease, "kind") != "HelmRelease" || key(s.backupRelease) != "velero/velero" || !unpaused(s.backupRelease) || val(s.backupRelease, "spec", "suspend") == true || str(s.backupRelease, "metadata", "labels", "kustomize.toolkit.fluxcd.io/name") != "infrastructure-controllers" || str(s.backupRelease, "metadata", "labels", "kustomize.toolkit.fluxcd.io/namespace") != "flux-system" {
		return p, errors.New("backup schedule reconciler ownership is unproven")
	}
	if !meta(s.release) || key(s.release) != "crossview/crossview" || !unpaused(s.release) || val(s.release, "spec", "suspend") == true || str(s.release, "metadata", "labels", "kustomize.toolkit.fluxcd.io/name") != "apps" || str(s.release, "metadata", "labels", "kustomize.toolkit.fluxcd.io/namespace") != "flux-system" || !meta(s.deployment) || key(s.deployment) != "crossview/crossview-postgres" || !unpaused(s.deployment) || num(s.deployment, "spec", "replicas") != 1 || num(s.deployment, "status", "readyReplicas") != 1 || str(s.deployment, "metadata", "annotations", "meta.helm.sh/release-name") != "crossview" || str(s.deployment, "metadata", "annotations", "meta.helm.sh/release-namespace") != "crossview" {
		return p, errors.New("bundled database ownership or readiness is unproven")
	}
	if len(s.extra) != len(extraTargets) {
		return p, errors.New("additional storage-consumer coverage is incomplete")
	}
	for i, w := range s.extra {
		t := extraTargets[i]
		if !meta(w.Release) || key(w.Release) != t.ns+"/"+t.release || !unpaused(w.Release) || val(w.Release, "spec", "suspend") == true || str(w.Release, "metadata", "labels", "kustomize.toolkit.fluxcd.io/name") != t.layer || str(w.Release, "metadata", "labels", "kustomize.toolkit.fluxcd.io/namespace") != "flux-system" || !meta(w.Deployment) || key(w.Deployment) != t.ns+"/"+t.deployment || !unpaused(w.Deployment) || num(w.Deployment, "spec", "replicas") != 1 || str(w.Deployment, "metadata", "annotations", "meta.helm.sh/release-name") != t.release || str(w.Deployment, "metadata", "annotations", "meta.helm.sh/release-namespace") != t.ns {
			return p, errors.New("additional storage-consumer ownership is unproven")
		}
	}
	claims, err := index(s.claims)
	if err != nil {
		return p, err
	}
	pvs, err := index(s.pvs)
	if err != nil {
		return p, err
	}
	volumes, err := index(s.volumes)
	if err != nil {
		return p, err
	}
	allowedClaims := map[string]bool{}
	seenBackings := map[string]bool{}
	for _, pv := range pvs {
		if str(pv, "spec", "csi", "driver") != "driver.longhorn.io" {
			continue
		}
		v := volumes["longhorn-system/"+str(pv, "spec", "csi", "volumeHandle")]
		if v != nil && seenBackings[key(v)] {
			return p, errors.New("a backing volume has multiple persistent-volume identities")
		}
		if v != nil {
			seenBackings[key(v)] = true
		}
		if v == nil || str(v, "status", "robustness") == "faulted" || (str(v, "status", "state") != "attached" && str(v, "status", "state") != "detached") || (str(v, "status", "state") == "attached" && str(v, "status", "robustness") != "healthy") {
			return p, errors.New("backing volume health is unproven")
		}
		p.pvs = append(p.pvs, pv)
		p.volumes = append(p.volumes, v)
		claimKey := str(pv, "spec", "claimRef", "namespace") + "/" + str(pv, "spec", "claimRef", "name")
		claim := claims[claimKey]
		if claim == nil || str(claim, "metadata", "uid") != str(pv, "spec", "claimRef", "uid") {
			// A retained PV can refer to a retired claim whose NAME has already
			// been reused by the successful fresh-storage recovery. Only the old
			// UID, Retain+Released state and detached backing establish that case.
			if str(pv, "status", "phase") != "Released" || str(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" || str(v, "status", "state") != "detached" {
				return p, errors.New("retained volume is not released and detached")
			}
			continue
		}
		if str(claim, "metadata", "uid") != str(pv, "spec", "claimRef", "uid") || str(claim, "spec", "volumeName") != str(pv, "metadata", "name") || str(claim, "status", "phase") != "Bound" {
			return p, errors.New("claim binding changed")
		}
		p.claims = append(p.claims, claim)
		allowedClaims[claimKey] = true
	}
	if len(p.volumes) != len(volumes) {
		return p, errors.New("a backing volume has no unique persistent-volume identity")
	}
	for _, pod := range s.pods {
		if str(pod, "status", "phase") == "Succeeded" || str(pod, "status", "phase") == "Failed" {
			continue
		}
		for _, volume := range items(pod, "spec", "volumes") {
			claimName := str(volume, "persistentVolumeClaim", "claimName")
			if claimName == "" || !allowedClaims[str(pod, "metadata", "namespace")+"/"+claimName] {
				continue
			}
			allowed := false
			for _, c := range p.clusters {
				if str(pod, "metadata", "namespace") == str(c, "metadata", "namespace") && owner(pod, "Cluster", str(c, "metadata", "uid")) {
					allowed = true
				}
			}
			deployments := []object{s.deployment}
			for _, w := range s.extra {
				deployments = append(deployments, w.Deployment)
			}
			for _, rs := range s.replicasets {
				for _, d := range deployments {
					if key(rs) != "" && str(rs, "metadata", "namespace") == str(d, "metadata", "namespace") && owner(rs, "Deployment", str(d, "metadata", "uid")) && owner(pod, "ReplicaSet", str(rs, "metadata", "uid")) {
						allowed = true
					}
				}
			}
			if !allowed {
				return p, errors.New("unreviewed persistent-volume consumer exists")
			}
		}
	}
	return p, nil
}

func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
func tests(o object) []object {
	// Kubernetes enforces the supplied resourceVersion on update. Replacing it
	// gives a typed 409 Conflict on status races, rather than JSON-test 422s.
	return []object{{"op": "test", "path": "/metadata/uid", "value": val(o, "metadata", "uid")}, {"op": "replace", "path": "/metadata/resourceVersion", "value": val(o, "metadata", "resourceVersion")}}
}
func restorePatch(before, current object, holder string, deployment bool) ([]object, error) {
	if !meta(current) || key(before) != key(current) || str(before, "metadata", "uid") != str(current, "metadata", "uid") || str(current, "metadata", "annotations", ownerKey) != holder {
		return nil, errors.New("restoration ownership changed")
	}
	expected := val(before, "spec")
	if deployment {
		copied := object{}
		spec, ok := expected.(map[string]any)
		if !ok {
			return nil, errors.New("original deployment spec is malformed")
		}
		for k, v := range spec {
			copied[k] = v
		}
		copied["replicas"] = float64(0)
		expected = copied
	}
	if str(before, "kind") == "HelmRelease" || str(before, "kind") == "ScheduledBackup" || str(before, "kind") == "Schedule" {
		copied := object{}
		spec, ok := expected.(map[string]any)
		if !ok {
			return nil, errors.New("original Helm owner spec is malformed")
		}
		for k, v := range spec {
			copied[k] = v
		}
		copied[suspensionField(before)] = true
		expected = copied
	}
	if !reflect.DeepEqual(expected, val(current, "spec")) {
		return nil, errors.New("restoration refuses intervening spec changes")
	}
	patch := tests(current)
	for _, k := range []string{hibernateKey, reconcileKey} {
		want := "disabled"
		if k == hibernateKey {
			if str(before, "kind") != "Cluster" {
				continue
			}
			want = "on"
		}
		if str(current, "metadata", "annotations", k) != want {
			return nil, errors.New("owned pause was changed")
		}
		path := "/metadata/annotations/" + escape(k)
		old := val(before, "metadata", "annotations", k)
		if old == nil {
			patch = append(patch, object{"op": "remove", "path": path})
		} else {
			patch = append(patch, object{"op": "replace", "path": path, "value": old})
		}
	}
	if deployment {
		patch = append(patch, object{"op": "replace", "path": "/spec/replicas", "value": val(before, "spec", "replicas")})
	}
	if str(before, "kind") == "HelmRelease" || str(before, "kind") == "ScheduledBackup" || str(before, "kind") == "Schedule" {
		field := suspensionField(before)
		old := val(before, "spec", field)
		if old == nil {
			patch = append(patch, object{"op": "remove", "path": "/spec/" + field})
		} else {
			patch = append(patch, object{"op": "replace", "path": "/spec/" + field, "value": old})
		}
	}
	patch = append(patch, object{"op": "remove", "path": "/metadata/annotations/" + escape(ownerKey)})
	return patch, nil
}

func preserved(before, current []object) error {
	m, e := index(current)
	if e != nil {
		return e
	}
	for _, old := range before {
		o := m[key(old)]
		if o == nil || str(o, "metadata", "uid") != str(old, "metadata", "uid") || !reflect.DeepEqual(val(o, "spec"), val(old, "spec")) {
			return fmt.Errorf("retained %s identity or spec changed", str(old, "kind"))
		}
	}
	return nil
}

// A restored operation does not own later storage expansions. Compare every
// other claim/PV spec field, including the full claim and physical CSI binding.
func storageBindingsPreserved(before, current []object) error {
	m, err := index(current)
	if err != nil {
		return err
	}
	for _, old := range before {
		o := m[key(old)]
		if o == nil || str(o, "kind") != str(old, "kind") || str(o, "metadata", "uid") != str(old, "metadata", "uid") {
			return errors.New("retained storage identity changed")
		}
		var path []string
		switch str(old, "kind") {
		case "PersistentVolumeClaim":
			path = []string{"resources", "requests", "storage"}
		case "PersistentVolume":
			path = []string{"capacity", "storage"}
		default:
			return errors.New("unknown retained storage kind")
		}
		if !reflect.DeepEqual(withoutField(val(old, "spec"), path), withoutField(val(o, "spec"), path)) {
			return errors.New("retained storage binding or non-capacity specification changed")
		}
	}
	return nil
}

// Copy just the projected path: comparison must not mutate the live inventory
// or the journal snapshot that active recovery still checks strictly.
func withoutField(value any, path []string) any {
	m, ok := value.(map[string]any)
	if !ok || len(path) == 0 {
		return value
	}
	copy := maps.Clone(m)
	if len(path) == 1 {
		delete(copy, path[0])
	} else if child, exists := copy[path[0]]; exists {
		copy[path[0]] = withoutField(child, path[1:])
	}
	return copy
}

// Attachment targets are controller-owned and must change when consumers move.
// Retain physical backing identity and data format, not the old worker placement.
func backingPreserved(before, current []object) error {
	return backingFieldsPreserved(before, current, []string{"size", "dataEngine", "frontend", "fromBackup", "backingImage"})
}

func backingFieldsPreserved(before, current []object, fields []string) error {
	m, e := index(current)
	if e != nil {
		return e
	}
	if len(m) != len(before) {
		return errors.New("backing volume inventory changed")
	}
	for _, old := range before {
		o := m[key(old)]
		if o == nil || str(o, "metadata", "uid") != str(old, "metadata", "uid") {
			return errors.New("backing volume identity changed")
		}
		for _, field := range fields {
			if !reflect.DeepEqual(val(o, "spec", field), val(old, "spec", field)) {
				return errors.New("backing volume data format changed")
			}
		}
	}
	return nil
}
