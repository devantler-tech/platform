package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"github.com/devantler-tech/platform/internal/strictjson"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Inspect the effective controllers render, rather than a patch that might not
// be included. Unknown indirection holds publication; it is never a stock-image
// fallback. The reviewed chart has only security-context post-render patches.
func runtimePublicationCandidate(data []byte) (string, error) {
	documents, err := runtimePublicationDocuments(data)
	if err != nil {
		return "", err
	}
	image := ""
	repository := false
	for _, o := range documents {
		if text(o, "kind") == "Setting" && text(o, "metadata", "namespace") == "longhorn-system" && text(o, "metadata", "name") == "default-instance-manager-image" {
			return "", errRuntimePublication
		}
		if text(o, "kind") == "HelmRepository" && text(o, "metadata", "namespace") == "longhorn-system" && text(o, "metadata", "name") == "longhorn" {
			spec, ok := value(o, "spec").(object)
			if repository || !ok || text(o, "apiVersion") != "source.toolkit.fluxcd.io/v1" || text(spec, "url") != "https://charts.longhorn.io" || !runtimeOnlyKeys(spec, "url", "interval", "timeout") {
				return "", errRuntimePublication
			}
			repository = true
		}
		if text(o, "kind") != "HelmRelease" || text(o, "metadata", "name") != "longhorn" || text(o, "metadata", "namespace") != "longhorn-system" {
			continue
		}
		if image != "" {
			return "", errRuntimePublication
		}
		image, err = runtimePublicationRelease(o)
		if err != nil {
			return "", err
		}
	}
	if image == "" || !repository {
		return "", errRuntimePublication
	}
	return image, nil
}

func runtimePublicationDocuments(data []byte) ([]object, error) {
	documents, err := runtimeYAMLDocuments(data)
	if err != nil {
		return nil, err
	}
	var result []object
	for _, encoded := range documents {
		var o object
		if json.Unmarshal(encoded, &o) != nil || o == nil {
			return nil, errRuntimePublication
		}
		result = append(result, o)
	}
	return result, nil
}

func runtimeYAMLDocuments(data []byte) ([][]byte, error) {
	if len(data) == 0 || len(data) > 16<<20 {
		return nil, errRuntimePublication
	}
	reader := kyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var result [][]byte
	for {
		raw, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errRuntimePublication
		}
		encoded, err := yaml.YAMLToJSONStrict(raw)
		if err != nil || strictjson.Validate(encoded, nil) != nil {
			return nil, errRuntimePublication
		}
		// YAMLReader exposes comments preceding an initial document marker
		// as an empty document. Empty documents have no declarative effect.
		if bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
			continue
		}
		result = append(result, encoded)
	}
	if len(result) == 0 {
		return nil, errRuntimePublication
	}
	return result, nil
}

func runtimePublicationRelease(o object) (string, error) {
	refused := func() (string, error) { return "", errRuntimePublication }
	if kind := value(o, "spec", "chart", "kind"); kind != nil && kind != "HelmChart" {
		return refused()
	}
	if preserve := value(o, "spec", "upgrade", "preserveValues"); preserve != nil && preserve != false {
		return refused()
	}
	for _, path := range [][]string{{"spec", "values"}, {"spec", "values", "defaultSettings"}, {"spec", "upgrade"}} {
		if raw := value(o, path...); raw != nil {
			if _, ok := raw.(object); !ok {
				return refused()
			}
		}
	}
	if text(o, "apiVersion") != "helm.toolkit.fluxcd.io/v2" ||
		text(o, "spec", "chart", "spec", "chart") != "longhorn" ||
		text(o, "spec", "chart", "spec", "version") != "1.13.0" ||
		text(o, "spec", "chart", "spec", "sourceRef", "kind") != "HelmRepository" ||
		text(o, "spec", "chart", "spec", "sourceRef", "name") != "longhorn" {
		return refused()
	}
	for _, path := range [][]string{{"spec", "chartRef"}, {"spec", "kubeConfig"}, {"spec", "commonMetadata"},
		{"spec", "chart", "spec", "valuesFiles"}, {"spec", "values", "namespaceOverride"},
		{"spec", "values", "global"}, {"spec", "values", "privateRegistry"}, {"spec", "values", "defaultSettings", "defaultInstanceManagerImage"}} {
		if value(o, path...) != nil {
			return refused()
		}
	}
	for _, path := range [][]string{{"spec", "targetNamespace"}, {"spec", "storageNamespace"}, {"spec", "releaseName"}, {"spec", "chart", "spec", "sourceRef", "namespace"}} {
		v := value(o, path...)
		expected := "longhorn-system"
		if path[len(path)-1] == "releaseName" {
			expected = "longhorn"
		}
		if v != nil && v != "" && v != expected {
			return refused()
		}
	}
	if v := value(o, "spec", "valuesFrom"); v != nil {
		refs, ok := v.([]any)
		if !ok || len(refs) != 0 {
			return refused()
		}
	}
	if !runtimeSecurityPostRenderers(value(o, "spec", "postRenderers")) {
		return refused()
	}
	image := runtimeStockImage
	if v := value(o, "spec", "values", "image"); v != nil {
		// Other chart images are outside this selector; each intermediate level
		// must still be a mapping, so malformed overrides cannot look absent.
		images, ok := v.(object)
		if !ok {
			return refused()
		}
		if raw, exists := images["longhorn"]; exists {
			longhorn, ok := raw.(object)
			if !ok {
				return refused()
			}
			if raw, exists := longhorn["instanceManager"]; exists {
				im, ok := raw.(object)
				if !ok || !runtimeOnlyKeys(im, "registry", "repository", "tag") || len(im) != 3 {
					return refused()
				}
				image = text(im, "registry") + "/" + text(im, "repository") + ":" + text(im, "tag")
			}
		}
	}
	if image != runtimeStockImage && image != runtimeRetirementImage {
		return refused()
	}
	return image, nil
}

func runtimeOnlyKeys(o object, keys ...string) bool {
	for key := range o {
		found := false
		for _, allowed := range keys {
			found = found || key == allowed
		}
		if !found {
			return false
		}
	}
	return true
}

func runtimeSecurityPostRenderers(raw any) bool {
	if raw == nil {
		return true
	}
	renderers, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, raw := range renderers {
		renderer, ok := raw.(object)
		kustomize, kok := value(renderer, "kustomize").(object)
		patches, pok := value(kustomize, "patches").([]any)
		if !ok || !kok || !pok || !runtimeOnlyKeys(renderer, "kustomize") || !runtimeOnlyKeys(kustomize, "patches") {
			return false
		}
		for _, raw := range patches {
			patch, ok := raw.(object)
			target, tok := value(patch, "target").(object)
			var document object
			encoded, err := runtimeSingleYAML([]byte(text(patch, "patch")))
			if !ok || !tok || !runtimeOnlyKeys(patch, "target", "patch") || !runtimeOnlyKeys(target, "kind", "name") ||
				err != nil || strictjson.Validate(encoded, nil) != nil || json.Unmarshal(encoded, &document) != nil ||
				!runtimeSecurityPatch(document, target) {
				return false
			}
		}
	}
	return true
}

func runtimeSingleYAML(data []byte) ([]byte, error) {
	documents, err := runtimeYAMLDocuments(data)
	if err != nil || len(documents) != 1 {
		return nil, errRuntimePublication
	}
	return documents[0], nil
}

func runtimeSecurityPatch(o, target object) bool {
	metadata, mok := value(o, "metadata").(object)
	spec, sok := value(o, "spec").(object)
	template, tok := value(spec, "template").(object)
	pod, pok := value(template, "spec").(object)
	if !mok || !sok || !tok || !pok || text(o, "apiVersion") != "apps/v1" || text(o, "kind") != text(target, "kind") ||
		text(metadata, "name") == "" || text(metadata, "name") != text(target, "name") ||
		!runtimeOnlyKeys(o, "apiVersion", "kind", "metadata", "spec") || !runtimeOnlyKeys(metadata, "name") ||
		!runtimeOnlyKeys(spec, "template") || !runtimeOnlyKeys(template, "spec") || !runtimeOnlyKeys(pod, "securityContext", "containers", "initContainers") {
		return false
	}
	if raw, exists := pod["securityContext"]; exists {
		if _, ok := raw.(object); !ok {
			return false
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		if raw, exists := pod[field]; exists {
			containers, ok := raw.([]any)
			if !ok {
				return false
			}
			for _, raw := range containers {
				container, ok := raw.(object)
				_, security := value(container, "securityContext").(object)
				if !ok || !security || text(container, "name") == "" || !runtimeOnlyKeys(container, "name", "securityContext") {
					return false
				}
			}
		}
	}
	return true
}
