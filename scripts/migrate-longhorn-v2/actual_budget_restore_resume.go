package main

import (
	"context"
	"errors"
	"reflect"
)

type actualBudgetPriorRestore struct {
	run, source, namespaceUID string
}

func verifyUnstartedActualBudgetOwner(ctx context.Context, c client, opts actualBudgetRestoreConfirmation, currentRun string) error {
	prior := opts.prior
	refused := errors.New("the prior Actual Budget restore must be a different failed first-attempt run")
	if prior == nil || !validBenchmarkOwner(prior.run, prior.source) || !nativeRestoreUID(prior.namespaceUID) ||
		prior.run == currentRun || prior.run == opts.backupRun {
		return refused
	}
	result, err := readFinishedMigrationRun(ctx, c, prior.run, prior.source)
	if err != nil || result.Attempt != 1 || result.Conclusion != "failure" {
		return refused
	}
	return nil
}

func actualBudgetPreparationMatches(current, desired object) bool {
	kind, api := text(desired, "kind"), text(desired, "apiVersion")
	name, namespace := text(desired, "metadata", "name"), text(desired, "metadata", "namespace")
	if !recoveredNativeObject(current, api, kind, namespace) || !nativeRestoreUID(text(current, "metadata", "uid")) ||
		text(current, "metadata", "name") != name || value(current, "metadata", "ownerReferences") != nil {
		return false
	}
	for label, value := range valueObject(desired, "metadata", "labels") {
		if valueObject(current, "metadata", "labels")[label] != value {
			return false
		}
	}
	switch kind {
	case "Namespace":
		if text(current, "status", "phase") != "Active" {
			return false
		}
		for key, expected := range valueObject(desired, "metadata", "annotations") {
			if valueObject(current, "metadata", "annotations")[key] != expected {
				return false
			}
		}
		return true
	case "CiliumNetworkPolicy":
		return reflect.DeepEqual(value(current, "spec"), value(desired, "spec"))
	case "ConfigMap":
		return value(current, "binaryData") == nil && reflect.DeepEqual(value(current, "data"), value(desired, "data"))
	default:
		return false
	}
}

func prepareActualBudgetRestoreScope(ctx context.Context, c client, source string, prior *actualBudgetPriorRestore, assets []object) (object, error) {
	const refused = "the original isolated Actual Budget preparation could not be verified"
	namespace, policy, modifier := assets[0], assets[1], assets[2]
	name := text(namespace, "metadata", "name")
	var current object
	var err error
	if prior != nil {
		current, err = c.get(ctx, "namespace", "", name, false)
		if err != nil || text(current, "metadata", "uid") != prior.namespaceUID {
			return nil, errors.New(refused)
		}
	} else {
		current, err = c.create(ctx, source, namespace)
		if err != nil {
			return nil, errors.New("isolated namespace creation is unconfirmed")
		}
	}
	if !actualBudgetPreparationMatches(current, namespace) {
		return nil, errors.New(refused)
	}
	if err = verifyUnstartedActualBudgetRestore(ctx, c, current); err != nil {
		return nil, err
	}
	if err = verifyActualBudgetPreparationAssets(ctx, c, source, policy, modifier, true); err != nil {
		return nil, err
	}
	return current, nil
}

func verifyActualBudgetPreparationAssets(ctx context.Context, c client, source string, policy, modifier object, createMissing bool) error {
	refused := errors.New("the restore isolation policy or claim modifier changed")
	var missing []object
	for _, asset := range []struct {
		resource string
		desired  object
	}{
		{"ciliumnetworkpolicy.cilium.io", policy},
		{"configmap", modifier},
	} {
		name := text(asset.desired, "metadata", "name")
		namespace := text(asset.desired, "metadata", "namespace")
		existing, err := c.get(ctx, asset.resource, namespace, name, true)
		if err != nil {
			return refused
		}
		if existing == nil && createMissing {
			missing = append(missing, asset.desired)
		} else if !actualBudgetPreparationMatches(existing, asset.desired) {
			return refused
		}
	}
	for _, desired := range missing {
		created, err := c.create(ctx, source, desired)
		if err != nil || !actualBudgetPreparationMatches(created, desired) {
			return refused
		}
	}
	return nil
}

// A prior accepted Restore or storage transfer may still own data after its
// workflow exits. Recovery of that state requires its own exact review.
func verifyUnstartedActualBudgetRestore(ctx context.Context, c client, namespace object) error {
	refused := errors.New("the Actual Budget restore already has a request, workload or storage; preserve the isolated scope")
	identity := benchmarkIdentity(namespace)
	name := identity.Name
	renewed, err := c.get(ctx, "namespace", "", name, false)
	if err != nil || benchmarkIdentity(renewed) != identity || !recoveredNativeObject(renewed, "v1", "Namespace", "") ||
		text(renewed, "status", "phase") != "Active" ||
		!reflect.DeepEqual(value(renewed, "metadata", "labels"), value(namespace, "metadata", "labels")) ||
		!reflect.DeepEqual(value(renewed, "metadata", "annotations"), value(namespace, "metadata", "annotations")) ||
		!actualBudgetRestoreScopeClear(ctx, c, identity) {
		return refused
	}
	restore, err := c.get(ctx, "restores.velero.io", "velero", name, true)
	if err != nil || restore != nil {
		return refused
	}
	for _, kind := range []string{"pods", "persistentvolumeclaims"} {
		items, err := c.list(ctx, kind, name)
		if err != nil || len(items) != 0 {
			return refused
		}
	}
	transfers, err := c.list(ctx, "datadownloads.velero.io", "velero")
	if err != nil {
		return refused
	}
	for _, transfer := range transfers {
		if text(transfer, "metadata", "labels", "velero.io/restore-name") == name ||
			text(transfer, "spec", "targetVolume", "namespace") == name {
			return refused
		}
	}
	return nil
}
