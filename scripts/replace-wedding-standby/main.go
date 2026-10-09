// Command replace-wedding-standby provides a narrowly scoped, volume-preserving
// repair for the failed Wedding replica. Without --execute it only reads.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type object = map[string]any

type inventory struct {
	cluster                     object
	operator                    object
	pods, claims, backups, jobs []object
	volumes                     map[string]object
}

type identity struct{ name, uid string }
type plan struct {
	primary, healthy, target identity
	claims                   []object
}

type options struct {
	clusterUID, podUID string
	now                time.Time
	fenced, detached   bool
}

type client struct {
	command func(context.Context, []string, []byte) ([]byte, error)
	source  func(context.Context) error
	wait    func(context.Context) error
	now     func() time.Time
}

func (c client) read(ctx context.Context, args ...string) (object, error) {
	b, err := c.command(ctx, append([]string{"get"}, args...), nil)
	if err != nil {
		return nil, fmt.Errorf("required %s read failed", args[0])
	}
	var result object
	if err = json.Unmarshal(b, &result); err != nil || result == nil {
		return nil, fmt.Errorf("required %s response is incomplete", args[0])
	}
	return result, nil
}
func (c client) snapshot(ctx context.Context) (inventory, error) {
	s := inventory{volumes: map[string]object{}}
	var err error
	s.cluster, err = c.read(ctx, "cluster.postgresql.cnpg.io", clusterName, "-n", namespace, "-o", "json")
	if err != nil {
		return s, err
	}
	s.operator, err = c.read(ctx, "deployment", "cloudnative-pg", "-n", "cnpg-system", "-o", "json")
	if err != nil {
		return s, err
	}
	for _, entry := range []struct {
		kind, selector string
		dst            *[]object
	}{
		{"pods", "cnpg.io/cluster=" + clusterName, &s.pods},
		{"pvc", "cnpg.io/instanceName=" + targetName, &s.claims},
		{"backups.postgresql.cnpg.io", "cnpg.io/cluster=" + clusterName, &s.backups},
		{"jobs", "cnpg.io/cluster=" + clusterName, &s.jobs},
	} {
		obj, e := c.read(ctx, entry.kind, "-n", namespace, "-l", entry.selector, "-o", "json")
		if e != nil {
			return s, e
		}
		if _, ok := value(obj, "items").([]any); !ok {
			return s, errors.New("incomplete resource listing")
		}
		*entry.dst = list(obj, "items")
	}
	for _, claim := range s.claims {
		name := str(claim, "spec", "volumeName")
		if name == "" {
			return s, errors.New("missing claim volume binding")
		}
		v, e := c.read(ctx, "pv", name, "-o", "json")
		if e != nil {
			return s, e
		}
		s.volumes[name] = v
	}
	return s, nil
}

func (c client) write(ctx context.Context, args []string, body any) error {
	if c.source == nil || c.source(ctx) != nil {
		return errors.New("current-main source proof failed; no further write")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err = c.command(ctx, args, b); err != nil {
		return fmt.Errorf("conditional %s failed; no retry or cleanup write", args[0])
	}
	return nil
}

func tests(o object) []object {
	return []object{{"op": "test", "path": "/metadata/uid", "value": id(o).uid}, {"op": "test", "path": "/metadata/resourceVersion", "value": str(o, "metadata", "resourceVersion")}}
}
func testPath(path string, v any) object     { return object{"op": "test", "path": path, "value": v} }
func editPath(op, path string, v any) object { return object{"op": op, "path": path, "value": v} }
func (c client) patch(ctx context.Context, kind string, o object, ops []object) error {
	if kind == "cluster" {
		kind = "cluster.postgresql.cnpg.io"
	}
	args := []string{"patch", kind, id(o).name, "--type=json", "--patch-file=/dev/stdin"}
	if kind != "pv" {
		args = append(args, "-n", namespace)
	}
	return c.write(ctx, args, ops)
}
func fencePatch(cluster object, remove bool) []object {
	ops := append(tests(cluster), testPath("/status/currentPrimary", str(cluster, "status", "currentPrimary")), testPath("/status/targetPrimary", str(cluster, "status", "targetPrimary")), testPath("/spec/instances", float64(3)))
	path := "/metadata/annotations/cnpg.io~1fencedInstances"
	if remove {
		return append(ops, testPath(path, `["wedding-db-1"]`), object{"op": "remove", "path": path})
	}
	if value(cluster, "metadata", "annotations") == nil {
		return append(ops, editPath("add", "/metadata/annotations", object{fenceKey: `["wedding-db-1"]`}))
	}
	return append(ops, editPath("add", path, `["wedding-db-1"]`))
}
func detachPatch(claim object, uid string) ([]object, error) {
	ops := append(tests(claim), testPath("/spec/volumeName", str(claim, "spec", "volumeName")))
	for i, ref := range list(claim, "metadata", "ownerReferences") {
		if str(ref, "uid") == uid && value(ref, "controller") == true {
			path := fmt.Sprintf("/metadata/ownerReferences/%d", i)
			return append(ops, testPath(path, ref), object{"op": "remove", "path": path}, editPath("add", "/metadata/annotations/cnpg.io~1pvcStatus", "detached")), nil
		}
	}
	return nil, errors.New("cluster ownership disappeared before detachment")
}
func (c client) pause(ctx context.Context) error {
	if c.wait != nil {
		return c.wait(ctx)
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c client) fenced(ctx context.Context) bool {
	b, err := c.command(ctx, []string{"get", "--raw", "/api/v1/namespaces/" + namespace + "/pods/" + targetName + ":9187/proxy/metrics"}, nil)
	if err != nil {
		return false
	}
	count := 0
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "cnpg_collector_fencing_on" {
			if len(fields) != 2 || fields[1] != "1" {
				return false
			}
			count++
		}
	}
	return count == 1
}
func stable(s inventory, o options, p plan, claimID, volumeID identity) error {
	current, err := validate(s, o)
	if err != nil {
		return err
	}
	if current.primary != p.primary || current.healthy != p.healthy || current.target != p.target || id(current.claims[0]) != claimID || id(s.volumes[str(current.claims[0], "spec", "volumeName")]) != volumeID {
		return errors.New("protected instance or storage identity changed")
	}
	return nil
}
func retained(s inventory, claimID, volumeID identity, detached bool) bool {
	if len(s.claims) != 1 {
		return false
	}
	claim := s.claims[0]
	volume := s.volumes[str(claim, "spec", "volumeName")]
	if !validMeta(claim, namespace) || id(claim) != claimID || str(claim, "status", "phase") != "Bound" || !validMeta(volume, "") || id(volume) != volumeID || str(volume, "status", "phase") != "Bound" || str(volume, "spec", "persistentVolumeReclaimPolicy") != "Retain" || str(volume, "spec", "claimRef", "uid") != claimID.uid || str(volume, "spec", "claimRef", "name") != claimID.name || str(volume, "spec", "claimRef", "namespace") != namespace {
		return false
	}
	if detached {
		if str(claim, "metadata", "annotations", "cnpg.io/pvcStatus") != "detached" {
			return false
		}
		for _, ref := range list(claim, "metadata", "ownerReferences") {
			if value(ref, "controller") == true {
				return false
			}
		}
	}
	return true
}

func repair(ctx context.Context, c client, o options, execute bool) error {
	s, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	p, err := validate(s, o)
	if err != nil {
		return err
	}
	if !execute {
		return nil
	}
	claimID := id(p.claims[0])
	volume := s.volumes[str(p.claims[0], "spec", "volumeName")]
	volumeID := id(volume)
	// Pin each write to a new complete observation, never retrying a rejected CAS.
	refresh := func() error {
		var e error
		s, e = c.snapshot(ctx)
		if e != nil {
			return e
		}
		if c.now != nil {
			o.now = c.now()
		}
		return stable(s, o, p, claimID, volumeID)
	}
	if err = refresh(); err != nil {
		return err
	}
	volume = s.volumes[volumeID.name]
	if str(volume, "spec", "persistentVolumeReclaimPolicy") == "Delete" {
		ops := append(tests(volume), testPath("/spec/claimRef", value(volume, "spec", "claimRef")), testPath("/spec/persistentVolumeReclaimPolicy", "Delete"), editPath("replace", "/spec/persistentVolumeReclaimPolicy", "Retain"))
		if err = c.patch(ctx, "pv", volume, ops); err != nil {
			return err
		}
	}
	if err = refresh(); err != nil {
		return err
	}
	if !retained(s, claimID, volumeID, false) {
		return errors.New("Retain policy was not read back")
	}
	if err = c.patch(ctx, "cluster", s.cluster, fencePatch(s.cluster, false)); err != nil {
		return err
	}
	o.fenced = true
	fenceCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		if err = refresh(); err != nil {
			return err
		}
		if c.fenced(fenceCtx) {
			break
		}
		if err = c.pause(fenceCtx); err != nil {
			return errors.New("instance did not acknowledge fencing; storage unchanged")
		}
	}
	if err = refresh(); err != nil {
		return err
	}
	if !retained(s, claimID, volumeID, false) || !c.fenced(ctx) {
		return errors.New("volume retention or fencing proof lost")
	}
	ops, err := detachPatch(s.claims[0], o.clusterUID)
	if err != nil {
		return err
	}
	if err = c.patch(ctx, "pvc", s.claims[0], ops); err != nil {
		return err
	}
	o.detached = true
	if err = refresh(); err != nil {
		return err
	}
	if !retained(s, claimID, volumeID, true) || !c.fenced(ctx) {
		return errors.New("detachment or fencing proof lost; pod preserved")
	}
	var target object
	for _, pod := range s.pods {
		if id(pod) == p.target {
			target = pod
		}
	}
	deletion := object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": p.target.uid, "resourceVersion": str(target, "metadata", "resourceVersion")}, "propagationPolicy": "Background"}
	if err = c.write(ctx, []string{"delete", "--raw", "/api/v1/namespaces/" + namespace + "/pods/" + targetName, "-f", "-"}, deletion); err != nil {
		return err
	}
	for {
		s, err = c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = protectedPeers(s, o, p); err != nil {
			return err
		}
		if !retained(s, claimID, volumeID, true) {
			return errors.New("old volume retention proof lost")
		}
		oldPresent := false
		for _, pod := range s.pods {
			if id(pod) == p.target {
				oldPresent = true
			}
		}
		if !oldPresent {
			break
		}
		if err = c.pause(ctx); err != nil {
			return err
		}
	}
	if str(s.cluster, "metadata", "annotations", fenceKey) != `["wedding-db-1"]` {
		return errors.New("repair fence changed; refusing to remove it")
	}
	if err = c.patch(ctx, "cluster", s.cluster, fencePatch(s.cluster, true)); err != nil {
		return err
	}
	// Require two separated complete healthy samples before returning clearance.
	good := 0
	for {
		s, err = c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = protectedPeers(s, o, p); err != nil {
			return err
		}
		if !retained(s, claimID, volumeID, true) {
			return errors.New("old volume retention proof lost")
		}
		ok, err := c.recovered(ctx, s, o, p, claimID)
		if err != nil {
			return err
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
			return errors.New("replacement did not reach a stable healthy state")
		}
	}
}

func protectedPeers(s inventory, o options, p plan) error {
	if !auditedOperator(s.operator) || !condition(s.cluster, "ContinuousArchiving") || !condition(s.cluster, "LastBackupSucceeded") {
		return errors.New("operator or backup health changed during repair")
	}
	if !validMeta(s.cluster, namespace) || id(s.cluster).uid != o.clusterUID || str(s.cluster, "status", "currentPrimary") != p.primary.name || str(s.cluster, "status", "targetPrimary") != p.primary.name || num(s.cluster, "spec", "instances") != 3 || value(s.cluster, "spec", "nodeMaintenanceWindow", "inProgress") == true {
		return errors.New("cluster or primary changed during repair")
	}
	for _, peer := range []identity{p.primary, p.healthy} {
		found := false
		for _, pod := range s.pods {
			if id(pod) == peer {
				role := "replica"
				if peer == p.primary {
					role = "primary"
				}
				found = validMeta(pod, namespace) && owned(pod, o.clusterUID) && condition(pod, "Ready") && str(pod, "metadata", "labels", "cnpg.io/instanceRole") == role
			}
		}
		if !found {
			return errors.New("protected peer identity, readiness or role changed")
		}
	}
	return nil
}
func (c client) recovered(ctx context.Context, s inventory, o options, p plan, oldClaim identity) (bool, error) {
	if num(s.cluster, "status", "readyInstances") != 3 || !condition(s.cluster, "Ready") || !condition(s.cluster, "ContinuousArchiving") || str(s.cluster, "metadata", "annotations", fenceKey) != "" || len(s.pods) != 3 {
		return false, nil
	}
	seenUID, seenName := map[string]bool{}, map[string]bool{}
	for _, pod := range s.pods {
		if seenUID[id(pod).uid] || seenName[id(pod).name] {
			return false, errors.New("duplicate recovery observations")
		}
		seenUID[id(pod).uid] = true
		seenName[id(pod).name] = true
		if !validMeta(pod, namespace) || !owned(pod, o.clusterUID) || !condition(pod, "Ready") {
			return false, nil
		}
		if id(pod) == p.primary || id(pod) == p.healthy {
			continue
		}
		if id(pod).uid == p.target.uid || str(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" {
			return false, errors.New("replacement identity or role is unsafe")
		}
		mounted := 0
		for _, v := range list(pod, "spec", "volumes") {
			name := str(v, "persistentVolumeClaim", "claimName")
			if name == "" {
				continue
			}
			mounted++
			if name == oldClaim.name {
				return false, errors.New("replacement attempted to reuse retained storage")
			}
			claim, err := c.read(ctx, "pvc", name, "-n", namespace, "-o", "json")
			if err != nil {
				return false, err
			}
			if !validMeta(claim, namespace) || !owned(claim, o.clusterUID) || id(claim).uid == oldClaim.uid || str(claim, "status", "phase") != "Bound" {
				return false, errors.New("replacement claim is not bound and owned")
			}
			pv, err := c.read(ctx, "pv", str(claim, "spec", "volumeName"), "-o", "json")
			if err != nil {
				return false, err
			}
			if !validMeta(pv, "") || str(pv, "status", "phase") != "Bound" || str(pv, "spec", "claimRef", "uid") != id(claim).uid || str(pv, "spec", "claimRef", "name") != name || str(pv, "spec", "claimRef", "namespace") != namespace {
				return false, errors.New("replacement volume binding is incomplete")
			}
		}
		if mounted != 1 {
			return false, errors.New("replacement storage layout changed")
		}
	}
	return true, nil
}

const (
	namespace   = "wedding-app"
	clusterName = "wedding-db"
	targetName  = "wedding-db-1"
	fenceKey    = "cnpg.io/fencedInstances"
)

func value(o object, path ...string) any {
	var v any = o
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
func str(o object, path ...string) string  { s, _ := value(o, path...).(string); return s }
func num(o object, path ...string) float64 { n, _ := value(o, path...).(float64); return n }
func list(o object, path ...string) []object {
	var result []object
	items, _ := value(o, path...).([]any)
	for _, v := range items {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		result = append(result, m)
	}
	return result
}
func condition(o object, name string) bool {
	for _, c := range list(o, "status", "conditions") {
		if str(c, "type") == name {
			return str(c, "status") == "True"
		}
	}
	return false
}
func id(o object) identity { return identity{str(o, "metadata", "name"), str(o, "metadata", "uid")} }
func validMeta(o object, ns string) bool {
	return id(o).name != "" && id(o).uid != "" && str(o, "metadata", "namespace") == ns && str(o, "metadata", "resourceVersion") != "" && value(o, "metadata", "deletionTimestamp") == nil
}
func owned(o object, uid string) bool {
	count := 0
	for _, ref := range list(o, "metadata", "ownerReferences") {
		if value(ref, "controller") == true {
			if str(ref, "apiVersion") != "postgresql.cnpg.io/v1" || str(ref, "kind") != "Cluster" || str(ref, "name") != clusterName || str(ref, "uid") != uid {
				return false
			}
			count++
		}
	}
	return count == 1
}
func crashLoop(p object) bool {
	for _, c := range list(p, "status", "containerStatuses") {
		if str(c, "name") == "postgres" {
			return value(c, "ready") == false && str(c, "state", "waiting", "reason") == "CrashLoopBackOff"
		}
	}
	return false
}

func auditedOperator(o object) bool {
	if !validMeta(o, "cnpg-system") || id(o).name != "cloudnative-pg" || num(o, "metadata", "generation") < 1 || num(o, "metadata", "generation") != num(o, "status", "observedGeneration") || num(o, "spec", "replicas") < 1 || num(o, "spec", "replicas") != num(o, "status", "availableReplicas") || num(o, "spec", "replicas") != num(o, "status", "updatedReplicas") {
		return false
	}
	containers := list(o, "spec", "template", "spec", "containers")
	return len(containers) == 1 && str(containers[0], "image") == "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"
}

func validate(s inventory, o options) (plan, error) {
	p := plan{}
	if !auditedOperator(s.operator) {
		return p, errors.New("operator version or rollout is outside the audited fencing procedure")
	}
	if o.clusterUID == "" || o.podUID == "" || o.now.IsZero() || !validMeta(s.cluster, namespace) || id(s.cluster).name != clusterName || id(s.cluster).uid != o.clusterUID {
		return p, errors.New("cluster identity is stale or incomplete")
	}
	primary := str(s.cluster, "status", "currentPrimary")
	if num(s.cluster, "spec", "instances") != 3 || num(s.cluster, "status", "readyInstances") != 2 || primary == "" || primary == targetName || primary != str(s.cluster, "status", "targetPrimary") || value(s.cluster, "spec", "replica") != nil || value(s.cluster, "spec", "nodeMaintenanceWindow", "inProgress") == true {
		return p, errors.New("cluster is not a stable primary with one failed standby")
	}
	expectedFence := ""
	if o.fenced {
		expectedFence = `["wedding-db-1"]`
	}
	if !condition(s.cluster, "ContinuousArchiving") || !condition(s.cluster, "LastBackupSucceeded") || str(s.cluster, "metadata", "annotations", fenceKey) != expectedFence {
		return p, errors.New("archiving, backup or fencing precondition is not clear")
	}
	if len(s.pods) != 3 {
		return p, errors.New("expected three fully observed database pods")
	}
	var target object
	seen := map[string]bool{}
	for _, pod := range s.pods {
		name := id(pod).name
		if seen[name] || !validMeta(pod, namespace) || !owned(pod, o.clusterUID) || str(pod, "metadata", "labels", "cnpg.io/cluster") != clusterName || str(pod, "metadata", "labels", "cnpg.io/instanceName") != name || str(pod, "metadata", "labels", "cnpg.io/podRole") != "instance" {
			return p, errors.New("pod identity, ownership or coverage is incomplete")
		}
		seen[name] = true
		role := str(pod, "metadata", "labels", "cnpg.io/instanceRole")
		if name == targetName {
			if id(pod).uid != o.podUID || role != "replica" || condition(pod, "Ready") || (!o.fenced && !crashLoop(pod)) {
				return p, errors.New("target is not the bound failed standby")
			}
			p.target = id(pod)
			target = pod
		} else {
			if !condition(pod, "Ready") || str(pod, "status", "phase") != "Running" {
				return p, errors.New("a protected database peer is not healthy")
			}
			if name == primary && role == "primary" {
				p.primary = id(pod)
			} else if name != primary && role == "replica" {
				p.healthy = id(pod)
			} else {
				return p, errors.New("peer role does not match primary status")
			}
		}
	}
	if p.target.uid == "" || p.primary.uid == "" || p.healthy.uid == "" {
		return p, errors.New("required instance identity missing")
	}
	var mounts []string
	for _, v := range list(target, "spec", "volumes") {
		if n := str(v, "persistentVolumeClaim", "claimName"); n != "" {
			mounts = append(mounts, n)
		}
	}
	// This repair has one known PG_DATA claim. A new WAL/tablespace layout needs
	// its own reviewed procedure, not an inferred volume selection.
	if len(mounts) != 1 || mounts[0] != targetName || len(s.claims) != 1 {
		return p, errors.New("unexpected persistent volume layout")
	}
	claim := s.claims[0]
	claimStatus := "ready"
	ownership := owned(claim, o.clusterUID)
	if o.detached {
		claimStatus = "detached"
		ownership = true
		for _, ref := range list(claim, "metadata", "ownerReferences") {
			if value(ref, "controller") == true {
				ownership = false
			}
		}
	}
	if (!o.detached && len(list(claim, "metadata", "ownerReferences")) != 1) || !validMeta(claim, namespace) || id(claim).name != mounts[0] || !ownership || str(claim, "metadata", "labels", "cnpg.io/cluster") != clusterName || str(claim, "metadata", "labels", "cnpg.io/instanceName") != targetName || str(claim, "metadata", "annotations", "cnpg.io/pvcStatus") != claimStatus || str(claim, "status", "phase") != "Bound" {
		return p, errors.New("target claim is not bound with the expected ownership")
	}
	volume, ok := s.volumes[str(claim, "spec", "volumeName")]
	if !ok || !validMeta(volume, "") || id(volume).name != str(claim, "spec", "volumeName") || str(volume, "status", "phase") != "Bound" || str(volume, "spec", "claimRef", "uid") != id(claim).uid || str(volume, "spec", "claimRef", "name") != id(claim).name || str(volume, "spec", "claimRef", "namespace") != namespace {
		return p, errors.New("volume binding is stale or incomplete")
	}
	policy := str(volume, "spec", "persistentVolumeReclaimPolicy")
	if policy != "Retain" && policy != "Delete" {
		return p, errors.New("unknown volume reclaim policy")
	}
	for _, j := range s.jobs {
		if num(j, "status", "active") != 0 || num(j, "status", "succeeded") != 1 {
			return p, errors.New("database job has not completed")
		}
	}
	backupOK := false
	for _, b := range s.backups {
		if str(b, "status", "phase") == "running" || str(b, "status", "phase") == "pending" {
			return p, errors.New("backup is active")
		}
		if !validMeta(b, namespace) || str(b, "status", "phase") != "completed" || str(b, "spec", "cluster", "name") != clusterName || str(b, "spec", "method") != "plugin" || str(b, "spec", "pluginConfiguration", "name") != "barman-cloud.cloudnative-pg.io" || str(b, "status", "pluginMetadata", "clusterUID") != o.clusterUID {
			continue
		}
		done, err := time.Parse(time.RFC3339, str(b, "status", "stoppedAt"))
		if err == nil && !done.After(o.now) && o.now.Sub(done) <= 24*time.Hour {
			backupOK = true
		}
	}
	if !backupOK {
		return p, fmt.Errorf("no completed cluster-bound backup within %s", 24*time.Hour)
	}
	p.claims = []object{claim}
	return p, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "REPAIR=HOLD:", err)
		os.Exit(1)
	}
}

func dispatchAllowed(env func(string) string) bool {
	return env("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/replace-wedding-standby.yaml@refs/heads/main" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" && env("GITHUB_REF") == "refs/heads/main" && env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_RUN_ATTEMPT") == "1" && env("WEDDING_REPAIR_CONFIRM") == "retain-volumes-replace-failed-standby" && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(env("GITHUB_SHA"))
}
func run() error {
	execute := flag.Bool("execute", false, "replace the failed standby through the protected main workflow")
	clusterUID := flag.String("cluster-uid", "", "expected current Cluster UID")
	podUID := flag.String("pod-uid", "", "expected failed Pod UID")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuid.MatchString(*clusterUID) || !uuid.MatchString(*podUID) {
		return errors.New("explicit current cluster and pod UIDs are required")
	}
	contextName := "oidc@prod"
	if *execute {
		if !dispatchAllowed(os.Getenv) {
			return errors.New("execution requires the explicitly confirmed first protected main dispatch")
		}
		contextName = "admin@prod"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	c := client{now: time.Now}
	c.command = func(ctx context.Context, args []string, body []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", contextName, "--request-timeout=30s"}, args...)...)
		cmd.Stdin = bytes.NewReader(body)
		return cmd.Output()
	}
	c.source = func(ctx context.Context) error {
		return exec.CommandContext(ctx, "bash", "scripts/verify-prod-recovery-source.sh", os.Getenv("GITHUB_SHA")).Run()
	}
	if err := repair(ctx, c, options{clusterUID: *clusterUID, podUID: *podUID, now: time.Now()}, *execute); err != nil {
		return err
	}
	result := "PLAN=PASS no writes"
	if *execute {
		result = "REPAIR=PASS readyInstances=3 retainedClaims=1 separatedSamples=2"
	}
	fmt.Println(result)
	if *execute {
		if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(f, result)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			return closeErr
		}
	}
	return nil
}
