package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

type fakeAPI struct {
	objects       map[string][]object
	writes        int
	failWrite     int
	migrated      bool
	blockDetach   bool
	failReconcile bool
}

func fakeKey(kind, ns, name string) string { return kind + ":" + ns + ":" + name }
func fakeFixture() *fakeAPI {
	s := fixture()
	a := &fakeAPI{objects: map[string][]object{
		fakeKey("clusters.postgresql.cnpg.io", "*", ""):          s.clusters,
		fakeKey("pods", "*", ""):                                 s.pods,
		fakeKey("persistentvolumeclaims", "*", ""):               s.claims,
		fakeKey("persistentvolumes", "", ""):                     s.pvs,
		fakeKey("volumes.longhorn.io", "longhorn-system", ""):    s.volumes,
		fakeKey("backups.postgresql.cnpg.io", "*", ""):           s.backups,
		fakeKey("replicasets", "*", ""):                          s.replicasets,
		fakeKey("helmrelease", "crossview", "crossview"):         {s.release},
		fakeKey("deployment", "crossview", "crossview-postgres"): {s.deployment},
		fakeKey("configmaps", journalNS, ""):                     {},
		fakeKey("horizontalpodautoscalers", "*", ""):             {},
		fakeKey("replicas.longhorn.io", "longhorn-system", ""):   {},
	}}
	for _, w := range s.extra {
		a.objects[fakeKey("helmrelease", str(w.Release, "metadata", "namespace"), str(w.Release, "metadata", "name"))] = []object{w.Release}
		a.objects[fakeKey("deployment", str(w.Deployment, "metadata", "namespace"), str(w.Deployment, "metadata", "name"))] = []object{w.Deployment}
	}
	a.objects[fakeKey("kustomization", "flux-system", "infrastructure-controllers")] = []object{resource("Kustomization", "flux-system", "infrastructure-controllers")}
	for _, c := range s.clusters {
		c["status"].(object)["readyInstances"] = val(c, "spec", "instances")
		c["status"].(object)["phase"] = "Cluster in healthy state"
		c["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}
		a.objects[fakeKey("clusters.postgresql.cnpg.io", str(c, "metadata", "namespace"), str(c, "metadata", "name"))] = []object{c}
	}
	for _, v := range s.volumes {
		v["spec"] = object{"size": "1024", "dataEngine": "v1", "frontend": "blockdev", "nodeID": "old-node"}
	}
	for _, name := range []string{"system-managed-components-node-selector", "create-default-disk-labeled-nodes"} {
		o := resource("Setting", "longhorn-system", name)
		o["value"] = "false"
		if name == "system-managed-components-node-selector" {
			o["value"] = restrictedSelector
		}
		a.objects[fakeKey("settings.longhorn.io", "longhorn-system", name)] = []object{o}
	}
	for _, name := range []string{"longhorn-manager", "longhorn-csi-plugin", "engine-image-test"} {
		d := resource("DaemonSet", "longhorn-system", name)
		d["metadata"].(object)["generation"] = 1
		d["spec"] = object{"template": object{"spec": object{"nodeSelector": object{"node.longhorn.io/create-default-disk": "true"}}}}
		d["status"] = object{"desiredNumberScheduled": 3, "numberReady": 3, "updatedNumberScheduled": 3, "observedGeneration": 1}
		if name == "engine-image-test" {
			d["metadata"].(object)["labels"] = object{"longhorn.io/component": "engine-image"}
			a.objects[fakeKey("daemonsets", "longhorn-system", "")] = []object{d}
		}
		a.objects[fakeKey("daemonset", "longhorn-system", name)] = []object{d}
	}
	for n := 0; n < 3; n++ {
		name := fmt.Sprintf("worker-%d", n)
		node := resource("Node", "", name)
		node["metadata"].(object)["labels"] = object{"node.longhorn.io/create-default-disk": "true"}
		node["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
		a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], node)
		csi := resource("CSINode", "", name)
		csi["spec"] = object{"drivers": []any{object{"name": "driver.longhorn.io"}}}
		a.objects[fakeKey("csinodes", "", "")] = append(a.objects[fakeKey("csinodes", "", "")], csi)
	}
	a.objects[fakeKey("nodes.longhorn.io", "longhorn-system", "")] = []object{}
	admissionFixture(a)
	scheduledFixture(a)
	controllerFixture(a)
	a.objects[fakeKey("backups.velero.io", "velero", "")] = []object{}
	a.objects[fakeKey("schedules.velero.io", "velero", "")] = []object{}
	r := resource("HelmRelease", "velero", "velero")
	r["metadata"].(object)["labels"] = object{"kustomize.toolkit.fluxcd.io/name": "infrastructure-controllers", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
	r["spec"] = object{"suspend": false}
	a.objects[fakeKey("helmrelease", "velero", "velero")] = []object{r}
	return a
}
func (a *fakeAPI) stopped() bool {
	for _, c := range a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")] {
		if str(c, "metadata", "annotations", hibernateKey) != "on" {
			return false
		}
	}
	for _, w := range append([]workload{{resource("HelmRelease", "crossview", "crossview"), resource("Deployment", "crossview", "crossview-postgres")}}, fixture().extra...) {
		if num(a.objects[fakeKey("deployment", str(w.Deployment, "metadata", "namespace"), str(w.Deployment, "metadata", "name"))][0], "spec", "replicas") != 0 {
			return false
		}
	}
	return true
}
func fakeSet(o object, path string, v any, remove bool) {
	keys := strings.Split(strings.TrimPrefix(path, "/"), "/")
	m := o
	for _, raw := range keys[:len(keys)-1] {
		k := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		if m[k] == nil {
			m[k] = object{}
		}
		m = m[k].(map[string]any)
	}
	k := strings.ReplaceAll(strings.ReplaceAll(keys[len(keys)-1], "~1", "/"), "~0", "~")
	if remove {
		delete(m, k)
	} else {
		m[k] = v
	}
}

// command models the fixed maintenance commands, including stdin patch preconditions and injected failures.
func (a *fakeAPI) command(_ context.Context, input []byte, args ...string) ([]byte, error) {
	if args[0] == "patch" && args[1] == "kustomization" {
		if a.failReconcile {
			return nil, errors.New("controller layer failed")
		}
		if !a.stopped() {
			return nil, errors.New("attempted selector migration with running databases")
		}
		a.migrated = true
		for key, objects := range a.objects {
			if strings.HasPrefix(key, "settings.longhorn.io:") {
				for _, o := range objects {
					if str(o, "metadata", "name") == "system-managed-components-node-selector" {
						o["value"] = ""
					} else {
						o["value"] = "true"
					}
				}
			}
			if strings.HasPrefix(key, "daemonset:") || strings.HasPrefix(key, "daemonsets:") {
				for _, o := range objects {
					fakeSet(o, "/spec/template/spec/nodeSelector", object{}, false)
				}
			}
		}
		return []byte("reconciled"), nil
	}
	if args[0] == "create" {
		if len(args) > 1 && args[1] == "--dry-run=server" {
			var o object
			if err := json.Unmarshal(input, &o); err != nil {
				return nil, err
			}
			o["spec"].(object)["allowScheduling"] = false
			o["spec"].(object)["evictionRequested"] = true
			return json.Marshal(o)
		}
		a.writes++
		if a.writes == a.failWrite {
			return nil, errors.New("injected create failure")
		}
		var o object
		if err := json.Unmarshal(input, &o); err != nil {
			return nil, err
		}
		o["metadata"].(object)["uid"] = "journal-uid"
		o["metadata"].(object)["resourceVersion"] = "1"
		a.objects[fakeKey("configmaps", journalNS, "")] = []object{o}
		a.objects[fakeKey("configmap", journalNS, journalName)] = []object{o}
		return []byte("created"), nil
	}
	kind := args[1]
	name, ns := "", ""
	if len(args) > 2 && !strings.HasPrefix(args[2], "-") {
		name = args[2]
	}
	for i, arg := range args {
		if arg == "-A" {
			ns = "*"
		}
		if arg == "-n" {
			ns = args[i+1]
		}
	}
	objects, exists := a.objects[fakeKey(kind, ns, name)]
	if !exists {
		return nil, fmt.Errorf("unmodelled query %s", fakeKey(kind, ns, name))
	}
	if args[0] == "patch" {
		a.writes++
		if a.writes == a.failWrite {
			return nil, errors.New("injected patch failure")
		}
		var ops []object
		if !strings.Contains(strings.Join(args, " "), "--patch-file=/dev/stdin") {
			return nil, errors.New("patch stdin transport missing")
		}
		if err := json.Unmarshal(input, &ops); err != nil {
			return nil, err
		}
		o := objects[0]
		restartBefore := str(o, "spec", "template", "metadata", "annotations", quiesceKey)
		for _, op := range ops {
			if str(op, "path") == "/metadata/resourceVersion" && !reflect.DeepEqual(val(o, "metadata", "resourceVersion"), op["value"]) {
				return nil, errors.New("Error from server (Conflict): resourceVersion changed")
			}
		}
		for _, op := range ops {
			path := str(op, "path")
			switch str(op, "op") {
			case "test":
				keys := strings.Split(strings.TrimPrefix(path, "/"), "/")
				for i, k := range keys {
					keys[i] = strings.ReplaceAll(strings.ReplaceAll(k, "~1", "/"), "~0", "~")
				}
				if !reflect.DeepEqual(val(o, keys...), op["value"]) {
					return nil, errors.New("CAS failed")
				}
			case "add", "replace":
				fakeSet(o, path, op["value"], false)
			case "remove":
				fakeSet(o, path, nil, true)
			}
		}
		rv, _ := strconv.Atoi(str(o, "metadata", "resourceVersion"))
		o["metadata"].(object)["resourceVersion"] = strconv.Itoa(rv + 1)
		if kind == "clusters.postgresql.cnpg.io" {
			if str(o, "metadata", "annotations", hibernateKey) == "on" {
				o["status"].(object)["conditions"] = append(itemsAsAny(items(o, "status", "conditions")), object{"type": hibernateKey, "status": "True"})
			} else {
				o["status"].(object)["readyInstances"] = val(o, "spec", "instances")
				o["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}
			}
		}
		if kind == "deployment" && ns == "flux-system" && restartBefore != str(o, "spec", "template", "metadata", "annotations", quiesceKey) {
			fakeControllerRollout(a, o)
		}
		return []byte("patched"), nil
	}
	if a.stopped() && !a.blockDetach {
		if kind == "pods" {
			objects = []object{}
		}
		if kind == "volumes.longhorn.io" {
			for _, o := range objects {
				o["status"].(object)["state"] = "detached"
			}
		}
	}
	if name != "" {
		return json.Marshal(objects[0])
	}
	return json.Marshal(object{"metadata": object{}, "items": itemsAsAny(objects)})
}
func itemsAsAny(o []object) []any {
	a := []any{}
	for _, x := range o {
		a = append(a, x)
	}
	return a
}
func testEngine(a *fakeAPI) engine {
	now := time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)
	return engine{c: client{command: a.command}, holder: "100.1", source: strings.Repeat("a", 40), now: func() time.Time { return now }, sleep: func(d time.Duration) { now = now.Add(d) }, wait: 20 * time.Second, revisionSafe: func(context.Context) error { return nil }}
}

func TestFullMaintenanceAndEveryPartialPauseRestore(t *testing.T) {
	for fail := 0; fail <= 12; fail++ {
		t.Run(fmt.Sprintf("write-%d", fail), func(t *testing.T) {
			a := fakeFixture()
			a.failWrite = fail
			e := testEngine(a)
			ctx := context.Background()
			err := e.prepare(ctx)
			if fail == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if !a.stopped() {
					t.Fatal("databases not hibernated")
				}
				if err = e.afterPublish(ctx); err != nil {
					t.Fatal(err)
				}
				if !a.migrated {
					t.Fatal("clients not reconciled")
				}
				if err = e.verify(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("failed write was accepted")
			}
			if err = e.restore(ctx); err != nil {
				t.Fatal(err)
			}
			for _, c := range a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")] {
				if !unpaused(c) {
					t.Fatal("owned database pause stranded")
				}
			}
			if num(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "spec", "replicas") != 1 {
				t.Fatal("bundled database not restored")
			}
			if val(a.objects[fakeKey("helmrelease", "crossview", "crossview")][0], "spec", "suspend") != false {
				t.Fatal("Helm owner not restored")
			}
			for _, w := range fixture().extra {
				d := a.objects[fakeKey("deployment", str(w.Deployment, "metadata", "namespace"), str(w.Deployment, "metadata", "name"))][0]
				r := a.objects[fakeKey("helmrelease", str(w.Release, "metadata", "namespace"), str(w.Release, "metadata", "name"))][0]
				if num(d, "spec", "replicas") != 1 || val(r, "spec", "suspend") != false || !unpaused(d) || !unpaused(r) {
					t.Fatal("storage consumer not restored")
				}
			}
		})
	}
}
func TestTimeoutAndFailedPublishAlwaysLeaveStorageUntouched(t *testing.T) {
	for _, detach := range []bool{true, false} {
		a := fakeFixture()
		a.blockDetach = detach
		a.failReconcile = true
		e := testEngine(a)
		err := e.prepare(context.Background())
		if !detach {
			if err != nil {
				t.Fatal(err)
			}
			err = e.afterPublish(context.Background())
		}
		if err == nil {
			t.Fatal("failure accepted")
		}
		restored := e.restore(context.Background())
		if detach && restored != nil {
			t.Fatal(restored)
		}
		if !detach && (restored == nil || !a.stopped()) {
			t.Fatal("failed publication did not hold consumers safely paused")
		}
		if a.migrated {
			t.Fatal("failed migration accepted")
		}
	}
}
func TestForeignRunCannotResumeOrOverwriteTheJournal(t *testing.T) {
	a := fakeFixture()
	e := testEngine(a)
	if err := e.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	writes := a.writes
	e.holder = "101.1"
	if err := e.restore(context.Background()); err == nil {
		t.Fatal("foreign pause adopted")
	}
	if err := e.prepare(context.Background()); err == nil {
		t.Fatal("unfinished journal overwritten")
	}
	if a.writes != writes {
		t.Fatal("foreign run wrote resources")
	}
}

func TestVerificationRejectsBrokenPhysicalStorageAndReplacedHelmOwner(t *testing.T) {
	for _, field := range []string{"claim-phase", "retained-attachment", "faulted-volume", "helm-uid"} {
		t.Run(field, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.afterPublish(ctx); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "claim-phase":
				a.objects[fakeKey("persistentvolumeclaims", "*", "")][0]["status"].(object)["phase"] = "Lost"
			case "retained-attachment":
				v := a.objects[fakeKey("volumes.longhorn.io", "longhorn-system", "")][0]
				v["status"].(object)["state"] = "attached"
				pv := a.objects[fakeKey("persistentvolumes", "", "")][0]
				pv["status"] = object{"phase": "Released"}
			case "faulted-volume":
				a.objects[fakeKey("volumes.longhorn.io", "longhorn-system", "")][0]["status"].(object)["robustness"] = "faulted"
			case "helm-uid":
				a.objects[fakeKey("helmrelease", "crossview", "crossview")][0]["metadata"].(object)["uid"] = "replacement"
			}
			if err := e.verify(ctx); err == nil {
				t.Fatal("verification reported clean over broken identity or storage")
			}
		})
	}
}
func TestUnhealthyDatabaseRequiresRecoveryBeforeAnyMaintenanceWrite(t *testing.T) {
	a := fakeFixture()
	node := resource("Node", "", "autoscale-test")
	node["metadata"].(object)["labels"] = object{"ksail.io/autoscaled": "true"}
	node["spec"] = object{}
	node["status"] = object{"conditions": []any{object{"type": "Ready", "status": "True"}}}
	a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], node)
	a.objects[fakeKey("node", "", "autoscale-test")] = []object{node}
	for _, c := range a.objects[fakeKey("clusters.postgresql.cnpg.io", "*", "")] {
		c["status"].(object)["readyInstances"] = num(c, "spec", "instances") - 1
		c["status"].(object)["phase"] = "Waiting for the instances to become active"
		c["status"].(object)["conditions"] = []any{object{"type": "ContinuousArchiving", "status": "True"}}
	}
	w := resource("MutatingWebhookConfiguration", "", "kyverno-resource-mutating-webhook-cfg")
	w["webhooks"] = []any{object{"failurePolicy": "Fail", "rules": []any{object{"apiGroups": []any{"longhorn.io"}, "apiVersions": []any{"v1beta2"}, "resources": []any{"nodes"}, "operations": []any{"CREATE", "UPDATE"}}}}}
	a.objects[fakeKey("mutatingwebhookconfiguration", "", "kyverno-resource-mutating-webhook-cfg")] = []object{w}
	e := testEngine(a)
	if err := e.prepare(context.Background()); err == nil || a.writes != 0 {
		t.Fatalf("unhealthy database reached maintenance writes: writes=%d err=%v", a.writes, err)
	}
}

func TestFailureCleanupWiringPrecedesTalosMutation(t *testing.T) {
	b, err := os.ReadFile("../../.github/actions/deploy-prod/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err = yaml.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	steps := items(o, "runs", "steps")
	positions := map[string]int{}
	for i, s := range steps {
		run := str(s, "run")
		for _, phase := range []string{"before-publish", "after-publish", "verify", "restore"} {
			if run == "go run ./scripts/maintain-longhorn-clients "+phase {
				positions[phase] = i
				if phase == "restore" && !strings.Contains(str(s, "if"), "always()") {
					t.Fatal("cleanup is not unconditional")
				}
			}
		}
		if str(s, "id") == "publish_platform_manifest" {
			positions["publish"] = i
		}
		if str(s, "id") == "reconcile" {
			positions["reconcile"] = i
		}
		if strings.Contains(run, "cluster update") {
			positions["update"] = i
		}
	}
	for _, p := range []string{"before-publish", "after-publish", "verify", "restore", "publish", "reconcile", "update"} {
		if _, ok := positions[p]; !ok {
			t.Fatalf("missing %s", p)
		}
	}
	order := []string{"before-publish", "publish", "after-publish", "reconcile", "verify", "restore", "update"}
	for i := 1; i < len(order); i++ {
		if positions[order[i-1]] >= positions[order[i]] {
			t.Fatal("maintenance ordering is unsafe")
		}
	}
}

func TestPrepareCountsBaselineWorkersThroughTheDiskConfigLabelTransition(t *testing.T) {
	for label, ok := range map[string]bool{"true": true, "config": true, "false": false} {
		t.Run(label, func(t *testing.T) {
			a := fakeFixture()
			for _, n := range a.objects[fakeKey("nodes", "", "")] {
				n["metadata"].(object)["labels"].(object)["node.longhorn.io/create-default-disk"] = label
			}
			err := testEngine(a).prepare(context.Background())
			if ok && err != nil {
				t.Fatal(err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "three ready baseline storage workers")) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
