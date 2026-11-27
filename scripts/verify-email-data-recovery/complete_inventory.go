package main

import (
	"context"
	"errors"
	"regexp"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
)

// ownedResources is the served resource of each kind this invocation creates
// in its namespace.
var ownedResources = map[string]string{"Pod": "pods", "PersistentVolumeClaim": "persistentvolumeclaims", "Secret": "secrets", "ConfigMap": "configmaps", "CiliumNetworkPolicy": "ciliumnetworkpolicies.cilium.io"}

var ephemeralReportName = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})-[a-z0-9]{5}$`)

// useCompleteInventory binds the complete namespace inventory to the confirmed
// dispatch's private credential. It reads nothing until cleanup asks for it.
func (d *recoveryDriver) useCompleteInventory(env func(string) string) error {
	client, endpoint, err := namespaceinventory.LoadEmailRecoveryClient(env)
	if err != nil {
		return errors.New("inventory-credential")
	}
	d.completeNamespaceInventory = func(ctx context.Context) error {
		namespace, err := d.ref("Namespace", d.i.namespace, "")
		if err != nil {
			return err
		}
		inventory, err := namespaceinventory.ObserveEmailRecovery(ctx, client, endpoint, d.i.namespace, namespace.uid)
		if err != nil {
			return errors.New("complete-inventory")
		}
		return d.classifyInventory(ctx, inventory.Bindings())
	}
	return nil
}

// classifyInventory accepts the complete set of objects the namespace serves
// only when each one is either created by this invocation with its created UID,
// or one of the exact objects the platform adds to every namespace. Kubescape's
// scan records were already accepted, by kind and group, by the inventory. A
// missing owned object means the inventory is not the namespace's.
func (d *recoveryDriver) classifyInventory(ctx context.Context, bindings []namespaceinventory.Binding) error {
	// Reports may describe any object here or one this invocation created and
	// removed; reports and events never vouch for each other.
	known := map[string]bool{}
	for uid := range d.created {
		known[uid] = true
	}
	for _, b := range bindings {
		if !inventoryRecord(b) {
			known[b.UID] = true
		}
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.Namespace != d.i.namespace || b.UID == "" {
			return errors.New("inventory-foreign")
		}
		if b.Kind == "Event" && (b.Resource == "events" || b.Resource == "events.events.k8s.io") {
			continue
		}
		key := ownedReference{kind: b.Kind, name: b.Name, namespace: b.Namespace}.key()
		if seen[key] {
			return errors.New("inventory-duplicate-name")
		}
		seen[key] = true
		if ref, owned := d.refs[key]; owned {
			if ref.uid != b.UID || ownedResources[b.Kind] != b.Resource {
				return errors.New("inventory-replaced")
			}
			continue
		}
		accepted, err := d.platformObject(ctx, b, known)
		if err != nil {
			return err
		}
		if !accepted {
			return errors.New("inventory-foreign")
		}
	}
	for key, ref := range d.refs {
		if ref.namespace == d.i.namespace && !seen[key] {
			return errors.New("inventory-missing")
		}
	}
	return nil
}

// inventoryRecord reports kinds that only record facts about other objects.
func inventoryRecord(b namespaceinventory.Binding) bool {
	return b.Kind == "Event" || b.Kind == "PolicyReport" || b.Kind == "EphemeralReport"
}

// platformObject recognises what the platform adds to every namespace. Their
// contents are verified by the namespace's typed inventory; kinds here are
// matched with their served resource, never by name alone.
func (d *recoveryDriver) platformObject(ctx context.Context, b namespaceinventory.Binding, known map[string]bool) (bool, error) {
	switch b.Kind + "/" + b.Resource {
	case "ServiceAccount/serviceaccounts":
		return b.Name == "default", nil
	case "ConfigMap/configmaps":
		return b.Name == "kube-root-ca.crt" || b.Name == trustBundle, nil
	case "LimitRange/limitranges", "ResourceQuota/resourcequotas":
		_, generated := generatedLimits[b.Kind+"/"+b.Name]
		return generated, nil
	case "CiliumNetworkPolicy/ciliumnetworkpolicies.cilium.io", "NetworkPolicy/networkpolicies.networking.k8s.io":
		_, generated := d.namespacePolicies[b.Kind+"/"+b.Name]
		return generated, nil
	case "PolicyReport/policyreports.wgpolicyk8s.io":
		// Kyverno names a report after the UID of the object it describes.
		return known[b.Name], nil
	case "EphemeralReport/ephemeralreports.reports.kyverno.io":
		match := ephemeralReportName.FindStringSubmatch(b.Name)
		return match != nil && known[match[1]], nil
	case "CiliumEndpoint/ciliumendpoints.cilium.io":
		return d.readerEndpoint(ctx, b)
	}
	return false, nil
}

// readerEndpoint accepts Cilium's endpoint for this invocation's reader Pod
// only, bound by its owner's created UID, which outlives the Pod's removal.
func (d *recoveryDriver) readerEndpoint(ctx context.Context, b namespaceinventory.Binding) (bool, error) {
	if b.Name != "reader" {
		return false, nil
	}
	endpoint, found, err := d.lookup(ctx, "ciliumendpoints.cilium.io", b.Name, b.Namespace)
	if err != nil {
		return false, errors.New("inventory-endpoint")
	}
	if !found {
		// Removed after the complete read; a later inventory sees it absent.
		return true, nil
	}
	m := child(endpoint, "metadata")
	owners := items(m, "ownerReferences")
	if text(endpoint, "apiVersion") != "cilium.io/v2" || text(endpoint, "kind") != "CiliumEndpoint" || text(m, "uid") != b.UID || len(owners) != 1 {
		return false, nil
	}
	owner := owners[0]
	return text(owner, "apiVersion") == "v1" && text(owner, "kind") == "Pod" && text(owner, "name") == "reader" && d.created[text(owner, "uid")], nil
}
