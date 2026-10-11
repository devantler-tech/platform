package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const journalName = "longhorn-client-maintenance"
const journalNS = "flux-system"
const restrictedSelector = "node.longhorn.io/create-default-disk:true"

var errPatchConflict = errors.New("resourceVersion conflict")
var errResourceMissing = errors.New("resource not found")

type command func(context.Context, []byte, ...string) ([]byte, error)
type client struct{ command command }
type journal struct {
	Holder, Source, Phase          string
	ResourceHolder                 string
	PublicationNotStarted          bool
	Clusters, Claims, PVs, Volumes []object
	Nodes                          []object // Unsupported pre-publication experiment; reject on read.
	Release, Deployment            object
	Extra                          []workload
}
type engine struct {
	c                     client
	holder, source        string
	now                   func() time.Time
	sleep                 func(time.Duration)
	wait                  time.Duration
	publicationNotStarted bool
	previousFinished      func(context.Context, journal) error
	revisionSafe          func(context.Context) error
}

func (c client) read(ctx context.Context, kind, ns, name string) (object, error) {
	a := []string{"get", kind}
	if name != "" {
		a = append(a, name)
	}
	if ns == "*" {
		a = append(a, "-A")
	} else if ns != "" {
		a = append(a, "-n", ns)
	}
	a = append(a, "-o", "json")
	b, e := c.command(ctx, nil, a...)
	if e != nil {
		if strings.Contains(e.Error(), "(NotFound)") {
			return nil, errResourceMissing
		}
		return nil, e
	}
	var o object
	e = json.Unmarshal(b, &o)
	if e != nil || o == nil {
		return nil, errors.New("resource read is not a JSON object")
	}
	return o, nil
}
func (c client) list(ctx context.Context, kind, ns string) ([]object, error) {
	o, e := c.read(ctx, kind, ns, "")
	if e != nil {
		return nil, e
	}
	a, ok := val(o, "items").([]any)
	if !ok || val(o, "metadata", "continue") != nil && str(o, "metadata", "continue") != "" {
		return nil, errors.New("resource list is incomplete")
	}
	r := items(o, "items")
	if len(r) != len(a) {
		return nil, errors.New("resource list contains malformed entries")
	}
	return r, nil
}
func (c client) patch(ctx context.Context, kind string, o object, p []object) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	a := []string{"patch", kind, str(o, "metadata", "name"), "-n", str(o, "metadata", "namespace"), "--type=json", "--patch", string(b), "-o", "name"}
	_, e = c.command(ctx, nil, a...)
	if e != nil && strings.Contains(e.Error(), "(Conflict)") {
		return fmt.Errorf("%w: %w", errPatchConflict, e)
	}
	return e
}

// Rebuild each CAS from a fresh read. Status-only conflicts are retryable;
// replaced identities, changed specs, foreign ownership and denied writes are not.
func (e engine) patchOwned(ctx context.Context, old object, restoring bool) error {
	for attempt := 0; attempt < 5; attempt++ {
		current, err := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
		if err != nil {
			return err
		}
		var patch []object
		if restoring {
			if released(old, current) {
				return nil
			}
			patch, err = restorePatch(old, current, e.holder, str(old, "kind") == "Deployment")
		} else {
			if !released(old, current) {
				return errors.New("resource changed before its owned pause")
			}
			patch = pausePatch(current, e.holder)
		}
		if err != nil {
			return err
		}
		err = e.c.patch(ctx, resourceKind(old), current, patch)
		if !errors.Is(err, errPatchConflict) {
			return err
		}
		e.sleep(time.Second)
	}
	return errors.New("bounded owned-patch conflict retry expired")
}

func released(old, current object) bool {
	if !meta(current) || str(current, "metadata", "uid") != str(old, "metadata", "uid") || key(old) != key(current) || str(current, "metadata", "annotations", ownerKey) != "" {
		return false
	}
	return reflect.DeepEqual(val(current, "spec"), val(old, "spec")) && val(current, "metadata", "annotations", hibernateKey) == val(old, "metadata", "annotations", hibernateKey) && val(current, "metadata", "annotations", reconcileKey) == val(old, "metadata", "annotations", reconcileKey)
}

func journalResources(j journal) []object {
	r := append([]object{}, j.Clusters...)
	r = append(r, j.Deployment, j.Release)
	for _, w := range j.Extra {
		r = append(r, w.Deployment, w.Release)
	}
	return r
}

// A finished restoration releases only the fields this procedure owns. Later
// reviewed consumer upgrades must not leave a restored journal blocking forever.
// Pausing and interrupted restoration still require released's strict spec proof.
func ownershipReleased(old, current object) bool {
	if !meta(current) || str(current, "metadata", "uid") != str(old, "metadata", "uid") || key(old) != key(current) || str(current, "metadata", "annotations", ownerKey) != "" {
		return false
	}
	if val(current, "metadata", "annotations", hibernateKey) != val(old, "metadata", "annotations", hibernateKey) || val(current, "metadata", "annotations", reconcileKey) != val(old, "metadata", "annotations", reconcileKey) {
		return false
	}
	switch str(old, "kind") {
	case "HelmRelease":
		return reflect.DeepEqual(val(current, "spec", "suspend"), val(old, "spec", "suspend"))
	case "Deployment":
		return reflect.DeepEqual(val(current, "spec", "replicas"), val(old, "spec", "replicas"))
	case "Cluster":
		return true
	}
	return false
}

func (e engine) provenReleased(ctx context.Context, j journal) error {
	for _, old := range journalResources(j) {
		current, err := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
		if err != nil {
			return err
		}
		if !ownershipReleased(old, current) {
			return errors.New("previous maintenance has not released its original resources")
		}
	}
	return nil
}
func (e engine) collect(ctx context.Context) (inventory, error) {
	s := inventory{}
	for _, q := range []struct {
		kind, ns string
		dst      *[]object
	}{
		{"clusters.postgresql.cnpg.io", "*", &s.clusters}, {"pods", "*", &s.pods}, {"persistentvolumeclaims", "*", &s.claims}, {"persistentvolumes", "", &s.pvs}, {"volumes.longhorn.io", "longhorn-system", &s.volumes}, {"backups.postgresql.cnpg.io", "*", &s.backups}, {"replicasets", "*", &s.replicasets},
	} {
		a, err := e.c.list(ctx, q.kind, q.ns)
		if err != nil {
			return s, err
		}
		*q.dst = a
	}
	var err error
	s.release, err = e.c.read(ctx, "helmrelease", "crossview", "crossview")
	if err != nil {
		return s, err
	}
	s.deployment, err = e.c.read(ctx, "deployment", "crossview", "crossview-postgres")
	if err != nil {
		return s, err
	}
	for _, t := range extraTargets {
		r, err := e.c.read(ctx, "helmrelease", t.ns, t.release)
		if err != nil {
			return s, err
		}
		d, err := e.c.read(ctx, "deployment", t.ns, t.deployment)
		if err != nil {
			return s, err
		}
		s.extra = append(s.extra, workload{r, d})
	}
	return s, err
}
func (e engine) readJournal(ctx context.Context) (journal, object, error) {
	o, err := e.c.read(ctx, "configmap", journalNS, journalName)
	if err != nil {
		return journal{}, nil, err
	}
	var j journal
	err = json.Unmarshal([]byte(str(o, "data", "journal.json")), &j)
	if err != nil || !meta(o) || str(o, "metadata", "annotations", ownerKey) != j.Holder || j.Holder == "" || j.Source == "" {
		return j, o, errors.New("maintenance journal is malformed")
	}
	if err = journalScope(j); err != nil {
		return j, o, err
	}
	return j, o, nil
}
func (e engine) writeJournal(ctx context.Context, j journal, o object) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if o != nil {
		return e.c.patch(ctx, "configmap", o, append(tests(o), object{"op": "replace", "path": "/data/journal.json", "value": string(b)}, object{"op": "replace", "path": "/metadata/annotations/" + escape(ownerKey), "value": j.Holder}))
	}
	v := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": journalName, "namespace": journalNS, "annotations": object{ownerKey: j.Holder}}, "data": object{"journal.json": string(b)}}
	b, err = json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.c.command(ctx, b, "create", "-f", "-", "-o", "name")
	return err
}
func (e engine) ownerJournal(ctx context.Context) (journal, object, error) {
	j, o, err := e.readJournal(ctx)
	if err == nil && (j.Holder != e.holder || j.Source != e.source) {
		err = errors.New("maintenance belongs to another run or source; refusing adoption")
	}
	return j, o, err
}

func (e engine) poll(ctx context.Context, label string, check func(context.Context) (bool, error)) error {
	deadline := e.now().Add(e.wait)
	for {
		ok, err := check(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !e.now().Before(deadline) {
			return fmt.Errorf("bounded %s wait expired", label)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		e.sleep(10 * time.Second)
	}
}
func resourceKind(o object) string {
	switch str(o, "kind") {
	case "Cluster":
		return "clusters.postgresql.cnpg.io"
	case "HelmRelease":
		return "helmrelease"
	case "Deployment":
		return "deployment"
	}
	return ""
}
func pausePatch(o object, holder string) []object {
	p := tests(o)
	if val(o, "metadata", "annotations") == nil {
		p = append(p, object{"op": "add", "path": "/metadata/annotations", "value": object{}})
	}
	for k, v := range map[string]string{ownerKey: holder, reconcileKey: "disabled"} {
		p = append(p, object{"op": "add", "path": "/metadata/annotations/" + escape(k), "value": v})
	}
	switch str(o, "kind") {
	case "Cluster":
		p = append(p, object{"op": "add", "path": "/metadata/annotations/" + escape(hibernateKey), "value": "on"})
	case "HelmRelease":
		p = append(p, object{"op": "add", "path": "/spec/suspend", "value": true})
	case "Deployment":
		p = append(p, object{"op": "replace", "path": "/spec/replicas", "value": 0})
	}
	return p
}

func (e engine) detached(ctx context.Context, j journal) (bool, error) {
	volumes, err := e.c.list(ctx, "volumes.longhorn.io", "longhorn-system")
	if err != nil {
		return false, err
	}
	if len(volumes) != len(j.Volumes) {
		return false, errors.New("volume inventory changed during maintenance")
	}
	byKey, err := index(volumes)
	if err != nil {
		return false, err
	}
	for _, old := range j.Volumes {
		v := byKey[key(old)]
		if v == nil || str(v, "metadata", "uid") != str(old, "metadata", "uid") {
			return false, errors.New("backing volume identity changed")
		}
		if str(v, "status", "state") != "detached" {
			return false, nil
		}
	}
	for _, old := range j.Clusters {
		c, err := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
		if err != nil {
			return false, err
		}
		if str(c, "metadata", "uid") != str(old, "metadata", "uid") || str(c, "metadata", "annotations", ownerKey) != resourceHolder(j) || !reflect.DeepEqual(val(c, "spec"), val(old, "spec")) {
			return false, errors.New("database maintenance identity changed")
		}
		if !condition(c, hibernateKey) {
			return false, nil
		}
	}
	pods, err := e.c.list(ctx, "pods", "*")
	if err != nil {
		return false, err
	}
	claims := map[string]bool{}
	for _, c := range j.Claims {
		claims[key(c)] = true
	}
	for _, p := range pods {
		if str(p, "status", "phase") == "Succeeded" || str(p, "status", "phase") == "Failed" {
			continue
		}
		for _, v := range items(p, "spec", "volumes") {
			if claims[str(p, "metadata", "namespace")+"/"+str(v, "persistentVolumeClaim", "claimName")] {
				return false, nil
			}
		}
	}
	return true, nil
}

func (e engine) clientsReady(ctx context.Context) (bool, error) {
	for _, setting := range []struct{ name, value string }{{"system-managed-components-node-selector", ""}, {"create-default-disk-labeled-nodes", "true"}} {
		o, err := e.c.read(ctx, "settings.longhorn.io", "longhorn-system", setting.name)
		if err != nil {
			return false, err
		}
		if val(o, "value") != setting.value {
			return false, nil
		}
	}
	for _, name := range []string{"longhorn-manager", "longhorn-csi-plugin"} {
		o, err := e.c.read(ctx, "daemonset", "longhorn-system", name)
		if err != nil {
			return false, err
		}
		if str(o, "spec", "template", "spec", "nodeSelector", "node.longhorn.io/create-default-disk") != "" || num(o, "status", "desiredNumberScheduled") < 3 || num(o, "status", "numberReady") != num(o, "status", "desiredNumberScheduled") || num(o, "status", "updatedNumberScheduled") != num(o, "status", "desiredNumberScheduled") || num(o, "status", "observedGeneration") != num(o, "metadata", "generation") {
			return false, nil
		}
	}
	daemonsets, err := e.c.list(ctx, "daemonsets", "longhorn-system")
	if err != nil {
		return false, err
	}
	engines := 0
	for _, d := range daemonsets {
		if str(d, "metadata", "labels", "longhorn.io/component") != "engine-image" {
			continue
		}
		engines++
		if str(d, "spec", "template", "spec", "nodeSelector", "node.longhorn.io/create-default-disk") != "" || num(d, "status", "desiredNumberScheduled") < 3 || num(d, "status", "numberReady") != num(d, "status", "desiredNumberScheduled") || num(d, "status", "updatedNumberScheduled") != num(d, "status", "desiredNumberScheduled") || num(d, "status", "observedGeneration") != num(d, "metadata", "generation") {
			return false, nil
		}
	}
	if engines == 0 {
		return false, errors.New("engine-image client coverage is incomplete")
	}
	nodes, err := e.c.list(ctx, "nodes", "")
	if err != nil {
		return false, err
	}
	csi, err := e.c.list(ctx, "csinodes", "")
	if err != nil {
		return false, err
	}
	lh, err := e.c.list(ctx, "nodes.longhorn.io", "longhorn-system")
	if err != nil {
		return false, err
	}
	byName := map[string]object{}
	for _, n := range csi {
		byName[str(n, "metadata", "name")] = n
	}
	byLH := map[string]object{}
	for _, n := range lh {
		byLH[str(n, "metadata", "name")] = n
	}
	workers := 0
	for _, n := range nodes {
		if val(n, "metadata", "labels", "node-role.kubernetes.io/control-plane") != nil {
			continue
		}
		if !condition(n, "Ready") {
			return false, nil
		}
		if runnerOnlyNode(n) {
			continue
		}
		workers++
		registered := false
		for _, d := range items(byName[str(n, "metadata", "name")], "spec", "drivers") {
			if str(d, "name") == "driver.longhorn.io" {
				registered = true
			}
		}
		if !registered {
			return false, nil
		}
		if strings.HasPrefix(str(n, "metadata", "name"), "autoscale-") {
			l := byLH[str(n, "metadata", "name")]
			if l == nil {
				return false, nil
			}
			if val(l, "spec", "allowScheduling") != false || val(l, "spec", "evictionRequested") != true {
				return false, errors.New("compute-worker replica exclusion is unproven")
			}
		}
	}
	if workers < 3 {
		return false, errors.New("worker coverage is incomplete")
	}
	replicas, err := e.c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil {
		return false, err
	}
	for _, r := range replicas {
		if strings.HasPrefix(str(r, "spec", "nodeID"), "autoscale-") {
			return false, errors.New("replica storage exists on a compute worker")
		}
	}
	return true, nil
}

// The declared runner-only pool deliberately cannot schedule Longhorn clients.
// Require all three signals: a similarly named ordinary worker must still prove
// CSI registration, and unhealthy workers are rejected before this exception.
func runnerOnlyNode(n object) bool {
	if !strings.HasPrefix(str(n, "metadata", "name"), "autoscale-arc-runners-") || str(n, "metadata", "labels", "platform.devantler.tech/ci-runner") != "enabled" {
		return false
	}
	for _, t := range items(n, "spec", "taints") {
		if str(t, "key") == "platform.devantler.tech/ci-runner" && str(t, "value") == "enabled" && str(t, "effect") == "NoSchedule" {
			return true
		}
	}
	return false
}

func (e engine) prepare(ctx context.Context) error {
	// Interrupted holders require terminal-owner and live identity proof before
	// CAS takeover; a live or unknown holder remains a hard stop.
	journals, err := e.c.list(ctx, "configmaps", journalNS)
	if err != nil {
		return err
	}
	var previous *journal
	var previousObject object
	for _, o := range journals {
		if str(o, "metadata", "name") == journalName {
			j, original, err := e.readJournal(ctx)
			if err != nil {
				return err
			}
			if j.Phase == "restored" {
				if err = e.provenReleased(ctx, j); err != nil {
					return err
				}
				previous, previousObject = &j, original
				break
			}
			if j.Phase != "complete" {
				return e.recoverInterrupted(ctx, j, original)
			}
			if err = e.poll(ctx, "completed client placement", e.clientsReady); err != nil {
				return fmt.Errorf("completed maintenance drifted; a new reviewed repair is required: %w", err)
			}
			return nil
		}
	}
	setting, err := e.c.read(ctx, "settings.longhorn.io", "longhorn-system", "system-managed-components-node-selector")
	if err != nil {
		return err
	}
	if val(setting, "value") == "" {
		if previous != nil {
			if e.revisionSafe == nil {
				return errors.New("published revision proof is missing")
			}
			if err = e.revisionSafe(ctx); err != nil {
				return err
			}
		}
		if err = e.poll(ctx, "migrated client placement", e.clientsReady); err != nil {
			return err
		}
		if previous != nil {
			if err = e.provenReleased(ctx, *previous); err != nil {
				return err
			}
			previous.Holder, previous.Source = e.holder, e.source
			return e.writeJournal(ctx, *previous, previousObject)
		}
		return nil
	}
	if val(setting, "value") != restrictedSelector {
		return errors.New("live selector is outside the reviewed migration")
	}
	if e.revisionSafe == nil {
		return errors.New("published revision proof is missing")
	}
	if err = e.revisionSafe(ctx); err != nil {
		return err
	}
	s, err := e.collect(ctx)
	if err != nil {
		return err
	}
	p, err := buildPlan(s, e.now())
	if err != nil {
		return err
	}
	hpas, err := e.c.list(ctx, "horizontalpodautoscalers", "*")
	if err != nil {
		return err
	}
	for _, h := range hpas {
		if str(h, "spec", "scaleTargetRef", "name") == "crossview-postgres" || str(h, "spec", "scaleTargetRef", "name") == "storage" || str(h, "spec", "scaleTargetRef", "name") == "actual-budget-actualbudget" {
			return errors.New("bundled database has another replica owner")
		}
	}
	nodes, err := e.c.list(ctx, "nodes", "")
	if err != nil {
		return err
	}
	storageNodes := 0
	for _, n := range nodes {
		if !meta(n) || !condition(n, "Ready") || str(n, "metadata", "annotations", ownerKey) != "" {
			return errors.New("node readiness or maintenance ownership is unproven")
		}
		if condition(n, "Ready") && !strings.HasPrefix(str(n, "metadata", "name"), "autoscale-") && str(n, "metadata", "labels", "node.longhorn.io/create-default-disk") == "true" {
			storageNodes++
		}
	}
	if storageNodes < 3 {
		return errors.New("three ready baseline storage workers are required")
	}
	replicas, err := e.c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, r := range replicas {
		if strings.HasPrefix(str(r, "spec", "nodeID"), "autoscale-") {
			return errors.New("compute-worker replica storage must be evacuated before this migration")
		}
	}
	if err = e.replicaExclusion(ctx); err != nil {
		return err
	}
	j := journal{Holder: e.holder, Source: e.source, Phase: "prepared", PublicationNotStarted: true, Clusters: p.clusters, Claims: p.claims, PVs: p.pvs, Volumes: p.volumes, Release: p.release, Deployment: p.deployment, Extra: p.extra}
	if previous != nil {
		if err = e.provenReleased(ctx, *previous); err != nil {
			return err
		}
	}
	if err = e.writeJournal(ctx, j, previousObject); err != nil {
		return err
	}
	// CNPG 1.30.1 intentionally refuses to begin hibernation while unhealthy.
	// Require full health before any maintenance write, then reassert it here.
	// Never borrow node labels: failure cleanup must not remove an in-use client.
	if err = e.poll(ctx, "database health before hibernation", func(c context.Context) (bool, error) {
		clusters, err := e.c.list(c, "clusters.postgresql.cnpg.io", "*")
		if err != nil {
			return false, err
		}
		if err = preserved(j.Clusters, clusters); err != nil {
			return false, err
		}
		byKey, _ := index(clusters)
		for _, old := range j.Clusters {
			v := byKey[key(old)]
			if !condition(v, "Ready") || num(v, "status", "readyInstances") != num(v, "spec", "instances") || str(v, "status", "phase") != "Cluster in healthy state" || str(v, "status", "currentPrimary") != str(old, "status", "currentPrimary") || str(v, "status", "targetPrimary") != str(v, "status", "currentPrimary") {
				return false, nil
			}
		}
		for _, w := range append([]workload{{j.Release, j.Deployment}}, j.Extra...) {
			d, err := e.c.read(c, "deployment", str(w.Deployment, "metadata", "namespace"), str(w.Deployment, "metadata", "name"))
			if err != nil {
				return false, err
			}
			if str(d, "metadata", "uid") != str(w.Deployment, "metadata", "uid") || !reflect.DeepEqual(val(d, "spec"), val(w.Deployment, "spec")) {
				return false, errors.New("storage-consumer identity changed")
			}
			if num(d, "status", "readyReplicas") != num(d, "spec", "replicas") {
				return false, nil
			}
		}
		replicas, err := e.c.list(c, "replicas.longhorn.io", "longhorn-system")
		if err != nil {
			return false, err
		}
		for _, r := range replicas {
			if strings.HasPrefix(str(r, "spec", "nodeID"), "autoscale-") {
				return false, errors.New("compute-worker replica storage exclusion changed")
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	// Journal before every write; resourceVersion+UID tests prevent race adoption.
	resources := []object{p.release}
	for _, w := range p.extra {
		resources = append(resources, w.Release)
	}
	resources = append(resources, p.deployment)
	for _, w := range p.extra {
		resources = append(resources, w.Deployment)
	}
	resources = append(resources, p.clusters...)
	for _, old := range resources {
		if err = e.patchOwned(ctx, old, false); err != nil {
			return err
		}
	}
	if err = e.poll(ctx, "all-volume detachment", func(c context.Context) (bool, error) { return e.detached(c, j) }); err != nil {
		return err
	}
	j, o, err := e.ownerJournal(ctx)
	if err != nil {
		return err
	}
	j.Phase = "detached"
	j.PublicationNotStarted = false // The following publication may be partial.
	return e.writeJournal(ctx, j, o)
}

func (e engine) restore(ctx context.Context) error {
	all, err := e.c.list(ctx, "configmaps", journalNS)
	if err != nil {
		return err
	}
	exists := false
	for _, o := range all {
		if str(o, "metadata", "name") == journalName {
			exists = true
		}
	}
	if !exists {
		return nil
	}
	j, o, err := e.readJournal(ctx)
	if err != nil {
		return err
	}
	if j.Phase == "complete" {
		return nil
	}
	if j.Holder != e.holder || j.Source != e.source {
		return errors.New("restoration refuses a foreign journal")
	}
	if e.publicationNotStarted && !j.PublicationNotStarted {
		// Persist this positive step-outcome proof before releasing resources.
		// It is never inferred from a failed publication or the current selector.
		j.PublicationNotStarted = true
		if err = e.writeJournal(ctx, j, o); err != nil {
			return err
		}
		j, o, err = e.ownerJournal(ctx)
		if err != nil {
			return err
		}
	}
	// Publication can trigger asynchronous selector reconciliation even when
	// after-publish fails. Never reattach consumers during that transition.
	// A prepared journal (or positively skipped publication) has not exposed
	// the candidate and can restore against the unchanged old selector.
	if j.Phase != "restored" {
		safe := e.clientsReady
		if j.PublicationNotStarted {
			safe = func(c context.Context) (bool, error) {
				setting, err := e.c.read(c, "settings.longhorn.io", "longhorn-system", "system-managed-components-node-selector")
				if err != nil {
					return false, err
				}
				if val(setting, "value") != restrictedSelector {
					return e.clientsReady(c)
				}
				return true, nil
			}
		}
		if err = e.poll(ctx, "safe consumer restoration", safe); err != nil {
			return err
		}
	}
	var failures []error
	// Resume every owned resource even if one restoration fails. Never overwrite
	// an operator/maintainer's intervening spec or another run's annotation.
	resources := append([]object{}, j.Clusters...)
	resources = append(resources, j.Deployment)
	for _, w := range j.Extra {
		resources = append(resources, w.Deployment)
	}
	resources = append(resources, j.Release)
	for _, w := range j.Extra {
		resources = append(resources, w.Release)
	}
	for _, old := range resources {
		restorer := e
		restorer.holder = resourceHolder(j)
		err := restorer.patchOwned(ctx, old, true)
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	j.Phase = "restored"
	return e.writeJournal(ctx, j, o)
}

func (e engine) afterPublish(ctx context.Context) error {
	all, err := e.c.list(ctx, "configmaps", journalNS)
	if err != nil {
		return err
	}
	exists := false
	for _, o := range all {
		if str(o, "metadata", "name") == journalName {
			exists = true
		}
	}
	if !exists {
		return nil
	}
	j, _, err := e.readJournal(ctx)
	if err != nil {
		return err
	}
	if j.Phase == "complete" {
		return nil
	}
	if j.Phase == "restored" && j.Holder == e.holder && j.Source == e.source {
		if err = e.provenReleased(ctx, j); err != nil {
			return err
		}
		return e.poll(ctx, "restored client placement", e.clientsReady)
	}
	if j.Holder != e.holder || j.Source != e.source || j.Phase != "detached" {
		return errors.New("post-publication requires this run's detached journal")
	}
	ok, err := e.detached(ctx, j)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("volumes reattached before selector migration")
	}
	// Only the infrastructure-controller layer needs to converge while the
	// explicitly owned database resources are hibernated. Full Flux reconciliation
	// and all original health gates run after their restoration, never skipped.
	controller, err := e.c.read(ctx, "kustomization", "flux-system", "infrastructure-controllers")
	if err != nil {
		return err
	}
	request := tests(controller)
	if val(controller, "metadata", "annotations") == nil {
		request = append(request, object{"op": "add", "path": "/metadata/annotations", "value": object{}})
	}
	request = append(request, object{"op": "add", "path": "/metadata/annotations/reconcile.fluxcd.io~1requestedAt", "value": e.now().UTC().Format(time.RFC3339Nano)})
	err = e.c.patch(ctx, "kustomization", controller, request)
	if err != nil {
		return err
	}
	if err = e.poll(ctx, "client rollout", e.clientsReady); err != nil {
		return err
	}
	return e.restore(ctx)
}

func (e engine) verify(ctx context.Context) error {
	all, err := e.c.list(ctx, "configmaps", journalNS)
	if err != nil {
		return err
	}
	exists := false
	for _, o := range all {
		if str(o, "metadata", "name") == journalName {
			exists = true
		}
	}
	if !exists {
		return nil
	}
	j, o, err := e.readJournal(ctx)
	if err != nil {
		return err
	}
	if j.Phase == "complete" {
		return nil
	}
	if j.Holder != e.holder || j.Source != e.source || j.Phase != "restored" {
		return errors.New("maintenance restoration was not proven")
	}
	proof := func(c context.Context) (bool, error) {
		s, err := e.collect(c)
		if err != nil {
			return false, err
		}
		if err = preserved(j.Claims, s.claims); err != nil {
			return false, err
		}
		if err = preserved(j.PVs, s.pvs); err != nil {
			return false, err
		}
		for _, claim := range s.claims {
			for _, old := range j.Claims {
				if key(claim) == key(old) && str(claim, "status", "phase") != "Bound" {
					return false, errors.New("retained claim is no longer bound")
				}
			}
		}
		volumes, err := index(s.volumes)
		if err != nil {
			return false, err
		}
		for _, pv := range s.pvs {
			if str(pv, "spec", "csi", "driver") != "driver.longhorn.io" {
				continue
			}
			v := volumes["longhorn-system/"+str(pv, "spec", "csi", "volumeHandle")]
			if str(pv, "status", "phase") == "Released" {
				if str(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" || str(v, "status", "state") != "detached" {
					return false, errors.New("retired backing is not retained and detached")
				}
			} else if str(pv, "status", "phase") != "Bound" {
				return false, errors.New("persistent-volume binding is unknown")
			}
			if str(v, "status", "robustness") == "faulted" || (str(v, "status", "state") != "attached" && str(v, "status", "state") != "detached") || (str(v, "status", "state") == "attached" && str(v, "status", "robustness") != "healthy") {
				return false, nil
			}
		}
		if err = backingPreserved(j.Volumes, s.volumes); err != nil {
			return false, err
		}
		if err = preserved(j.Clusters, s.clusters); err != nil {
			return false, err
		}
		byKey, _ := index(s.clusters)
		for _, old := range j.Clusters {
			current := byKey[key(old)]
			if !unpaused(current) || num(current, "status", "readyInstances") != num(current, "spec", "instances") || !condition(current, "Ready") || !condition(current, "ContinuousArchiving") || str(current, "status", "systemID") != str(old, "status", "systemID") || str(current, "status", "currentPrimary") != str(old, "status", "currentPrimary") || str(current, "status", "targetPrimary") != str(current, "status", "currentPrimary") {
				return false, nil
			}
		}
		if num(s.deployment, "status", "readyReplicas") != 1 || !reflect.DeepEqual(val(s.deployment, "spec"), val(j.Deployment, "spec")) || str(s.deployment, "metadata", "uid") != str(j.Deployment, "metadata", "uid") || str(s.release, "metadata", "uid") != str(j.Release, "metadata", "uid") || !unpaused(s.deployment) || !unpaused(s.release) || !reflect.DeepEqual(val(s.release, "spec"), val(j.Release, "spec")) {
			return false, nil
		}
		if len(s.extra) != len(j.Extra) {
			return false, errors.New("storage-consumer coverage changed")
		}
		for i, w := range s.extra {
			old := j.Extra[i]
			if str(w.Deployment, "metadata", "uid") != str(old.Deployment, "metadata", "uid") || str(w.Release, "metadata", "uid") != str(old.Release, "metadata", "uid") || !reflect.DeepEqual(val(w.Deployment, "spec"), val(old.Deployment, "spec")) || !reflect.DeepEqual(val(w.Release, "spec"), val(old.Release, "spec")) || !unpaused(w.Deployment) || !unpaused(w.Release) || num(w.Deployment, "status", "readyReplicas") != num(w.Deployment, "spec", "replicas") {
				return false, nil
			}
		}
		return e.clientsReady(c)
	}
	for sample := 0; sample < 2; sample++ {
		if sample > 0 {
			e.sleep(10 * time.Second)
		}
		if err = e.poll(ctx, "database redundancy", proof); err != nil {
			return err
		}
	}
	j.Phase = "complete"
	return e.writeJournal(ctx, j, o)
}

func armed() (bool, error) {
	b, err := os.ReadFile("k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml")
	if err != nil {
		return false, err
	}
	var o object
	if err = yaml.Unmarshal(b, &o); err != nil {
		return false, err
	}
	if val(o, "spec", "values", "defaultSettings", "systemManagedComponentsNodeSelector") != "" {
		return false, nil
	}
	// This migration belongs only to the audited chart, not every future release.
	if str(o, "spec", "chart", "spec", "version") != "1.13.0" {
		return false, errors.New("empty-selector maintenance requires the audited Longhorn chart version")
	}
	if val(o, "spec", "values", "defaultSettings", "createDefaultDiskLabeledNodes") != true || !reflect.DeepEqual(val(o, "spec", "values", "longhornManager", "nodeSelector"), object{}) {
		return false, errors.New("desired selector migration differs from the reviewed chart contract")
	}
	return true, nil
}
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "one maintenance phase required")
		os.Exit(2)
	}
	phase := os.Args[1]
	if phase != "before-publish" && phase != "after-publish" && phase != "restore" && phase != "verify" {
		os.Exit(2)
	}
	a, err := armed()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !a && phase != "before-publish" {
		return
	}
	source := os.Getenv("GITHUB_SHA")
	run := os.Getenv("GITHUB_RUN_ID")
	attempt := os.Getenv("GITHUB_RUN_ATTEMPT")
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(run) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(attempt) {
		fmt.Fprintln(os.Stderr, "protected production workflow identity is required")
		os.Exit(1)
	}
	cmd := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "kubectl", append([]string{"--context", "admin@prod", "--request-timeout=20s"}, args...)...)
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			if strings.Contains(stderr.String(), "(NotFound)") {
				return nil, errResourceMissing
			}
			if strings.Contains(stderr.String(), "(Conflict)") {
				return nil, fmt.Errorf("%w: %w", errPatchConflict, err)
			}
			return nil, fmt.Errorf("maintenance %s failed: %w", args[0], err)
		}
		return stdout.Bytes(), nil
	}
	e := engine{c: client{command: cmd}, holder: run + "." + attempt, source: source, now: time.Now, sleep: time.Sleep, wait: 20 * time.Minute, publicationNotStarted: os.Getenv("LONGHORN_PUBLICATION_NOT_STARTED") == "true", previousFinished: finishedAttempt, revisionSafe: func(ctx context.Context) error { return isolatedRevision(ctx, source) }}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	if !a {
		// This guard is staged on main BEFORE the selector repair. Recovery and
		// later speculative deploys must not republish the old selector once the
		// reviewed migration is live. A refused rollback is HOLD, never green.
		b, readErr := os.ReadFile("k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml")
		var desired object
		if readErr != nil || yaml.Unmarshal(b, &desired) != nil {
			fmt.Fprintln(os.Stderr, "cannot establish desired client placement")
			os.Exit(1)
		}
		selector := val(desired, "spec", "values", "defaultSettings", "systemManagedComponentsNodeSelector")
		if selector != "" {
			live, readErr := e.c.read(ctx, "settings.longhorn.io", "longhorn-system", "system-managed-components-node-selector")
			if selector != restrictedSelector || readErr != nil || val(live, "value") != selector {
				fmt.Fprintln(os.Stderr, "Longhorn client maintenance HOLD: unsafe selector rollback refused")
				os.Exit(1)
			}
			if err = e.legacyPublicationSafe(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "Longhorn client maintenance HOLD:", err)
				os.Exit(1)
			}
		}
		return
	}
	switch phase {
	case "before-publish":
		err = e.prepare(ctx)
	case "after-publish":
		err = e.afterPublish(ctx)
	case "restore":
		err = e.restore(ctx)
	case "verify":
		err = e.verify(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Longhorn client maintenance HOLD:", err)
		os.Exit(1)
	}
	fmt.Println("PASS: Longhorn client maintenance phase", phase)
}
