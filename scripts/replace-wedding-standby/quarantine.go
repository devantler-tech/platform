package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const pauseKey = "cnpg.io/reconciliationLoop"
const pauseObservationKey = "platform.devantler.tech/standby-pause-observation"
const pauseObservationPath = "/metadata/annotations/platform.devantler.tech~1standby-pause-observation"
const pauseMessage = "Disable reconciliation loop annotation set, skipping the reconciliation."
const leaderLease = "db9c8771.cnpg.io"
const retainedCSIDriver = "driver.longhorn.io"
const operatorClockSkew = 2 * time.Second

var recoveryUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var observationID = regexp.MustCompile(`^[1-9][0-9]{0,19}\.1$`)
var recoverySHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type storageGuards struct{ jobUID, claimUID, volumeUID string }
type operatorLeader struct {
	lease, pod  identity
	holder      string
	transitions float64
}

// completeItems refuses missing or malformed list coverage, including empty JSON.
func completeItems(o object) ([]object, error) {
	items, ok := value(o, "items").([]any)
	if !ok || len(list(o, "items")) != len(items) {
		return nil, errors.New("resource list coverage is unknown")
	}
	return list(o, "items"), nil
}

// namespacePods includes unlabelled consumers that a cluster selector would miss.
func (c client) namespacePods(ctx context.Context) ([]object, error) {
	o, err := c.read(ctx, "pods", "-n", namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	pods, err := completeItems(o)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		// Terminating Pods still count as consumers. Only their metadata shape,
		// not validMeta's nonterminating predicate, is checked here.
		if id(pod).name == "" || id(pod).uid == "" || str(pod, "metadata", "namespace") != namespace || str(pod, "metadata", "resourceVersion") == "" {
			return nil, errors.New("namespace pod coverage is incomplete")
		}
		items, ok := value(pod, "spec", "volumes").([]any)
		if !ok || len(list(pod, "spec", "volumes")) != len(items) {
			return nil, errors.New("namespace volume coverage is incomplete")
		}
		for _, v := range list(pod, "spec", "volumes") {
			if value(v, "persistentVolumeClaim") != nil && str(v, "persistentVolumeClaim", "claimName") == "" {
				return nil, errors.New("namespace claim reference is incomplete")
			}
		}
	}
	return pods, nil
}

// mountsClaim checks all persistent claim references, not just the postgres volume.
func mountsClaim(pod object, name string) bool {
	for _, v := range list(pod, "spec", "volumes") {
		if str(v, "persistentVolumeClaim", "claimName") == name {
			return true
		}
	}
	return false
}

// completedJoinPlan accepts only the already-detached, completed-bootstrap HOLD.
func completedJoinPlan(s inventory, o options, g storageGuards) (plan, error) {
	p := plan{}
	if o.now.IsZero() || o.clusterUID == "" || o.podUID == "" || g.jobUID == "" || g.claimUID == "" || g.volumeUID == "" || !validMeta(s.cluster, namespace) || id(s.cluster).name != clusterName || id(s.cluster).uid != o.clusterUID || !auditedOperator(s.operator) || num(s.cluster, "spec", "instances") != 3 || num(s.cluster, "status", "readyInstances") != 2 || str(s.cluster, "status", "currentPrimary") == "" || str(s.cluster, "status", "currentPrimary") == targetName || str(s.cluster, "status", "targetPrimary") != str(s.cluster, "status", "currentPrimary") || str(s.cluster, "metadata", "annotations", fenceKey) != "" || !observationAbsent(s.cluster) || (o.continuePause && !ownedPause(s.cluster)) || (!o.continuePause && str(s.cluster, "metadata", "annotations", pauseKey) != "") || value(s.cluster, "spec", "nodeMaintenanceWindow", "inProgress") == true || value(s.cluster, "spec", "replica") != nil {
		return p, errors.New("completed-join recovery requires the unchanged two-peer HOLD")
	}
	if len(s.jobs) != 1 || len(s.pods) != 3 || len(s.claims) != 1 {
		return p, errors.New("completed-join coverage is incomplete")
	}
	j := s.jobs[0]
	if !validMeta(j, namespace) || id(j) != (identity{targetName + "-join", g.jobUID}) || !owned(j, o.clusterUID) || str(j, "metadata", "labels", "cnpg.io/jobRole") != "join" || str(j, "metadata", "labels", "cnpg.io/instanceName") != targetName || num(j, "status", "succeeded") != 1 || num(j, "status", "active") != 0 || num(j, "status", "failed") != 0 || str(j, "status", "completionTime") == "" || !mountsClaim(object{"spec": value(j, "spec", "template", "spec")}, targetName) {
		return p, errors.New("join job identity, ownership or completion is unproven")
	}
	for _, pod := range s.pods {
		if id(pod).uid == o.podUID {
			refs := list(pod, "metadata", "ownerReferences")
			statuses := list(pod, "status", "containerStatuses")
			if !validMeta(pod, namespace) || str(pod, "status", "phase") != "Succeeded" || len(refs) != 1 || str(refs[0], "kind") != "Job" || str(refs[0], "name") != id(j).name || str(refs[0], "uid") != g.jobUID || value(refs[0], "controller") != true || !mountsClaim(pod, targetName) || len(statuses) != 1 || str(statuses[0], "name") != "join" || str(statuses[0], "state", "terminated", "reason") != "Completed" || value(statuses[0], "state", "terminated", "exitCode") != float64(0) {
				return p, errors.New("join pod completion is unproven")
			}
			p.target = id(pod)
		} else if id(pod).name == str(s.cluster, "status", "currentPrimary") {
			if p.primary.uid != "" {
				return p, errors.New("duplicate primary")
			}
			p.primary = id(pod)
		} else {
			if p.healthy.uid != "" || id(pod).name == targetName {
				return p, errors.New("unexpected database instance")
			}
			p.healthy = id(pod)
		}
	}
	if p.target.uid == "" || p.primary.uid == "" || p.healthy.uid == "" {
		return p, errors.New("required instance identity missing")
	}
	if err := protectedPeers(s, o, p); err != nil {
		return p, err
	}
	if !recentBackup(s, o) {
		return p, errors.New("no recent cluster-bound completed backup")
	}
	p.claims = s.claims
	v := s.volumes[str(s.claims[0], "spec", "volumeName")]
	if !retained(s, identity{targetName, g.claimUID}, identity{id(v).name, g.volumeUID}, true) || str(s.claims[0], "metadata", "labels", "cnpg.io/cluster") != clusterName || str(s.claims[0], "metadata", "labels", "cnpg.io/instanceName") != targetName || str(v, "spec", "csi", "driver") != retainedCSIDriver || str(v, "spec", "csi", "volumeHandle") == "" {
		return p, errors.New("detached volume retention is unproven")
	}
	return p, nil
}

// recentBackup requires complete, cluster-bound success; active backups stop recovery.
func recentBackup(s inventory, o options) bool {
	found := false
	for _, b := range s.backups {
		if !validMeta(b, namespace) || str(b, "spec", "cluster", "name") != clusterName || (str(b, "status", "phase") != "completed" && str(b, "status", "phase") != "failed") {
			return false
		}
		t, err := time.Parse(time.RFC3339, str(b, "status", "stoppedAt"))
		if err == nil && validMeta(b, namespace) && str(b, "status", "phase") == "completed" && str(b, "spec", "cluster", "name") == clusterName && str(b, "spec", "method") == "plugin" && str(b, "spec", "pluginConfiguration", "name") == "barman-cloud.cloudnative-pg.io" && str(b, "status", "pluginMetadata", "clusterUID") == o.clusterUID && !t.After(o.now) && o.now.Sub(t) <= 24*time.Hour {
			found = true
		}
	}
	return found
}

// backingVolume positively observes the retained physical storage identity and
// its detached state. A retained PV alone cannot prove that its backing CR exists.
func (c client) backingVolume(ctx context.Context, handle string) (object, error) {
	v, err := c.read(ctx, "volumes.longhorn.io", handle, "-n", "longhorn-system", "-o", "json")
	if err != nil {
		return nil, err
	}
	if !validMeta(v, "longhorn-system") || id(v).name != handle || str(v, "status", "state") != "detached" {
		return nil, errors.New("original backing volume is missing, changed or attached")
	}
	return v, nil
}

// leader binds the pause acknowledgment to a live, unchanged operator leader.
func (c client) leader(ctx context.Context) (operatorLeader, error) {
	l := operatorLeader{}
	lease, err := c.read(ctx, "lease", leaderLease, "-n", "cnpg-system", "-o", "json")
	if err != nil {
		return l, err
	}
	parts := strings.Split(str(lease, "spec", "holderIdentity"), "_")
	renew, err := time.Parse(time.RFC3339Nano, str(lease, "spec", "renewTime"))
	now := c.now()
	_, transitionsPresent := value(lease, "spec", "leaseTransitions").(float64)
	if !validMeta(lease, "cnpg-system") || id(lease).name != leaderLease || !transitionsPresent || num(lease, "spec", "leaseTransitions") < 0 || len(parts) != 2 || !recoveryUUID.MatchString(parts[1]) || !strings.HasPrefix(parts[0], "cloudnative-pg-") || err != nil || renew.After(now.Add(operatorClockSkew)) || now.Sub(renew) > time.Duration(num(lease, "spec", "leaseDurationSeconds"))*time.Second || num(lease, "spec", "leaseDurationSeconds") <= 0 {
		return l, errors.New("operator leader lease is unknown or expired")
	}
	pod, err := c.read(ctx, "pod", parts[0], "-n", "cnpg-system", "-o", "json")
	if err != nil {
		return l, err
	}
	containers := list(pod, "spec", "containers")
	if !validMeta(pod, "cnpg-system") || id(pod).name != parts[0] || len(containers) != 1 || str(containers[0], "image") != "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1" || str(pod, "status", "phase") != "Running" || !condition(pod, "Ready") || str(pod, "metadata", "labels", "app.kubernetes.io/instance") != "cloudnative-pg" || str(pod, "metadata", "labels", "app.kubernetes.io/name") != "cloudnative-pg" {
		return l, errors.New("operator leader pod is not bound and ready")
	}
	return operatorLeader{id(lease), id(pod), str(lease, "spec", "holderIdentity"), num(lease, "spec", "leaseTransitions")}, nil
}

// pauseLogRecords refuses truncated or malformed controller log coverage.
func pauseLogRecords(b []byte) ([]object, error) {
	if len(b) > 1<<20 {
		return nil, errors.New("operator log coverage exceeds the bound")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	records := []object{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var record object
		if json.Unmarshal(line, &record) != nil || record == nil {
			return nil, errors.New("operator log coverage is malformed")
		}
		records = append(records, record)
	}
	return records, nil
}

// pauseAcknowledged accepts only a bound, recent reconciliation not observed
// before the pause. The audited controller logger emits Warning through Info;
// its structured protocol is info, not the wrapper's method name.
// The clock allowance never admits an observed reconciliation replay.
func pauseAcknowledged(b []byte, since, now time.Time, previous map[string]bool) (bool, error) {
	records, err := pauseLogRecords(b)
	if err != nil {
		return false, err
	}
	found := false
	for _, record := range records {
		t, err := time.Parse(time.RFC3339Nano, str(record, "ts"))
		if err == nil && !t.Before(since.Add(-operatorClockSkew)) && !t.After(now.Add(operatorClockSkew)) && str(record, "level") == "info" && str(record, "msg") == pauseMessage && str(record, "namespace") == namespace && str(record, "name") == clusterName && str(record, "Cluster", "namespace") == namespace && str(record, "Cluster", "name") == clusterName && recoveryUUID.MatchString(str(record, "reconcileID")) && !previous[str(record, "reconcileID")] {
			found = true
		}
	}
	return found, nil
}

// ownedPause requires exclusive ownership, not just a disabled annotation.
func ownedPause(cluster object) bool {
	return ownedAnnotation(cluster, pauseKey, "disabled")
}

func ownedAnnotation(cluster object, key, expected string) bool {
	found := false
	for _, f := range list(cluster, "metadata", "managedFields") {
		if value(f, "fieldsV1", "f:metadata", "f:annotations", "f:"+key) != nil {
			if str(f, "manager") != fieldManager {
				return false
			}
			found = true
		}
	}
	return found && str(cluster, "metadata", "annotations", key) == expected
}

// Even an empty or previously owned marker means this is not a new attempt.
func observationAbsent(cluster object) bool {
	annotations, _ := value(cluster, "metadata", "annotations").(map[string]any)
	if _, exists := annotations[pauseObservationKey]; exists {
		return false
	}
	for _, f := range list(cluster, "metadata", "managedFields") {
		if value(f, "fieldsV1", "f:metadata", "f:annotations", "f:"+pauseObservationKey) != nil {
			return false
		}
	}
	return true
}

// released retains the original PV and stale claim reservation, never clearing it
// for reuse or deleting the underlying CSI volume.
func released(volume object, oldClaim, oldVolume identity) bool {
	return validMeta(volume, "") && id(volume) == oldVolume && str(volume, "status", "phase") == "Released" && str(volume, "spec", "persistentVolumeReclaimPolicy") == "Retain" && str(volume, "spec", "claimRef", "name") == oldClaim.name && str(volume, "spec", "claimRef", "namespace") == namespace && str(volume, "spec", "claimRef", "uid") == oldClaim.uid
}

// quarantineCompletedJoin is a separate, non-resumable recovery. Its only
// deletions are the bound completed Job and detached PVC, never any PV or peer.
func quarantineCompletedJoin(ctx context.Context, c client, o options, g storageGuards, execute bool) error {
	if o.diagnosePause && (execute || o.continuePause) {
		return errors.New("pause diagnostic cannot share recovery execution")
	}
	if execute && o.continuePause && !observationID.MatchString(o.pauseObservation) {
		return errors.New("owned-pause continuation requires a new first-run observation identity")
	}
	s, err := c.snapshotPending(ctx, true)
	if err != nil {
		return err
	}
	p, err := completedJoinPlan(s, o, g)
	if err != nil {
		return err
	}
	oldClaim := id(s.claims[0])
	oldVolume := id(s.volumes[str(s.claims[0], "spec", "volumeName")])
	originalVolume := s.volumes[oldVolume.name]
	oldCSI := value(originalVolume, "spec", "csi")
	backing, err := c.backingVolume(ctx, str(originalVolume, "spec", "csi", "volumeHandle"))
	if err != nil {
		return err
	}
	backingID := id(backing)
	leader, err := c.leader(ctx)
	if err != nil {
		return err
	}
	// Verify every namespace consumer before any mutation, including unlabelled Pods.
	consumers := func(ctx context.Context, allowJoin bool) (bool, error) {
		pods, e := c.namespacePods(ctx)
		if e != nil {
			return false, e
		}
		present := false
		for _, pod := range pods {
			if mountsClaim(pod, oldClaim.name) {
				if id(pod) != p.target || str(pod, "status", "phase") != "Succeeded" {
					return false, errors.New("retained claim has an unexpected consumer")
				}
				present = true
			}
		}
		return allowJoin || !present, nil
	}
	if _, err = consumers(ctx, true); err != nil {
		return err
	}
	since := c.now()
	logArgs := []string{"logs", leader.pod.name, "-n", "cnpg-system", "--since-time=" + since.Add(-operatorClockSkew).Format(time.RFC3339Nano), "--limit-bytes=1048577"}
	baseline, err := c.command(ctx, logArgs, nil)
	if err != nil {
		return errors.New("pre-pause operator log read failed; no writes")
	}
	records, err := pauseLogRecords(baseline)
	if err != nil {
		return err
	}
	previous := map[string]bool{}
	for _, record := range records {
		if reconcile := str(record, "reconcileID"); recoveryUUID.MatchString(reconcile) {
			previous[reconcile] = true
		}
	}
	if !execute && !o.diagnosePause {
		return nil
	}
	paused, marked := o.continuePause, false
	// Rebind after unrelated reads without removing the version precondition.
	rebindCluster := func(ctx context.Context) error {
		if e := c.proveSource(ctx); e != nil {
			return e
		}
		cluster, e := c.read(ctx, "cluster.postgresql.cnpg.io", clusterName, "-n", namespace, "--show-managed-fields=true", "-o", "json")
		if e != nil {
			return e
		}
		s.cluster = cluster
		o.now = c.now()
		if id(cluster).name != clusterName {
			return errors.New("cluster name changed during repair")
		}
		if e = protectedPeers(s, o, p); e != nil {
			return e
		}
		markerValid := observationAbsent(cluster)
		if marked {
			markerValid = ownedAnnotation(cluster, pauseObservationKey, o.pauseObservation)
		}
		if !recentBackup(s, o) || (paused && !ownedPause(cluster)) || !markerValid || str(cluster, "metadata", "annotations", fenceKey) != "" {
			return errors.New("backup or pause ownership changed")
		}
		return nil
	}
	refresh := func(ctx context.Context) error {
		if e := c.proveSource(ctx); e != nil {
			return e
		}
		var e error
		s, e = c.snapshotPending(ctx, true)
		if e != nil {
			return e
		}
		original, e := c.read(ctx, "pv", oldVolume.name, "-o", "json")
		if e != nil {
			return e
		}
		if id(original) != oldVolume || !reflect.DeepEqual(value(original, "spec", "csi"), oldCSI) {
			return errors.New("original backing volume identity changed")
		}
		backing, e := c.backingVolume(ctx, backingID.name)
		if e != nil || id(backing) != backingID {
			return errors.New("original backing volume retention is unproven")
		}
		current, e := c.leader(ctx)
		if e != nil || current != leader {
			return errors.New("operator leader changed; no further write")
		}
		return rebindCluster(ctx)
	}
	if err = refresh(ctx); err != nil {
		return err
	}
	if _, err = completedJoinPlan(s, o, g); err != nil {
		return err
	}
	ops := pausePatch(s.cluster, p.primary.name)
	if o.diagnosePause {
		// A dry-run response is an observation only. Return before setting pause
		// ownership, waiting for acknowledgment or entering any cleanup path.
		args := append(patchArgs("cluster", s.cluster), "--dry-run=server")
		return c.write(ctx, args, ops)
	}
	if err = guardedClusterPatch(ctx, c, s.cluster, func(ctx context.Context) (object, error) {
		if e := refresh(ctx); e != nil {
			return nil, e
		}
		if _, e := completedJoinPlan(s, o, g); e != nil {
			return nil, e
		}
		if _, e := consumers(ctx, true); e != nil {
			return nil, e
		}
		if e := rebindCluster(ctx); e != nil {
			return nil, e
		}
		return s.cluster, nil
	}, func(cluster object) []object {
		if o.continuePause {
			// Capture the actual request boundary, including a positively rejected
			// request's rebind. A baseline-time or pre-marker record is insufficient.
			since = c.now()
			return observationPatch(cluster, p.primary.name, o.pauseObservation)
		}
		return pausePatch(cluster, p.primary.name)
	}); err != nil {
		return err
	}
	paused = true
	marked = o.continuePause
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		if err = refresh(ackCtx); err != nil {
			return err
		}
		b, e := c.command(ackCtx, logArgs, nil)
		if e != nil {
			return errors.New("operator pause acknowledgment read failed")
		}
		ackSince := since
		if o.continuePause {
			// The parser subtracts one clock allowance. Add two so the effective
			// cutoff remains one allowance after the request: even a fast
			// controller clock cannot admit an unseen pre-marker record.
			ackSince = since.Add(2 * operatorClockSkew)
		}
		acknowledged, e := pauseAcknowledged(b, ackSince, c.now(), previous)
		if e != nil {
			return e
		}
		if acknowledged {
			break
		}
		if err = c.pause(ackCtx); err != nil {
			return errors.New("operator did not acknowledge pause; storage unchanged")
		}
	}
	if err = refresh(ctx); err != nil {
		return err
	}
	if !retained(s, oldClaim, oldVolume, true) {
		return errors.New("old retention changed before job removal")
	}
	if len(s.jobs) != 1 || id(s.jobs[0]) != (identity{targetName + "-join", g.jobUID}) || !owned(s.jobs[0], o.clusterUID) || !validMeta(s.jobs[0], namespace) || str(s.jobs[0], "metadata", "labels", "cnpg.io/jobRole") != "join" || str(s.jobs[0], "metadata", "labels", "cnpg.io/instanceName") != targetName || num(s.jobs[0], "status", "succeeded") != 1 || num(s.jobs[0], "status", "active") != 0 || num(s.jobs[0], "status", "failed") != 0 {
		return errors.New("completed job changed")
	}
	if _, err = consumers(ctx, true); err != nil {
		return err
	}
	job := s.jobs[0]
	deletion := object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": g.jobUID, "resourceVersion": str(job, "metadata", "resourceVersion")}, "propagationPolicy": "Foreground"}
	if err = c.write(ctx, []string{"delete", "--raw", "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + targetName + "-join", "-f", "-"}, deletion); err != nil {
		return err
	}
	for {
		if err = refresh(ctx); err != nil {
			return err
		}
		if !retained(s, oldClaim, oldVolume, true) {
			return errors.New("retention changed during job removal")
		}
		if len(s.jobs) == 0 {
			absent, e := consumers(ctx, false)
			if e != nil {
				return e
			}
			if absent {
				break
			}
		} else if len(s.jobs) != 1 || id(s.jobs[0]).uid != g.jobUID {
			return errors.New("unexpected job appeared during pause")
		}
		if err = c.pause(ctx); err != nil {
			return errors.New("job or claim consumers did not disappear")
		}
	}
	// Rebind the same observations immediately before the PVC delete.
	if err = refresh(ctx); err != nil {
		return err
	}
	if len(s.jobs) != 0 || !retained(s, oldClaim, oldVolume, true) {
		return errors.New("retained claim delete preconditions changed")
	}
	if absent, e := consumers(ctx, false); e != nil || !absent {
		return errors.New("claim consumer absence is unproven")
	}
	deletion = object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": oldClaim.uid, "resourceVersion": str(s.claims[0], "metadata", "resourceVersion")}, "propagationPolicy": "Background"}
	if err = c.write(ctx, []string{"delete", "--raw", "/api/v1/namespaces/" + namespace + "/persistentvolumeclaims/" + oldClaim.name, "-f", "-"}, deletion); err != nil {
		return err
	}
	for {
		if err = refresh(ctx); err != nil {
			return err
		}
		v, e := c.read(ctx, "pv", oldVolume.name, "-o", "json")
		if e != nil {
			return e
		}
		if len(s.claims) == 0 && released(v, oldClaim, oldVolume) {
			break
		}
		if len(s.claims) > 1 || (len(s.claims) == 1 && id(s.claims[0]) != oldClaim) {
			return errors.New("unexpected claim appeared during pause")
		}
		if !validMeta(v, "") || id(v) != oldVolume || str(v, "spec", "persistentVolumeReclaimPolicy") != "Retain" || str(v, "spec", "claimRef", "uid") != oldClaim.uid {
			return errors.New("original volume retention changed")
		}
		if err = c.pause(ctx); err != nil {
			return errors.New("original volume did not become safely retained")
		}
	}
	if err = refresh(ctx); err != nil {
		return err
	}
	v, err := c.read(ctx, "pv", oldVolume.name, "-o", "json")
	if err != nil {
		return err
	}
	if len(s.jobs) != 0 || len(s.claims) != 0 || !released(v, oldClaim, oldVolume) {
		return errors.New("quarantine readback changed before resume")
	}
	if absent, e := consumers(ctx, false); e != nil || !absent {
		return errors.New("claim consumer absence is unproven")
	}
	if err = rebindCluster(ctx); err != nil {
		return err
	}
	if err = guardedClusterPatch(ctx, c, s.cluster, func(ctx context.Context) (object, error) {
		if e := refresh(ctx); e != nil {
			return nil, e
		}
		v, e := c.read(ctx, "pv", oldVolume.name, "-o", "json")
		if e != nil {
			return nil, e
		}
		if len(s.jobs) != 0 || len(s.claims) != 0 || !released(v, oldClaim, oldVolume) {
			return nil, errors.New("quarantine readback changed after resume rejection")
		}
		if absent, e := consumers(ctx, false); e != nil || !absent {
			return nil, errors.New("claim consumer absence is unproven")
		}
		if e := rebindCluster(ctx); e != nil {
			return nil, e
		}
		return s.cluster, nil
	}, func(cluster object) []object {
		marker := ""
		if marked {
			marker = o.pauseObservation
		}
		return resumePausePatch(cluster, p.primary.name, marker)
	}); err != nil {
		return err
	}
	paused = false
	marked = false
	good := 0
	for {
		if err = refresh(ctx); err != nil {
			return err
		}
		v, err = c.read(ctx, "pv", oldVolume.name, "-o", "json")
		if err != nil {
			return err
		}
		if !released(v, oldClaim, oldVolume) || str(s.cluster, "metadata", "annotations", pauseKey) != "" {
			return errors.New("retained original volume or resumed reconciliation changed")
		}
		for _, claim := range s.claims {
			if id(claim).uid == oldClaim.uid || str(claim, "spec", "volumeName") == oldVolume.name {
				return errors.New("replacement reused the retained storage")
			}
			if name := str(claim, "spec", "volumeName"); name != "" {
				pv := s.volumes[name]
				if str(pv, "spec", "csi", "driver") != retainedCSIDriver || str(pv, "spec", "csi", "volumeHandle") == "" || str(pv, "spec", "csi", "volumeHandle") == str(object{"csi": oldCSI}, "csi", "volumeHandle") {
					return errors.New("replacement backing volume is not fresh")
				}
			}
		}
		ok, e := c.recovered(ctx, s, o, p, oldClaim, originalVolume)
		if e != nil {
			return e
		}
		if ok {
			good++
		} else {
			good = 0
		}
		if good == 2 {
			return nil
		}
		if err = c.pause(ctx); err != nil {
			return errors.New("replacement did not reach two healthy observations")
		}
	}
}

// pausePatch binds the same first request to the observed identity, version,
// stable primary, replica count and complete annotation map in both modes.
func pausePatch(cluster object, primary string) []object {
	ops := append(tests(cluster), testPath("/status/currentPrimary", primary), testPath("/status/targetPrimary", primary), testPath("/spec/instances", float64(3)))
	if value(cluster, "metadata", "annotations") == nil {
		return append(ops, editPath("add", "/metadata/annotations", object{pauseKey: "disabled"}))
	}
	return append(ops, testPath("/metadata/annotations", value(cluster, "metadata", "annotations")), editPath("add", "/metadata/annotations/cnpg.io~1reconciliationLoop", "disabled"))
}

// observationPatch changes only a new run-bound marker. The audited controller
// watches Cluster metadata updates, so this requests a fresh paused reconcile
// without briefly enabling the operator or removing the existing pause.
func observationPatch(cluster object, primary, marker string) []object {
	return append(tests(cluster), testPath("/status/currentPrimary", primary), testPath("/status/targetPrimary", primary), testPath("/spec/instances", float64(3)), testPath("/metadata/annotations/cnpg.io~1reconciliationLoop", "disabled"), testPath("/metadata/annotations", value(cluster, "metadata", "annotations")), editPath("add", pauseObservationPath, marker))
}

func resumePausePatch(cluster object, primary, marker string) []object {
	ops := append(tests(cluster), testPath("/status/currentPrimary", primary), testPath("/status/targetPrimary", primary), testPath("/spec/instances", float64(3)), testPath("/metadata/annotations", value(cluster, "metadata", "annotations")), testPath("/metadata/annotations/cnpg.io~1reconciliationLoop", "disabled"))
	if marker != "" {
		ops = append(ops, testPath(pauseObservationPath, marker), object{"op": "remove", "path": pauseObservationPath})
	}
	return append(ops, object{"op": "remove", "path": "/metadata/annotations/cnpg.io~1reconciliationLoop"})
}

// storageDispatchAllowed admits only the separately approved, first main dispatch.
func storageDispatchAllowed(env func(string) string) bool {
	return env("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" && env("GITHUB_REF") == "refs/heads/main" && env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_RUN_ATTEMPT") == "1" && env("WEDDING_REPAIR_CONFIRM") == "retain-volume-rebuild-after-rejection" && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(env("GITHUB_SHA"))
}

func ownedPauseDispatchAllowed(env func(string) string) bool {
	return env("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" && env("GITHUB_REF") == "refs/heads/main" && env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_RUN_ATTEMPT") == "1" && env("WEDDING_REPAIR_CONFIRM") == "continue-owned-pause-retain-volume" && recoverySHA.MatchString(env("GITHUB_SHA")) && observationID.MatchString(env("GITHUB_RUN_ID")+".1")
}

// quarantineResult reports only aggregate evidence, not storage identifiers.
func quarantineResult(execute bool) string {
	if execute {
		return "REPAIR=PASS readyInstances=3 retainedVolumes=1 separatedSamples=2"
	}
	return "PLAN=PASS completedJoin=1 retainedVolumes=1 noWrites=1"
}
