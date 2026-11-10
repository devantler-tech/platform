package main

import (
	"context"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
)

const (
	auxiliaryReaderUID = "reader-owner-canary"
	auxiliarySAUID     = "serviceaccount-owner-canary"
)

func retainedAuxiliaryFixture(kind, parentKind string) (*recoveryDriver, namespaceinventory.Binding, map[string]namespaceinventory.Binding, object) {
	ns := retainedRecovery.namespace
	binding := namespaceinventory.Binding{Name: "diagnostic-child", Namespace: ns, UID: "child-canary", Kind: kind, Resource: "policyreports.wgpolicyk8s.io"}
	api := "wgpolicyk8s.io/v1alpha2"
	if kind == "CiliumEndpoint" {
		binding.Name, binding.Resource, api = "reader", "ciliumendpoints.cilium.io", "cilium.io/v2"
	}
	owner := object{"apiVersion": "v1", "kind": "Pod", "name": "reader", "uid": auxiliaryReaderUID}
	if parentKind == "ServiceAccount" {
		owner["kind"], owner["name"], owner["uid"] = "ServiceAccount", "default", auxiliarySAUID
	}
	actual := object{"apiVersion": api, "kind": kind, "metadata": object{"name": binding.Name, "namespace": ns, "uid": binding.UID, "ownerReferences": []any{owner}}}
	if kind == "PolicyReport" {
		actual["scope"] = object{"apiVersion": "v1", "kind": owner["kind"], "name": owner["name"], "namespace": ns, "uid": owner["uid"]}
	}
	bindings := map[string]namespaceinventory.Binding{
		"Pod/" + ns + "/reader":             {Name: "reader", Namespace: ns, UID: auxiliaryReaderUID, Kind: "Pod", Resource: "pods"},
		"ServiceAccount/" + ns + "/default": {Name: "default", Namespace: ns, UID: auxiliarySAUID, Kind: "ServiceAccount", Resource: "serviceaccounts"},
	}
	k := &cleanupAPI{objects: map[string]object{cleanupKey(binding.Resource, ns, binding.Name): actual}}
	d := &recoveryDriver{i: retainedRecovery, k: k, refs: map[string]ownedReference{}}
	return d, binding, bindings, actual
}

// Removing owner identity, canonical GVR or scope checks would authorize a
// foreign controller child as disposable namespace-generated state.
func TestRetainedAuxiliaryBindsOnlyObservedControllerChildren(t *testing.T) {
	for _, tc := range []struct{ kind, parentKind, want string }{
		{"CiliumEndpoint", "Pod", "Pod/" + retainedRecovery.namespace + "/reader"},
		{"PolicyReport", "Pod", "Pod/" + retainedRecovery.namespace + "/reader"},
		{"PolicyReport", "ServiceAccount", "ServiceAccount/" + retainedRecovery.namespace + "/default"},
	} {
		t.Run(tc.kind+"/"+tc.parentKind, func(t *testing.T) {
			d, binding, bindings, _ := retainedAuxiliaryFixture(tc.kind, tc.parentKind)
			parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, auxiliaryReaderUID)
			if err != nil || parent != tc.want {
				t.Fatal("canonical controller child lost its exact parent binding")
			}
			if len(d.refs) != 0 || len(d.k.(*cleanupAPI).objects) != 1 {
				t.Fatal("auxiliary observation adopted or mutated a resource")
			}
		})
	}
}

func TestRetainedAuxiliaryRefusesForeignOrAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct{ kind, parentKind string }{{"CiliumEndpoint", "Pod"}, {"PolicyReport", "Pod"}, {"PolicyReport", "ServiceAccount"}} {
		for _, failure := range []string{"foreign resource", "foreign kind", "foreign namespace", "unsafe name", "empty UID", "replaced UID", "wrong name", "wrong namespace", "wrong kind", "wrong API", "terminating", "unowned", "duplicate owners", "malformed owners", "foreign owner UID", "foreign owner name", "foreign owner kind", "foreign owner API", "malformed owner controller", "missing reader witness", "child reuses owner UID", "foreign invocation"} {
			t.Run(tc.kind+"/"+tc.parentKind+"/"+failure, func(t *testing.T) {
				d, binding, bindings, actual := retainedAuxiliaryFixture(tc.kind, tc.parentKind)
				meta := child(actual, "metadata")
				owner := items(meta, "ownerReferences")[0]
				readerUID := auxiliaryReaderUID
				switch failure {
				case "foreign resource":
					binding.Resource += ".foreign.invalid"
				case "foreign kind":
					binding.Kind = "Secret"
				case "foreign namespace":
					binding.Namespace = "foreign-namespace"
				case "unsafe name":
					binding.Name = "../private-input-canary"
				case "empty UID":
					binding.UID = ""
				case "replaced UID":
					meta["uid"] = "private-replacement-canary"
				case "wrong name":
					meta["name"] = "foreign"
				case "wrong namespace":
					meta["namespace"] = "foreign"
				case "wrong kind":
					actual["kind"] = "Foreign"
				case "wrong API":
					actual["apiVersion"] = "foreign.invalid/v1"
				case "terminating":
					meta["deletionTimestamp"] = "2026-11-03T00:00:00Z"
				case "unowned":
					meta["ownerReferences"] = []any{}
				case "duplicate owners":
					meta["ownerReferences"] = []any{owner, owner}
				case "malformed owners":
					meta["ownerReferences"] = []any{owner, "foreign"}
				case "foreign owner UID":
					owner["uid"] = "private-owner-canary"
				case "foreign owner name":
					owner["name"] = "foreign"
				case "foreign owner kind":
					owner["kind"] = "Namespace"
				case "foreign owner API":
					owner["apiVersion"] = "foreign.invalid/v1"
				case "malformed owner controller":
					owner["controller"] = "true"
				case "missing reader witness":
					readerUID = ""
				case "child reuses owner UID":
					binding.UID = owner["uid"].(string)
					meta["uid"] = binding.UID
				case "foreign invocation":
					d.i.run = "99999999999"
					d.i.namespace = "email-data-recovery-99999999999"
				}
				parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, readerUID)
				if err == nil || parent != "" {
					t.Fatal("foreign, duplicate or uncertain auxiliary was accepted")
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("private auxiliary identity escaped in an error")
				}
				if len(d.refs) != 0 || len(d.k.(*cleanupAPI).objects) != 1 {
					t.Fatal("refused observation adopted or mutated a resource")
				}
			})
		}
	}
}

func TestRetainedPolicyReportRequiresExactScopeAndCanonicalServiceAccount(t *testing.T) {
	for _, parentKind := range []string{"Pod", "ServiceAccount"} {
		for _, field := range []string{"apiVersion", "kind", "name", "namespace", "uid"} {
			t.Run(parentKind+"/scope-"+field, func(t *testing.T) {
				d, binding, bindings, actual := retainedAuxiliaryFixture("PolicyReport", parentKind)
				child(actual, "scope")[field] = "private-scope-canary"
				if parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, auxiliaryReaderUID); err == nil || parent != "" {
					t.Fatal("mismatched PolicyReport scope was accepted")
				}
			})
		}
	}
	for _, failure := range []string{"missing", "foreign resource", "wrong UID", "wrong name", "wrong namespace", "wrong kind"} {
		t.Run("serviceaccount/"+failure, func(t *testing.T) {
			d, binding, bindings, _ := retainedAuxiliaryFixture("PolicyReport", "ServiceAccount")
			key := "ServiceAccount/" + retainedRecovery.namespace + "/default"
			parent := bindings[key]
			switch failure {
			case "missing":
				delete(bindings, key)
			case "foreign resource":
				parent.Resource = "serviceaccounts.foreign.invalid"
			case "wrong UID":
				parent.UID = "private-replacement-canary"
			case "wrong name":
				parent.Name = "foreign"
			case "wrong namespace":
				parent.Namespace = "foreign"
			case "wrong kind":
				parent.Kind = "Secret"
			}
			if failure != "missing" {
				bindings[key] = parent
			}
			if parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, auxiliaryReaderUID); err == nil || parent != "" {
				t.Fatal("report inherited authority from an unbound ServiceAccount")
			}
		})
	}
	d, binding, bindings, _ := retainedAuxiliaryFixture("CiliumEndpoint", "ServiceAccount")
	if parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, auxiliaryReaderUID); err == nil || parent != "" {
		t.Fatal("CiliumEndpoint was allowed a ServiceAccount parent")
	}
}

// After reader removal, a still-present frozen child can be observed until GC
// removes it. Its independently witnessed owner UID must remain unchanged.
func TestRetainedPodAuxiliaryUsesIndependentReaderWitnessAfterParentDisappears(t *testing.T) {
	d, binding, bindings, _ := retainedAuxiliaryFixture("PolicyReport", "Pod")
	delete(bindings, "Pod/"+retainedRecovery.namespace+"/reader")
	parent, err := d.retainedAuxiliary(context.Background(), binding, bindings, auxiliaryReaderUID)
	if err != nil || parent != "Pod/"+retainedRecovery.namespace+"/reader" {
		t.Fatal("frozen child lost its independent reader owner witness during GC")
	}
}
