package main

import (
	"encoding/json"
	"reflect"
)

func runtimePublicationRoots() map[string]string {
	return map[string]string{
		"bootstrap": "clusters/prod/bootstrap", "flux-instance": "providers/hetzner/infrastructure/controllers/flux-instance",
		"infrastructure-controllers": "providers/hetzner/infrastructure/controllers", "infrastructure": "providers/hetzner/infrastructure",
		"apps": "providers/hetzner/apps", "platform-health": "providers/hetzner/health",
		"github-broker":         "providers/hetzner/apps/github-broker/server",
		"business-site-preview": "providers/hetzner/business-site-preview-workload",
	}
}

// A raw directory render is meaningful only when the actual producer and Flux
// wrappers select it without another layer of image/namespace transformation.
func runtimePublicationSource(config, wrappers []byte) error {
	encoded, err := runtimeSingleYAML(config)
	var cfg object
	if err != nil || json.Unmarshal(encoded, &cfg) != nil || text(cfg, "spec", "workload", "sourceDirectory") != "k8s" ||
		text(cfg, "spec", "workload", "kustomizationFile") != "clusters/prod" || text(cfg, "spec", "workload", "tag") != "latest" ||
		text(cfg, "spec", "cluster", "localRegistry", "registry") != "ghcr.io/devantler-tech/platform/manifests" {
		return errRuntimePublication
	}
	documents, err := runtimePublicationDocuments(wrappers)
	if err != nil {
		return err
	}
	expected, seen := runtimePublicationRoots(), map[string]bool{}
	for _, o := range documents {
		name := text(o, "metadata", "name")
		path, known := expected[name]
		if !known || seen[name] || text(o, "apiVersion") != "kustomize.toolkit.fluxcd.io/v1" || text(o, "kind") != "Kustomization" ||
			text(o, "metadata", "namespace") != "flux-system" || text(o, "spec", "path") != path ||
			text(o, "spec", "sourceRef", "kind") != "OCIRepository" || text(o, "spec", "sourceRef", "name") != "flux-system" {
			return errRuntimePublication
		}
		if ns := value(o, "spec", "sourceRef", "namespace"); ns != nil && ns != "flux-system" {
			return errRuntimePublication
		}
		for _, field := range []string{"patches", "components", "images", "namePrefix", "nameSuffix", "targetNamespace", "kubeConfig"} {
			if value(o, "spec", field) != nil {
				return errRuntimePublication
			}
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return errRuntimePublication
	}
	return nil
}

func runtimePublicationFluxSource(render []byte) error {
	documents, err := runtimePublicationDocuments(render)
	if err != nil {
		return err
	}
	instances := 0
	for _, o := range documents {
		if text(o, "metadata", "namespace") != "flux-system" {
			continue
		}
		// The operator owns the root source and root Kustomization. A second
		// declarative writer could redirect production after this render.
		if (text(o, "kind") == "OCIRepository" || text(o, "kind") == "Kustomization") && text(o, "metadata", "name") == "flux-system" {
			return errRuntimePublication
		}
		if text(o, "kind") != "FluxInstance" {
			continue
		}
		instances++
		if instances != 1 || text(o, "apiVersion") != "fluxcd.controlplane.io/v1" || text(o, "metadata", "name") != "flux" {
			return errRuntimePublication
		}
		if sync := value(o, "spec", "sync"); sync != nil && !reflect.DeepEqual(sync, object{"kind": "OCIRepository", "url": "oci://ghcr.io/devantler-tech/platform/manifests", "ref": "latest", "path": "./clusters/prod"}) {
			return errRuntimePublication
		}
		patches, ok := value(o, "spec", "kustomize", "patches").([]any)
		if !ok {
			return errRuntimePublication
		}
		rootPatches := 0
		for _, raw := range patches {
			patch, ok := raw.(object)
			kind := text(patch, "target", "kind")
			if !ok || kind == "" || kind == "Kustomization" {
				return errRuntimePublication
			}
			if kind != "OCIRepository" {
				continue
			}
			rootPatches++
			if rootPatches != 1 || !runtimeOnlyKeys(patch, "target", "patch") || !reflect.DeepEqual(value(patch, "target"), object{"kind": "OCIRepository", "name": "flux-system", "namespace": "flux-system"}) {
				return errRuntimePublication
			}
			encoded, err := runtimeSingleYAML([]byte(text(patch, "patch")))
			var operations []object
			if err != nil || json.Unmarshal(encoded, &operations) != nil || len(operations) != 1 || !runtimeOnlyKeys(operations[0], "op", "path", "value") || text(operations[0], "op") != "add" || text(operations[0], "path") != "/spec/verify" {
				return errRuntimePublication
			}
			if _, ok := value(operations[0], "value").(object); !ok {
				return errRuntimePublication
			}
		}
		if rootPatches != 1 {
			return errRuntimePublication
		}
	}
	if instances != 1 {
		return errRuntimePublication
	}
	return nil
}
