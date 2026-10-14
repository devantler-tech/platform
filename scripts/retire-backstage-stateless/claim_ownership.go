package main

import (
	"errors"
	"strings"
)

// Controller-created claims are not Flux prune targets. Prove that ownership
// and absence from every complete Flux inventory instead of requiring an opt-out
// annotation. Physical Retain and the captured-identity admission fence remain
// independent prerequisites; no claim metadata is changed or normalized here.
func retainedClaimOwnership(s snapshot, claim map[string]any) error {
	if claim["apiVersion"] != "v1" || field(claim, "status", "phase") != "Bound" || text(field(claim, "spec", "volumeName")) == "" {
		return errors.New("retained claim binding unverified")
	}
	if raw := field(claim, "metadata", "annotations"); raw != nil {
		annotations, ok := raw.(map[string]any)
		if !ok {
			return errors.New("retained claim metadata malformed")
		}
		if prune, present := annotations["kustomize.toolkit.fluxcd.io/prune"]; present && prune != "disabled" {
			return errors.New("retained claim prune metadata conflicts with preservation")
		}
	}
	profile, err := controllerProfile(s, claim)
	if err != nil || profile != "cnpg-role" {
		return errors.New("retained database claim ownership unverified")
	}
	owners, err := ownerReferences(claim)
	if err != nil || len(owners) != 1 || owners[0]["apiVersion"] != "postgresql.cnpg.io/v1" {
		return errors.New("retained claim owner API unverified")
	}
	layers, err := members(s.layers)
	if err != nil || len(layers) == 0 {
		return errors.New("retained claim inventory coverage incomplete")
	}
	claimID := "backstage_" + text(field(claim, "metadata", "name")) + "__PersistentVolumeClaim"
	for _, layer := range layers {
		entries, ok := field(layer, "status", "inventory", "entries").([]any)
		if layer["apiVersion"] != "kustomize.toolkit.fluxcd.io/v1" || layer["kind"] != "Kustomization" || !ready(layer) || !ok || len(entries) == 0 {
			return errors.New("retained claim inventory unsettled")
		}
		for _, entry := range entries {
			id := text(field(object(entry), "id"))
			parts := strings.Split(id, "_")
			// Flux parses namespace first and group/kind from the right: RBAC
			// names may contain encoded colons and literal underscores.
			if len(parts) < 4 || strings.Join(parts[1:len(parts)-2], "_") == "" || parts[len(parts)-1] == "" {
				return errors.New("retained claim inventory identity incomplete")
			}
			if id == claimID {
				return errors.New("retained claim is a Flux prune target")
			}
		}
	}
	return nil
}
