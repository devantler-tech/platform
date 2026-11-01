package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type cleanupAPI struct {
	objects      map[string]object
	paths        map[string]string
	blockBackend bool
	cancel       context.CancelFunc
}

func cleanupKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }
func (a *cleanupAPI) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	switch args[0] {
	case "get":
		kind, ns := args[1], ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "--namespace=") {
				ns = strings.TrimPrefix(arg, "--namespace=")
			}
		}
		if strings.Contains(kind, ",") {
			out := []any{}
			resourceNames := map[string]string{"Pod": "pods", "PersistentVolumeClaim": "persistentvolumeclaims", "Secret": "secrets", "ConfigMap": "configmaps", "Service": "services", "ServiceAccount": "serviceaccounts", "Deployment": "deployments.apps", "StatefulSet": "statefulsets.apps", "DaemonSet": "daemonsets.apps", "Job": "jobs.batch", "CronJob": "cronjobs.batch", "CiliumNetworkPolicy": "ciliumnetworkpolicies.cilium.io", "NetworkPolicy": "networkpolicies.networking.k8s.io"}
			for _, o := range a.objects {
				if resource, known := resourceNames[text(o, "kind")]; known && text(child(o, "metadata"), "namespace") == ns && strings.Contains(","+kind+",", ","+resource+",") {
					out = append(out, o)
				}
			}
			return json.Marshal(object{"items": out})
		}
		if len(args) < 3 {
			return nil, errors.New("get")
		}
		o, found := a.objects[cleanupKey(kind, ns, args[2])]
		if !found {
			return nil, nil
		}
		return json.Marshal(o)
	case "delete":
		if len(args) != 3 {
			return nil, errors.New("delete")
		}
		key, found := a.paths[strings.TrimPrefix(args[1], "--raw=")]
		if !found {
			return nil, errors.New("foreign-delete")
		}
		var options object
		if json.Unmarshal(input, &options) != nil || text(child(options, "preconditions"), "uid") != text(child(a.objects[key], "metadata"), "uid") {
			return nil, errors.New("unfenced-delete")
		}
		deleting := a.objects[key]
		delete(a.objects, key)
		if text(deleting, "kind") == "Namespace" {
			for childKey, childObject := range a.objects {
				if text(child(childObject, "metadata"), "namespace") == text(child(deleting, "metadata"), "name") {
					delete(a.objects, childKey)
				}
			}
		}
		if text(deleting, "kind") == "PersistentVolumeClaim" {
			if a.blockBackend {
				a.cancel()
			} else {
				delete(a.objects, cleanupKey("PersistentVolume", "", "pvc-target-volume"))
				delete(a.objects, cleanupKey("volumes.longhorn.io", "longhorn-system", "pvc-target-volume"))
			}
		}
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected-operation")
}
func cleanupFixture(t *testing.T, blocked bool) (*recoveryDriver, *cleanupAPI, context.Context) {
	t.Helper()
	env := confirmedInvocation()
	i, err := authorizeDataRecovery(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &cleanupAPI{objects: map[string]object{}, paths: map[string]string{}, blockBackend: blocked, cancel: cancel}
	d := &recoveryDriver{i: i, image: fixtureImage, k: a, refs: map[string]ownedReference{}, volumeName: "pvc-target-volume", volumeUID: "target-longhorn-uid", pvUID: "target-pv-uid"}
	policySource, err := os.Open("../../" + namespacePolicySource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policySource.Close() })
	d.namespacePolicies, err = readNamespacePolicies(policySource)
	if err != nil {
		t.Fatal(err)
	}
	graph := append(recoveryResources(i, "test-key", "test-admin"), recoveryReader(i, fixtureImage), downloadManifest(i, "0123456789abcdef0123456789abcdef"), backupManifest(i))
	for _, o := range graph {
		m := child(o, "metadata")
		kind, ns, name := text(o, "kind"), text(m, "namespace"), text(m, "name")
		uid := "owned-" + kind + "-" + name
		m["uid"] = uid
		ref := ownedReference{kind: kind, name: name, namespace: ns, uid: uid}
		d.refs[ref.key()] = ref
		a.objects[cleanupKey(kind, ns, name)] = o
	}
	pvc := a.objects[cleanupKey("PersistentVolumeClaim", i.namespace, "data")]
	claimUID := text(child(pvc, "metadata"), "uid")
	child(pvc, "spec")["volumeName"] = d.volumeName
	pvc["status"] = object{"phase": "Bound"}
	a.objects[cleanupKey("PersistentVolume", "", d.volumeName)] = object{"metadata": object{"name": d.volumeName, "uid": d.pvUID}, "spec": object{"claimRef": object{"namespace": i.namespace, "name": "data", "uid": claimUID}, "persistentVolumeReclaimPolicy": "Delete", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": d.volumeName}}}
	a.objects[cleanupKey("volumes.longhorn.io", "longhorn-system", d.volumeName)] = object{"metadata": object{"name": d.volumeName, "uid": d.volumeUID}, "spec": object{"encrypted": true, "numberOfReplicas": float64(3), "dataEngine": "v1"}}
	a.objects[cleanupKey("DataDownload", "velero", i.namespace+"-download")]["status"] = object{"phase": "Completed"}
	a.objects[cleanupKey("Backup", "velero", i.namespace+"-backup")]["status"] = object{"phase": "Completed", "expiration": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}
	pod, sourcePVC, pv := servingFixture()
	d.source, err = proveServingSource(pod, sourcePVC, pv, fixtureImage)
	if err != nil {
		t.Fatal(err)
	}
	a.objects[cleanupKey("Pod", "private-mail", "private-mail-0")] = pod
	a.objects[cleanupKey("PersistentVolumeClaim", "private-mail", "private-mail-data")] = sourcePVC
	a.objects[cleanupKey("PersistentVolume", "", d.source.pv)] = pv
	for path, key := range map[string]string{
		"/api/v1/namespaces/" + i.namespace + "/pods/reader":                                    cleanupKey("Pod", i.namespace, "reader"),
		"/api/v1/namespaces/" + i.namespace + "/persistentvolumeclaims/data":                    cleanupKey("PersistentVolumeClaim", i.namespace, "data"),
		"/api/v1/namespaces/" + i.namespace + "/secrets/storage-key":                            cleanupKey("Secret", i.namespace, "storage-key"),
		"/api/v1/namespaces/" + i.namespace + "/secrets/test-authentication":                    cleanupKey("Secret", i.namespace, "test-authentication"),
		"/api/v1/namespaces/" + i.namespace + "/configmaps/datastore":                           cleanupKey("ConfigMap", i.namespace, "datastore"),
		"/apis/cilium.io/v2/namespaces/" + i.namespace + "/ciliumnetworkpolicies/isolation":     cleanupKey("CiliumNetworkPolicy", i.namespace, "isolation"),
		"/apis/storage.k8s.io/v1/storageclasses/" + i.namespace:                                 cleanupKey("StorageClass", "", i.namespace),
		"/api/v1/namespaces/" + i.namespace:                                                     cleanupKey("Namespace", "", i.namespace),
		"/apis/velero.io/v2alpha1/namespaces/velero/datadownloads/" + i.namespace + "-download": cleanupKey("DataDownload", "velero", i.namespace+"-download"),
	} {
		a.paths[path] = key
	}
	return d, a, ctx
}
func TestCleanupRetainsTheKeyUntilBothStorageObjectsAreAbsent(t *testing.T) {
	t.Run("still-present", func(t *testing.T) {
		d, a, ctx := cleanupFixture(t, true)
		if d.cleanupRestore(ctx) == nil {
			t.Fatal("live backend was treated as removed")
		}
		if _, found := a.objects[cleanupKey("Secret", d.i.namespace, "storage-key")]; !found {
			t.Fatal("key was destroyed while encrypted storage still existed")
		}
	})
	t.Run("both-absent", func(t *testing.T) {
		d, a, ctx := cleanupFixture(t, false)
		if err := d.cleanupRestore(ctx); err != nil {
			t.Fatalf("complete owned cleanup failed: %s", err)
		}
		if _, found := a.objects[cleanupKey("Secret", d.i.namespace, "storage-key")]; found {
			t.Fatal("disposed storage retained its disposable key")
		}
		if len(d.refs) != 1 {
			t.Fatal("cleanup did not retain only the native backup")
		}
	})
}

func TestCleanupRetainsAnUnexpectedClaimAndItsNamespace(t *testing.T) {
	d, a, ctx := cleanupFixture(t, false)
	key := cleanupKey("PersistentVolumeClaim", d.i.namespace, "other-data")
	a.objects[key] = object{"kind": "PersistentVolumeClaim", "metadata": object{"name": "other-data", "namespace": d.i.namespace, "uid": "foreign-claim-uid"}}
	if d.cleanupRestore(ctx) == nil {
		t.Fatal("foreign claim allowed namespace disposal")
	}
	for _, key := range []string{key, cleanupKey("Namespace", "", d.i.namespace), cleanupKey("Secret", d.i.namespace, "storage-key")} {
		if _, found := a.objects[key]; !found {
			t.Fatal("foreign data or retained recovery custody was destroyed")
		}
	}
}

// These are hand-checked API shapes of the four platform-generated policies,
// independent of the source parser used by the cleanup guard.
func generatedNamespacePolicyFixture(namespace, namespaceUID string) []object {
	policies := []object{
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": object{"name": "default-deny"}, "spec": object{"endpointSelector": object{}, "ingress": []any{object{}}, "egress": []any{object{}}, "enableDefaultDeny": object{"ingress": true, "egress": true}}},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": object{"name": "allow-dns"}, "spec": object{"endpointSelector": object{}, "egress": []any{object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "UDP"}, object{"port": "53", "protocol": "TCP"}}}}}}}},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": object{"name": "allow-cnpg-operator"}, "spec": object{"endpointSelector": object{"matchExpressions": []any{object{"key": "k8s:cnpg.io/cluster", "operator": "Exists"}}}, "ingress": []any{object{"fromEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "cnpg-system", "k8s:app.kubernetes.io/name": "cloudnative-pg", "k8s:app.kubernetes.io/instance": "cloudnative-pg"}}}, "toPorts": []any{object{"ports": []any{object{"port": "8000", "protocol": "TCP"}, object{"port": "5432", "protocol": "TCP"}}}}}}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": object{"name": "default-deny"}, "spec": object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}},
	}
	for _, policy := range policies {
		m := child(policy, "metadata")
		name := text(m, "name")
		rule := "generate-" + name
		if text(policy, "kind") == "NetworkPolicy" {
			rule = "generate-default-deny-networkpolicy"
		}
		m["namespace"], m["uid"] = namespace, "generated-"+text(policy, "kind")+"-"+name
		m["labels"] = object{"app.kubernetes.io/managed-by": "kyverno", "generate.kyverno.io/policy-name": "add-default-deny", "generate.kyverno.io/policy-namespace": "", "generate.kyverno.io/rule-name": rule, "generate.kyverno.io/trigger-group": "", "generate.kyverno.io/trigger-kind": "Namespace", "generate.kyverno.io/trigger-namespace": "", "generate.kyverno.io/trigger-uid": namespaceUID, "generate.kyverno.io/trigger-version": "v1"}
	}
	return policies
}

func TestCleanupRecognizesOnlyBoundPlatformGeneratedPolicies(t *testing.T) {
	d, api, ctx := cleanupFixture(t, false)
	namespace, _ := d.ref("Namespace", d.i.namespace, "")
	for _, policy := range generatedNamespacePolicyFixture(d.i.namespace, namespace.uid) {
		api.objects[cleanupKey(text(policy, "kind"), d.i.namespace, text(child(policy, "metadata"), "name"))] = policy
	}
	if err := d.cleanupRestore(ctx); err != nil {
		t.Fatalf("verified platform policies prevented owned namespace disposal: %s", err)
	}
	if _, exists := api.objects[cleanupKey("Namespace", "", d.i.namespace)]; exists {
		t.Fatal("the verified disposable namespace was retained")
	}
	for _, o := range api.objects {
		if text(child(o, "metadata"), "namespace") == d.i.namespace {
			t.Fatal("namespace disposal left a generated resource")
		}
	}
}

func TestCleanupRetainsPoliciesWithChangedSpecsOrOwnership(t *testing.T) {
	mutations := map[string]func(object){
		"widened-spec": func(o object) {
			if text(o, "kind") == "NetworkPolicy" {
				child(o, "spec")["ingress"] = []any{object{}}
			} else {
				child(o, "spec")["egress"] = []any{object{"toEntities": []any{"all"}}}
			}
		},
		"additional-specs": func(o object) { o["specs"] = []any{object{"endpointSelector": object{}, "egress": []any{object{}}}} },
		"foreign-trigger": func(o object) {
			child(child(o, "metadata"), "labels")["generate.kyverno.io/trigger-uid"] = "another-namespace-uid"
		},
		"missing-trigger": func(o object) { delete(child(child(o, "metadata"), "labels"), "generate.kyverno.io/trigger-uid") },
		"foreign-rule": func(o object) {
			child(child(o, "metadata"), "labels")["generate.kyverno.io/rule-name"] = "another-rule"
		},
		"foreign-manager": func(o object) {
			child(child(o, "metadata"), "labels")["app.kubernetes.io/managed-by"] = "another-manager"
		},
		"foreign-name": func(o object) { child(o, "metadata")["name"] = "another-policy" },
		"foreign-api":  func(o object) { o["apiVersion"] = "cilium.io/v3" },
		"missing-uid":  func(o object) { delete(child(o, "metadata"), "uid") },
	}
	for name, mutate := range mutations {
		for policyIndex := range 4 {
			t.Run(fmt.Sprintf("%s/policy-%d", name, policyIndex), func(t *testing.T) {
				d, api, ctx := cleanupFixture(t, false)
				namespace, _ := d.ref("Namespace", d.i.namespace, "")
				policy := generatedNamespacePolicyFixture(d.i.namespace, namespace.uid)[policyIndex]
				mutate(policy)
				key := cleanupKey(text(policy, "kind"), d.i.namespace, text(child(policy, "metadata"), "name"))
				api.objects[key] = policy
				if d.cleanupRestore(ctx) == nil {
					t.Fatal("changed or unowned policy allowed namespace disposal")
				}
				for _, retained := range []string{key, cleanupKey("Namespace", "", d.i.namespace), cleanupKey("Secret", d.i.namespace, "storage-key")} {
					if _, found := api.objects[retained]; !found {
						t.Fatal("uncertain policy or recovery custody was removed")
					}
				}
			})
		}
	}
}
