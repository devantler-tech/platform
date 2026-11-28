package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// A native storage observation is one input to restore verification. It grants
// neither restore acceptance nor cleanup authority and is not a journal phase.
type cnpgRecoveredStorageObservation struct {
	binding                                            cnpgRecoveredStorage
	namespace, cluster, pod, claim, pv, volume, engine object
	replicas                                           []object
}

func (p *cnpgProofRun) observeRecoveredStorage(ctx context.Context) (result cnpgRecoveredStorageObservation, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = cnpgRecoveredStorageObservation{}
			err = fmt.Errorf("recovered database storage observation held: phase=%s", phase)
		}
	}()
	r := p.record
	if p.c.context != "admin@prod" || r.Version != 1 || r.Phase != "cluster-created" || r.Plan == nil || r.Admission == nil || r.Storage == nil || r.Preparation == nil || r.Credential == nil || len(r.Resources) != 5 || r.RestoreVerified || r.Cleaned || !knownCNPGArchiveSource(r.Plan.Source.Namespace, r.Plan.Source.Cluster) || !commandContextActive(ctx) {
		return result, errors.New("recovery observation lacks its completed owned creation checkpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	phase = "owner-before"
	if _, err = p.currentExecution(ctx); err != nil {
		return result, err
	}
	phase = "inventory-before"
	before, err := readRecoveredStorageSnapshot(ctx, p.c, r, nil)
	if err != nil {
		return result, err
	}
	phase = "inventory-after"
	after, err := readRecoveredStorageSnapshot(ctx, p.c, r, nil)
	if err != nil || !sameRecoveredStorageObservation(before, after) {
		return result, errors.New("recovered storage changed between complete observations")
	}
	phase = "owner-after"
	if _, err = p.currentExecution(ctx); err != nil || !commandContextActive(ctx) {
		return result, errors.New("recovered storage owner or command context changed")
	}
	return after, nil
}

// readRecoveredStorageSnapshot joins complete, unfiltered API inventories. It
// intentionally waits for bootstrap Jobs and their Pods to be fully removed;
// neither a completed Job nor its retry Pods may be hidden by a selector.
// Disk/runtime qualification, generated credentials, SQL, filesystem placement
// and realized isolation remain separate required inputs to restore acceptance.
func readRecoveredStorageSnapshot(ctx context.Context, c client, r cnpgProofRecord, qualified *cnpgStorageEnvironment) (cnpgRecoveredStorageObservation, error) {
	refused := errors.New("recovered storage inventories are incomplete or contradictory")
	var zero cnpgRecoveredStorageObservation
	identity := r.Resources["Namespace"]
	if !isolationNamespace.MatchString(identity.Name) || identity.Name != "longhorn-db-proof-"+r.Target+"-"+r.Run || identity.Namespace != "" || !isolationUID.MatchString(identity.UID) {
		return zero, refused
	}
	ns := identity.Name
	readNamespace := func() (object, error) {
		n, err := c.get(ctx, "namespace", "", ns, false)
		if err != nil || !recoveredNativeObject(n, "v1", "Namespace", "") || benchmarkIdentity(n) != identity || text(n, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || text(n, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != r.Run || text(n, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
			return nil, refused
		}
		return n, nil
	}
	namespace, err := readNamespace()
	if err != nil {
		return zero, err
	}
	for _, kind := range []string{"jobs", "deployments", "statefulsets", "daemonsets", "cronjobs", "replicasets"} {
		items, e := c.list(ctx, kind, ns)
		if e != nil || len(items) != 0 || !commandContextActive(ctx) {
			return zero, refused
		}
	}
	s := cnpgRecoveredStorageObservation{namespace: namespace}
	var pods, claims, clusters, pvs, volumes, engines, replicas []object
	for _, query := range []struct {
		query, api, kind, namespace string
		destination                 *[]object
	}{
		{"pods", "v1", "Pod", ns, &pods},
		{"persistentvolumeclaims", "v1", "PersistentVolumeClaim", ns, &claims},
		{"clusters.postgresql.cnpg.io", "postgresql.cnpg.io/v1", "Cluster", ns, &clusters},
		{"persistentvolumes", "v1", "PersistentVolume", "", &pvs},
		{"volumes.longhorn.io", "longhorn.io/v1beta2", "Volume", "longhorn-system", &volumes},
		{"engines.longhorn.io", "longhorn.io/v1beta2", "Engine", "longhorn-system", &engines},
		{"replicas.longhorn.io", "longhorn.io/v1beta2", "Replica", "longhorn-system", &replicas},
	} {
		items, e := c.list(ctx, query.query, query.namespace)
		if e != nil || !recoveredStorageInventory(items, query.api, query.kind, query.namespace) || !commandContextActive(ctx) {
			return zero, refused
		}
		*query.destination = items
	}
	if len(pods) != 1 || len(claims) != 1 || len(clusters) != 1 {
		return zero, refused
	}
	s.pod, s.claim, s.cluster = pods[0], claims[0], clusters[0]
	for _, pv := range pvs {
		if text(pv, "metadata", "name") == text(s.claim, "spec", "volumeName") || text(pv, "spec", "claimRef", "uid") == text(s.claim, "metadata", "uid") || text(pv, "spec", "claimRef", "namespace") == ns && text(pv, "spec", "claimRef", "name") == text(s.claim, "metadata", "name") || text(pv, "spec", "csi", "driver") == "driver.longhorn.io" && text(pv, "spec", "csi", "volumeHandle") == "pvc-"+text(s.claim, "metadata", "uid") {
			if s.pv != nil {
				return zero, refused
			}
			s.pv = pv
		}
	}
	if s.pv == nil {
		return zero, refused
	}
	for _, volume := range volumes {
		if text(volume, "metadata", "name") == text(s.pv, "spec", "csi", "volumeHandle") || text(volume, "status", "kubernetesStatus", "pvName") == text(s.pv, "metadata", "name") || text(volume, "status", "kubernetesStatus", "namespace") == ns && text(volume, "status", "kubernetesStatus", "pvcName") == text(s.claim, "metadata", "name") {
			if s.volume != nil {
				return zero, refused
			}
			s.volume = volume
		}
	}
	if s.volume == nil {
		return zero, refused
	}
	for _, engine := range engines {
		if recoveredVolumeAssociated(engine, s.volume) {
			if s.engine != nil {
				return zero, refused
			}
			s.engine = engine
		}
	}
	if s.engine == nil {
		return zero, refused
	}
	for _, replica := range replicas {
		if recoveredCopyAssociated(replica, s.volume, s.engine) {
			s.replicas = append(s.replicas, replica)
		}
	}
	slices.SortFunc(s.replicas, func(a, b object) int {
		return strings.Compare(text(a, "metadata", "name"), text(b, "metadata", "name"))
	})
	s.binding, err = bindCNPGRecoveredStorage(r, s.cluster, s.pod, s.claim, s.pv, s.volume, s.engine, s.replicas, qualified)
	if err != nil {
		return zero, refused
	}
	after, err := readNamespace()
	if err != nil || !reflect.DeepEqual(recoveredStableObject(namespace), recoveredStableObject(after)) {
		return zero, refused
	}
	return s, nil
}

// Unrelated retiring resources remain visible. Only the selected graph must be
// live; every inventory row still needs a typed API identity and unique UID.
func recoveredStorageInventory(items []object, api, kind, namespace string) bool {
	uids, names := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		name, uid := text(item, "metadata", "name"), text(item, "metadata", "uid")
		if text(item, "apiVersion") != api || text(item, "kind") != kind || text(item, "metadata", "namespace") != namespace || name == "" || len(name) > 253 || !kubeName.MatchString(name) || !isolationUID.MatchString(uid) || text(item, "metadata", "resourceVersion") == "" || uids[uid] || names[name] {
			return false
		}
		uids[uid], names[name] = true, true
	}
	return true
}

// Resource versions, managed fields and incidental status may advance during
// healthy operation. Requalify every status predicate in each snapshot, then
// require stable native identities, specifications, ownership and processes.
func sameRecoveredStorageObservation(a, b cnpgRecoveredStorageObservation) bool {
	if !reflect.DeepEqual(a.binding, b.binding) || len(a.replicas) != len(b.replicas) {
		return false
	}
	left := append([]object{a.namespace, a.cluster, a.pod, a.claim, a.pv, a.volume, a.engine}, a.replicas...)
	right := append([]object{b.namespace, b.cluster, b.pod, b.claim, b.pv, b.volume, b.engine}, b.replicas...)
	for i := range left {
		if !reflect.DeepEqual(recoveredStableObject(left[i]), recoveredStableObject(right[i])) {
			return false
		}
	}
	return true
}

func recoveredStableObject(o object) object {
	return object{"apiVersion": value(o, "apiVersion"), "kind": value(o, "kind"), "identity": benchmarkIdentity(o), "spec": value(o, "spec"), "generation": value(o, "metadata", "generation"), "labels": value(o, "metadata", "labels"), "annotations": value(o, "metadata", "annotations"), "owners": value(o, "metadata", "ownerReferences"), "finalizers": value(o, "metadata", "finalizers"), "deleting": value(o, "metadata", "deletionTimestamp")}
}
