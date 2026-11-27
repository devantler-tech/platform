package main

import (
	"context"
	"errors"
)

// createEnvironmentResource records intent before exclusive creation. A pending
// phase cannot be invoked again, including after a lost API response.
func (p *cnpgProofRun) createEnvironmentResource(ctx context.Context, key string) (object, error) {
	refused := errors.New("recovery environment creation lacks its recorded ordered plan")
	index, previous, requested, completed := -1, "", "", ""
	switch key {
	case "Namespace":
		index, previous, requested, completed = 0, "restore-planned", "namespace-requested", "namespace-created"
	case "Policy":
		index, previous, requested, completed = 1, "namespace-created", "policy-requested", "policy-created"
	}
	if index < 0 || p.c.context != "admin@prod" || p.record.Version != 1 || p.record.Phase != previous || p.record.Plan == nil || len(p.record.Plan.Resources) != 4 || len(p.record.Resources) != index || p.record.RestoreVerified || p.record.Cleaned || !commandContextActive(ctx) {
		return nil, refused
	}
	desired := p.record.Plan.Resources[index]
	namespace := "longhorn-db-proof-" + p.record.Target + "-" + p.record.Run
	expectedKind, expectedName, expectedNS := "Namespace", namespace, ""
	query := "namespace"
	if index == 1 {
		expectedKind, expectedName, expectedNS, query = "CiliumNetworkPolicy", "restore-isolation", namespace, "ciliumnetworkpolicies.cilium.io"
		if identity := p.record.Resources["Namespace"]; identity.Name != namespace || identity.Namespace != "" || !isolationUID.MatchString(identity.UID) {
			return nil, refused
		}
		nativeNamespace, err := p.c.get(ctx, "namespace", "", namespace, false)
		if err != nil || benchmarkIdentity(nativeNamespace) != p.record.Resources["Namespace"] || value(nativeNamespace, "metadata", "deletionTimestamp") != nil || text(nativeNamespace, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || text(nativeNamespace, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(nativeNamespace, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
			return nil, refused
		}
	}
	if !isolationNamespace.MatchString(namespace) || text(desired, "kind") != expectedKind || text(desired, "metadata", "name") != expectedName || text(desired, "metadata", "namespace") != expectedNS || text(desired, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(desired, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return nil, refused
	}
	p.record.Phase = requested
	if err := p.save(ctx); err != nil {
		return nil, err
	}
	created, err := p.createOwned(ctx, desired)
	if err != nil {
		return nil, err
	}
	if !isolationUID.MatchString(text(created, "metadata", "uid")) || text(created, "apiVersion") != text(desired, "apiVersion") || text(created, "metadata", "namespace") != expectedNS || value(created, "metadata", "deletionTimestamp") != nil || text(created, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(created, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return nil, refused
	}
	if index == 0 && text(created, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || index == 1 && !recoveryJSONEqual(value(created, "spec"), value(desired, "spec")) {
		return nil, refused
	}
	if p.record.Resources == nil {
		p.record.Resources = map[string]benchmarkResource{}
	}
	p.record.Resources[key] = benchmarkIdentity(created)
	p.record.Phase = completed
	if err = p.save(ctx); err != nil {
		return nil, err
	}
	readback, err := p.c.get(ctx, query, expectedNS, expectedName, false)
	if err != nil || benchmarkIdentity(readback) != benchmarkIdentity(created) || value(readback, "metadata", "deletionTimestamp") != nil || !recoveryJSONEqual(value(created, "spec"), value(readback, "spec")) || !recoveryJSONEqual(value(created, "metadata", "labels"), value(readback, "metadata", "labels")) {
		return nil, errors.New("recovery environment creation could not be read back")
	}
	return readback, nil
}

// validCNPGEnvironmentTransition permits exactly one recorded creation at a time.
func validCNPGEnvironmentTransition(before, after cnpgProofRecord) bool {
	if before.Plan == nil || after.Plan == nil || len(before.Plan.Resources) != 4 {
		return false
	}
	for i, step := range []struct{ key, previous, requested, completed string }{
		{"Namespace", "restore-planned", "namespace-requested", "namespace-created"},
		{"Policy", "namespace-created", "policy-requested", "policy-created"},
	} {
		if before.Phase == step.previous && after.Phase == step.requested {
			return len(before.Resources) == i && recoveryJSONEqual(before.Resources, after.Resources)
		}
		if before.Phase == step.requested && after.Phase == step.completed {
			if len(before.Resources) != i || len(after.Resources) != i+1 {
				return false
			}
			identity := after.Resources[step.key]
			desired := before.Plan.Resources[i]
			return identity.Name == text(desired, "metadata", "name") && identity.Namespace == text(desired, "metadata", "namespace") && isolationUID.MatchString(identity.UID)
		}
	}
	return false
}
