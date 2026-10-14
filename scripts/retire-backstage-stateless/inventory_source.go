package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// layerSourceKey resolves only characterized Flux source kinds and bounded
// Kubernetes identities, including the default namespace on a local reference.
func layerSourceKey(layer map[string]any) (string, error) {
	kind, name := text(field(layer, "spec", "sourceRef", "kind")), text(field(layer, "spec", "sourceRef", "name"))
	namespace := text(field(layer, "spec", "sourceRef", "namespace"))
	if namespace == "" {
		namespace = text(field(layer, "metadata", "namespace"))
	}
	if kind != "OCIRepository" && kind != "GitRepository" || len(name) > 253 || len(namespace) > 63 || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`).MatchString(name) || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(namespace) {
		return "", errors.New("inventory source reference unverified")
	}
	return kind + "/" + namespace + "/" + name, nil
}

// collectLayerSources captures each inventory's own source, rather than treating
// independent tenant publications as if they shared the platform root's digest.
func (e engine) collectLayerSources(ctx context.Context, s *snapshot) error {
	layers, err := members(s.layers)
	if err != nil {
		return err
	}
	s.layerSources = map[string]map[string]any{"OCIRepository/flux-system/flux-system": s.source}
	for _, layer := range layers {
		key, err := layerSourceKey(layer)
		if err != nil {
			return err
		}
		if _, found := s.layerSources[key]; !found {
			parts := strings.Split(key, "/")
			source, err := e.read(ctx, "--namespace="+parts[1], "get", strings.ToLower(parts[0]), parts[2], "--output=json")
			if err != nil {
				return errors.New("inventory source unavailable")
			}
			s.layerSources[key] = source
		}
	}
	return nil
}

// currentLayerInventory requires exact source identity, current-generation
// readiness and an applied revision equal to that source's current artifact.
func currentLayerInventory(s snapshot, layer map[string]any) bool {
	key, err := layerSourceKey(layer)
	if err != nil {
		return false
	}
	source := s.layerSources[key]
	parts := strings.Split(key, "/")
	revision := text(field(source, "status", "artifact", "revision"))
	return metadataValid(source) && source["apiVersion"] == "source.toolkit.fluxcd.io/v1" && source["kind"] == parts[0] && field(source, "metadata", "namespace") == parts[1] && field(source, "metadata", "name") == parts[2] && ready(source) && revision != "" && field(layer, "status", "lastAppliedRevision") == revision
}
