package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

type activationAPI struct {
	t                   *testing.T
	s                   inventory
	managers, databases []object
	baseline, record    object
	writes              int
	lostResponse        bool
	priorLive           bool
	preparations        map[string]object
	preparationFailed   bool
}

// newActivationAPI models complete fixed-roster reads and successful native
// preparation, with writes restricted to the activation journal and raw nodes.
func newActivationAPI(t *testing.T) (*activationAPI, temporaryV2Activation) {
	t.Helper()
	s, managers := v2ReadinessFixture()
	for i, node := range s.nodes {
		node["metadata"].(object)["uid"] = fmt.Sprintf("node-%d", i)
		if strings.HasPrefix(text(node, "metadata", "name"), "prod-worker-") {
			node["metadata"].(object)["labels"].(object)["node.kubernetes.io/instance-type"] = "cx43"
		}
		node["spec"] = object{"providerID": fmt.Sprintf("hcloud://%d", i+100), "unschedulable": false}
		node["status"].(object)["nodeInfo"] = object{"systemUUID": fmt.Sprintf("hardware-%d", i), "bootID": fmt.Sprintf("12345678-1234-4234-8234-%012x", i+1), "osImage": "Talos fixture", "architecture": "amd64"}
		node["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": fmt.Sprintf("10.0.99.%d", i+10)}}
	}
	for _, lh := range s.storageNodes {
		lh["metadata"].(object)["namespace"] = "longhorn-system"
		lh["spec"].(object)["allowScheduling"] = true
		lh["spec"].(object)["evictionRequested"] = false
		if strings.HasPrefix(text(lh, "metadata", "name"), "autoscale-longhorn-") {
			lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
			lh["status"] = object{"diskStatus": object{}}
		}
	}
	f := &activationAPI{t: t, s: s, managers: managers, baseline: unusedBaselineFixture(t, s)}
	for _, selected := range []struct{ namespace, name string }{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"}} {
		db := resource(selected.name)
		db["metadata"].(object)["namespace"] = selected.namespace
		db["spec"] = object{"instances": float64(3)}
		db["status"] = object{"readyInstances": float64(3), "systemID": "123456789", "phase": "Cluster in healthy state"}
		f.databases = append(f.databases, db)
	}
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	f.preparations = map[string]object{}
	for ordinal, selection := range []string{"temporary-v1-1", "temporary-v1-2"} {
		name, _, err := resolveTemporaryInventoryTarget(selection, s.nodes, pools)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range s.nodes {
			if text(node, "metadata", "name") != name {
				continue
			}
			var lh object
			for _, storage := range s.storageNodes {
				if text(storage, "metadata", "name") == name {
					lh = storage
				}
			}
			binding := temporaryV2Binding{Selection: selection, Name: name, NodeUID: text(node, "metadata", "uid"), ProviderID: text(node, "spec", "providerID"),
				MachineUUID: text(node, "status", "nodeInfo", "systemUUID"), LonghornUID: text(lh, "metadata", "uid")}
			cm := temporaryPreparedFixture(t, binding)
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), `"ownerRun":"7777"`, fmt.Sprintf(`"ownerRun":"%d"`, 7777+ordinal), 1)
			f.preparations[selection] = cm
		}
	}
	a := temporaryV2Activation{c: client{context: "fixture", run: f.run}, source: strings.Repeat("a", 40), owner: "456", pools: pools}
	a.reader = func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
		for _, node := range f.s.nodes {
			addresses, _ := value(node, "status", "addresses").([]any)
			if len(addresses) == 1 && text(addresses[0].(object), "address") == address {
				boot := text(node, "status", "nodeInfo", "bootID")
				return &activationBootReader{before: boot, after: boot}, func() error { return nil }, nil
			}
		}
		return nil, nil, errTemporaryActivation
	}
	return f, a
}

// run enforces the production command boundary and identity-preconditioned
// writes, including an uncertain response after an actual disk registration.
func (f *activationAPI) run(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.t.Helper()
	if program == "git" && reflect.DeepEqual(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"}) {
		return []byte(strings.Repeat("a", 40) + "\trefs/heads/main\n"), nil
	}
	if program == "gh" && len(args) == 2 && args[0] == "api" {
		source := strings.Repeat("1", 40)
		status := "completed"
		conclusion := "success"
		if strings.Contains(args[1], "/7777/") || strings.Contains(args[1], "/7778/") {
			if f.preparationFailed {
				conclusion = "failure"
			}
		} else if !strings.Contains(args[1], "/5678/") {
			source = strings.Repeat("a", 40)
			if f.priorLive {
				status = "in_progress"
			}
		}
		return json.Marshal(object{"status": status, "conclusion": conclusion, "head_sha": source, "head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
	}
	if program == "talosctl" && len(args) == 5 && args[1] == "get" && args[4] == "--output=json" {
		for i, node := range f.s.nodes {
			if args[0] != "--nodes="+fmt.Sprintf("10.0.99.%d", i+10) {
				continue
			}
			if args[2] == "systeminformation" && args[3] == "systeminformation" {
				return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": text(node, "status", "nodeInfo", "systemUUID")}})
			}
			raw, eph := rawFixture()
			raw["spec"].(object)["partitionUUID"] = fmt.Sprintf("12345678-1234-4234-8234-%012x", i+100)
			eph["spec"].(object)["uuid"] = fmt.Sprintf("ephemeral-%d", i)
			if args[2] == "volumestatus" && args[3] == "r-longhorn" {
				return json.Marshal(raw)
			}
			if args[2] == "volumestatus" && args[3] == "EPHEMERAL" {
				return json.Marshal(eph)
			}
		}
	}
	if program != "kubectl" || len(args) < 4 || args[0] != "--context=fixture" || args[1] != "--request-timeout=20s" {
		f.t.Fatalf("activation escaped its native scope: %s %v", program, args)
	}
	args = args[2:]
	if args[0] == "get" {
		kind := args[1]
		if args[2] == "--output=json" {
			collections := map[string][]object{"nodes": f.s.nodes, "nodes.longhorn.io": f.s.storageNodes, "volumes.longhorn.io": f.s.volumes, "replicas.longhorn.io": f.s.replicas, "engines.longhorn.io": f.s.engines, "orphans.longhorn.io": f.s.orphans, "pods": f.s.pods, "instancemanagers.longhorn.io": f.managers, "clusters.postgresql.cnpg.io": f.databases}
			items, ok := collections[kind]
			if !ok {
				f.t.Fatalf("unexpected collection %s", kind)
			}
			if items == nil {
				items = []object{}
			}
			return json.Marshal(object{"items": items})
		}
		name := args[2]
		if kind == "configmap" {
			for selection, prepared := range f.preparations {
				if name == "longhorn-v2-prepare-"+selection {
					if prepared == nil {
						return nil, nil
					}
					return json.Marshal(prepared)
				}
			}
			if name == temporaryActivationJournal {
				if f.record == nil {
					return nil, nil
				}
				return json.Marshal(f.record)
			}
			if name == "longhorn-v2-benchmark-v1" {
				return json.Marshal(f.baseline)
			}
		}
		if kind == "settings.longhorn.io" {
			settings := map[string]string{"v1-data-engine": "true", "v2-data-engine": "true", "storage-over-provisioning-percentage": "100", "storage-minimal-available-percentage": "25", "guaranteed-instance-manager-cpu": `{"v1":"6","v2":"25"}`, "data-engine-cpu-mask": `{"v2":"0x3"}`, "data-engine-number-of-cpu-cores": `{"v2":"0"}`, "data-engine-cpu-isolation-enabled": `{"v2":"true"}`, "data-engine-interrupt-mode-enabled": `{"v2":"false"}`, "data-engine-hugepage-enabled": `{"v2":"true"}`, "data-engine-memory-size": `{"v2":"2048"}`}
			setting, ok := settings[name]
			if !ok {
				f.t.Fatalf("unexpected setting %s", name)
			}
			o := resource(name)
			o["value"] = setting
			return json.Marshal(o)
		}
		for _, o := range append(append([]object{}, f.s.nodes...), f.s.storageNodes...) {
			if text(o, "metadata", "name") == name && (kind == "node" && text(o, "metadata", "namespace") == "" || kind == "nodes.longhorn.io" && text(o, "metadata", "namespace") == "longhorn-system") {
				return json.Marshal(o)
			}
		}
	}
	if args[0] == "create" {
		if f.record != nil {
			return nil, errConflict
		}
		var created object
		if json.Unmarshal(input, &created) != nil || text(created, "kind") != "ConfigMap" || text(created, "metadata", "name") != temporaryActivationJournal || text(created, "metadata", "namespace") != "longhorn-system" {
			f.t.Fatal("activation created a resource outside its fixed journal")
		}
		created["metadata"].(object)["uid"] = "activation-journal-uid"
		created["metadata"].(object)["resourceVersion"] = "1"
		f.record = created
		return json.Marshal(created)
	}
	if args[0] == "patch" {
		var target object
		if args[1] == "configmap" && args[2] == temporaryActivationJournal {
			target = f.record
		}
		if args[1] == "nodes.longhorn.io" {
			for _, lh := range f.s.storageNodes {
				if text(lh, "metadata", "name") == args[2] && strings.HasPrefix(args[2], "autoscale-longhorn-") {
					target = lh
				}
			}
		}
		var changes []object
		if target == nil || json.Unmarshal(input, &changes) != nil || len(changes) < 3 || !reflect.DeepEqual(changes[:2], identityTests(target)) {
			f.t.Fatal("activation write lost its exact native identity or exceeded temporary scope")
		}
		for _, change := range changes[2:] {
			parts := strings.Split(strings.TrimPrefix(text(change, "path"), "/"), "/")
			parent := target
			for _, part := range parts[:len(parts)-1] {
				parent = parent[part].(object)
			}
			parent[strings.ReplaceAll(parts[len(parts)-1], "~1", "/")] = change["value"]
		}
		version, _ := strconv.Atoi(text(target, "metadata", "resourceVersion"))
		target["metadata"].(object)["resourceVersion"] = strconv.Itoa(version + 1)
		if args[1] == "nodes.longhorn.io" {
			f.writes++
			target["status"] = object{"conditions": readyConditions(), "diskStatus": object{"longhorn-v2": object{"diskUUID": "disk-" + args[2], "conditions": readyConditions()}}}
			if f.lostResponse {
				f.lostResponse = false
				return nil, errors.New("response lost after registration")
			}
		}
		return nil, nil
	}
	f.t.Fatalf("unexpected activation command: %s %v", program, args)
	return nil, nil
}

// Registered disks must remain eligible to the independent publication reader;
// replaying the completed operation must not repeat a disk write.
func TestTemporaryActivationRegistersOnlyThreeJournalOwnedRawDisks(t *testing.T) {
	f, a := newActivationAPI(t)
	if err := a.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, record, err := a.read(t.Context())
	if err != nil || !record.Completed || f.writes != 3 {
		t.Fatal("activation did not prove all three owned raw disks:", err)
	}
	if err = a.run(t.Context()); err != nil || f.writes != 3 {
		t.Fatal("completed activation repeated a registration:", err)
	}
	for _, node := range f.s.nodes {
		if text(node, "metadata", "labels", longhornmigration.PoolLabel) == "" {
			continue
		}
		var storage object
		for _, lh := range f.s.storageNodes {
			if text(lh, "metadata", "name") == text(node, "metadata", "name") {
				storage = lh
			}
		}
		if eligible, err := longhornmigration.Eligible(poolConfigFixture(), node, storage, f.record); !eligible || err != nil {
			t.Fatal("actual registration output blocks the shared publication gate:", err)
		}
	}
}

// No journal or disk may be written without successful native preparation.
func TestTemporaryActivationHoldsBeforeWritesWithoutSuccessfulPreparation(t *testing.T) {
	for _, mode := range []string{"missing preparation", "failed preparation", "unfinished preparation"} {
		t.Run(mode, func(t *testing.T) {
			f, a := newActivationAPI(t)
			switch mode {
			case "missing preparation":
				f.preparations["temporary-v1-1"] = nil
			case "failed preparation":
				f.preparationFailed = true
			case "unfinished preparation":
				cm := f.preparations["temporary-v1-1"]
				cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), `"phase":"prepared"`, `"phase":"reset-requested"`, 1)
			}
			if err := a.run(t.Context()); err == nil || f.writes != 0 || f.record != nil {
				t.Fatal("activation wrote before successful native preparation was bound:", err)
			}
		})
	}
}

// Only a later owner may resume an uncertain owned patch; an unrelated storage
// owner's annotation cannot be adopted even when the desired disk matches.
func TestTemporaryActivationResumesItsUncertainWriteWithoutAdoptingForeignStorage(t *testing.T) {
	f, a := newActivationAPI(t)
	f.lostResponse = true
	if a.run(t.Context()) == nil || f.writes != 1 {
		t.Fatal("uncertain registration was accepted or retried")
	}
	a.owner = "789"
	if err := a.run(t.Context()); err != nil || f.writes != 3 {
		t.Fatal("owned partial activation could not safely resume:", err)
	}
	f, a = newActivationAPI(t)
	f.s.storageNodes[3]["metadata"].(object)["annotations"] = object{temporaryActivationOwner: "foreign"}
	if a.run(t.Context()) == nil || f.writes != 0 {
		t.Fatal("unowned storage was adopted")
	}
}

// Completed and interrupted registration survive a normal host reboot without
// replaying an owned disk write or rewriting the original observation evidence.
func TestTemporaryActivationPreservesStorageIdentityAcrossReboot(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			f, a := newActivationAPI(t)
			f.lostResponse = interrupted
			if err := a.run(t.Context()); (err != nil) != interrupted {
				t.Fatal("initial registration did not reach the selected boundary:", err)
			}
			_, original, err := a.read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for i, node := range f.s.nodes {
				if !strings.HasPrefix(text(node, "metadata", "name"), "autoscale-longhorn-") {
					continue
				}
				info := value(node, "status", "nodeInfo").(object)
				info["bootID"] = fmt.Sprintf("12345678-1234-4234-8234-%012x", i+500)
				info["osImage"] = "Talos upgraded fixture"
			}
			a.owner = "789"
			if err = a.run(t.Context()); err != nil || f.writes != 3 {
				t.Fatal("normal reboot invalidated intact prepared storage:", err)
			}
			if err = a.accepted(t.Context()); err != nil {
				t.Fatal("completed storage was not accepted after reboot:", err)
			}
			_, recovered, err := a.read(t.Context())
			if err != nil || !reflect.DeepEqual(original.Targets, recovered.Targets) {
				t.Fatal("recovery rewrote the original registration observations:", err)
			}
		})
	}
}

// Matching but unreviewed manager images must fail before disk registration.
func TestTemporaryActivationRejectsUnqualifiedClientsBeforeRegisteringDisks(t *testing.T) {
	f, a := newActivationAPI(t)
	f.managers[0]["spec"].(object)["image"] = "example.invalid/unreviewed:latest"
	value(f.s.pods[0], "spec", "containers").([]any)[0].(object)["image"] = "example.invalid/unreviewed:latest"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if a.run(ctx) == nil || f.writes != 0 {
		t.Fatal("unqualified V2 clients reached raw-disk registration")
	}
}

// Read-only acceptance must revalidate the completed workflow and current
// storage identities rather than trust a cached completion bit.
func TestCompletedTemporaryActivationRequiresCurrentOwnedStorage(t *testing.T) {
	f, a := newActivationAPI(t)
	if a.accepted(t.Context()) == nil || f.writes != 0 {
		t.Fatal("an unregistered pool authorized the canary")
	}
	for _, mode := range []string{"valid", "journal replaced", "worker changed", "workflow live", "disk fenced", "preparation replaced"} {
		t.Run(mode, func(t *testing.T) {
			f, a := newActivationAPI(t)
			if err := a.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "journal replaced":
				f.record["metadata"].(object)["uid"] = "replacement"
			case "worker changed":
				f.s.nodes[3]["spec"].(object)["providerID"] = "hcloud://999"
			case "workflow live":
				f.priorLive = true
			case "disk fenced":
				f.s.storageNodes[3]["spec"].(object)["allowScheduling"] = false
			case "preparation replaced":
				f.preparations["temporary-v1-1"]["metadata"].(object)["uid"] = "replacement-preparation"
			}
			before, _ := json.Marshal(f.record)
			err := a.accepted(t.Context())
			after, _ := json.Marshal(f.record)
			if (err == nil) != (mode == "valid") || f.writes != 3 || string(before) != string(after) {
				t.Fatal("canary acceptance ignored changed evidence or mutated the plan:", err)
			}
		})
	}
}
