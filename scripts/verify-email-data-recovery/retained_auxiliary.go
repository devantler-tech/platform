package main

import (
	"context"
	"errors"
	"regexp"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
)

var (
	retainedAuxiliaryName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	retainedAuxiliaryUID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Only the two observed controller-generated child kinds may use this path.
// The caller must freeze both child UID and returned parent key on its first
// complete inventory. A missing child is permitted only after its owned reader
// reference was removed; this helper never adopts or deletes an auxiliary.
func (d *recoveryDriver) retainedAuxiliary(ctx context.Context, binding namespaceinventory.Binding, bindings map[string]namespaceinventory.Binding, readerUID string) (string, error) {
	fail := func() (string, error) { return "", errors.New("retained-auxiliary") }
	if d == nil || d.k == nil || d.i != retainedRecovery || ctx.Err() != nil || binding.Namespace != retainedRecovery.namespace || len(binding.Name) > 253 || !retainedAuxiliaryName.MatchString(binding.Name) || !retainedAuxiliaryUID.MatchString(binding.UID) || !retainedAuxiliaryUID.MatchString(readerUID) {
		return fail()
	}
	api := ""
	switch {
	case binding.Kind == "CiliumEndpoint" && binding.Resource == "ciliumendpoints.cilium.io":
		api = "cilium.io/v2"
	case binding.Kind == "PolicyReport" && binding.Resource == "policyreports.wgpolicyk8s.io":
		api = "wgpolicyk8s.io/v1alpha2"
	default:
		return fail()
	}
	actual, found, err := d.lookup(ctx, binding.Resource, binding.Name, binding.Namespace)
	if err != nil || !found || ctx.Err() != nil || text(actual, "apiVersion") != api || text(actual, "kind") != binding.Kind {
		return fail()
	}
	m := child(actual, "metadata")
	if text(m, "name") != binding.Name || text(m, "namespace") != binding.Namespace || text(m, "uid") != binding.UID || m["deletionTimestamp"] != nil {
		return fail()
	}
	owners := items(m, "ownerReferences")
	if len(owners) != 1 || text(owners[0], "apiVersion") != "v1" || text(owners[0], "uid") == binding.UID {
		return fail()
	}
	owner := owners[0]
	for _, flag := range []string{"controller", "blockOwnerDeletion"} {
		if value, present := owner[flag]; present {
			if _, valid := value.(bool); !valid {
				return fail()
			}
		}
	}
	parent, ok := retainedAuxiliaryOwner(owner, binding.Kind, bindings, readerUID)
	if !ok {
		return fail()
	}
	if binding.Kind == "PolicyReport" {
		scope := child(actual, "scope")
		if text(scope, "apiVersion") != "v1" || text(scope, "kind") != text(owner, "kind") || text(scope, "name") != text(owner, "name") || text(scope, "namespace") != binding.Namespace || text(scope, "uid") != text(owner, "uid") {
			return fail()
		}
	}
	return parent, nil
}

func retainedAuxiliaryOwner(owner object, childKind string, bindings map[string]namespaceinventory.Binding, readerUID string) (string, bool) {
	ns := retainedRecovery.namespace
	if text(owner, "kind") == "Pod" && text(owner, "name") == "reader" && text(owner, "uid") == readerUID {
		// The independent reader witness remains usable while its frozen child
		// awaits GC after Pod deletion. No current Pod is newly adopted here.
		return "Pod/" + ns + "/reader", true
	}
	if childKind != "PolicyReport" || text(owner, "kind") != "ServiceAccount" || text(owner, "name") != "default" {
		return "", false
	}
	key := "ServiceAccount/" + ns + "/default"
	parent, found := bindings[key]
	if !found || parent.Resource != "serviceaccounts" || parent.Kind != "ServiceAccount" || parent.Name != "default" || parent.Namespace != ns || !retainedAuxiliaryUID.MatchString(parent.UID) || parent.UID == readerUID || text(owner, "uid") != parent.UID {
		return "", false
	}
	return key, true
}
