package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func runtimePublicationManifestFixture() object {
	return object{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": object{"name": "longhorn", "namespace": "longhorn-system"},
		"spec":     object{"chart": object{"spec": object{"chart": "longhorn", "version": "1.13.0", "sourceRef": object{"kind": "HelmRepository", "name": "longhorn"}}}, "values": object{}}}
}

func runtimePublicationStreamFixture(t *testing.T, manifest object) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, []byte("\n---\napiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata: {name: longhorn, namespace: longhorn-system}\nspec: {url: 'https://charts.longhorn.io'}\n")...)
}

func TestRuntimePublicationCandidateRejectsIndirectOrAmbiguousImageSelection(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(object)
	}{
		{"chart defaults", runtimeStockImage, nil},
		{"reviewed immutable image", runtimeRetirementImage, func(o object) {
			valueObject(o, "spec", "values")["image"] = object{"longhorn": object{"instanceManager": object{
				"registry": "ghcr.io", "repository": "devantler-tech/platform-longhorn-instance-manager",
				"tag": "v1.13.0-retirement.1-591ed0ccebd032c4d9705c356c267e724b30e2a9-1@sha256:4f7c2ff5666c6ce1ccab78a5e8c5c108147b12b80729303b9f1d91dfb9ee9cd7"}}}
		}},
		{"empty values references", runtimeStockImage, func(o object) { valueObject(o, "spec")["valuesFrom"] = []any{} }},
		{"external values override", "", func(o object) {
			valueObject(o, "spec")["valuesFrom"] = []any{object{"kind": "ConfigMap", "name": "override", "targetPath": "image.longhorn.instanceManager.tag"}}
		}},
		{"registry override", "", func(o object) {
			valueObject(o, "spec", "values")["global"] = object{"imageRegistry": "unreviewed.example"}
		}},
		{"private registry override", "", func(o object) {
			valueObject(o, "spec", "values")["privateRegistry"] = object{"registryUrl": "unreviewed.example"}
		}},
		{"unreviewed tag", "", func(o object) {
			valueObject(o, "spec", "values")["image"] = object{"longhorn": object{"instanceManager": object{"tag": "latest"}}}
		}},
		{"nonliteral tag", "", func(o object) {
			valueObject(o, "spec", "values")["image"] = object{"longhorn": object{"instanceManager": object{"tag": "${image_tag}"}}}
		}},
		{"malformed values", "", func(o object) { valueObject(o, "spec")["values"] = "indirect" }},
		{"malformed default settings", "", func(o object) { valueObject(o, "spec", "values")["defaultSettings"] = "indirect" }},
		{"preserved release values", "", func(o object) { valueObject(o, "spec")["upgrade"] = object{"preserveValues": true} }},
		{"alternate chart producer", "", func(o object) { valueObject(o, "spec", "chart")["kind"] = "OCIRepository" }},
		{"redirected release", "", func(o object) { valueObject(o, "spec")["targetNamespace"] = "other" }},
		{"different chart", "", func(o object) { valueObject(o, "spec", "chart", "spec")["version"] = "1.13.1" }},
		{"different values file", "", func(o object) { valueObject(o, "spec", "chart", "spec")["valuesFiles"] = []any{"override.yaml"} }},
		{"namespace override", "", func(o object) { valueObject(o, "spec", "values")["namespaceOverride"] = "other" }},
		{"alternate source", "", func(o object) { valueObject(o, "spec", "chart", "spec", "sourceRef")["kind"] = "GitRepository" }},
		{"manager image post-render override", "", func(o object) {
			valueObject(o, "spec")["postRenderers"] = []any{object{"kustomize": object{"patches": []any{object{
				"target": object{"kind": "DaemonSet", "name": "longhorn-manager"},
				"patch":  "- op: add\n  path: /spec/template/spec/containers/0/env/-\n  value: {name: DEFAULT_INSTANCE_MANAGER_IMAGE, value: unreviewed/image:latest}\n",
			}}}}}
		}},
		{"security-only post-render", runtimeStockImage, func(o object) {
			valueObject(o, "spec")["postRenderers"] = []any{object{"kustomize": object{"patches": []any{object{
				"target": object{"kind": "DaemonSet", "name": "longhorn-manager"},
				"patch":  "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: longhorn-manager}\nspec:\n  template:\n    spec:\n      securityContext: {fsGroupChangePolicy: OnRootMismatch}\n      containers:\n        - name: longhorn-manager\n          securityContext: {seLinuxOptions: {}}\n",
			}}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := runtimePublicationManifestFixture()
			if tc.change != nil {
				tc.change(manifest)
			}
			raw := runtimePublicationStreamFixture(t, manifest)
			got, err := runtimePublicationCandidate(raw)
			if tc.want == "" {
				if err == nil || got != "" {
					t.Fatal("ambiguous image selection was accepted")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("candidate image = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRuntimePublicationChartSourceMustBeUnambiguous(t *testing.T) {
	raw := runtimePublicationStreamFixture(t, runtimePublicationManifestFixture())
	for _, data := range [][]byte{
		[]byte(strings.ReplaceAll(string(raw), "https://charts.longhorn.io", "https://unreviewed.example")),
		append(append([]byte{}, raw...), []byte("\n---\napiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata: {name: longhorn, namespace: longhorn-system}\nspec: {url: 'https://charts.longhorn.io'}\n")...),
		[]byte(strings.ReplaceAll(string(raw), "spec: {url:", "spec: {secretRef: {name: indirect}, url:")),
	} {
		if got, err := runtimePublicationCandidate(data); err == nil || got != "" {
			t.Fatal("ambiguous chart source accepted")
		}
	}
}

func TestRuntimePublicationCandidateRejectsIncompleteStreams(t *testing.T) {
	raw, err := json.Marshal(runtimePublicationManifestFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("kind: ["), []byte("kind: HelmRelease\nkind: Other\n"), append(append(raw, []byte("\n---\n")...), raw...),
		append(append(raw, []byte("\n---\n")...), []byte("apiVersion: longhorn.io/v1beta2\nkind: Setting\nmetadata: {name: default-instance-manager-image, namespace: longhorn-system}\nvalue: unreviewed/image:latest\n")...)} {
		if got, err := runtimePublicationCandidate(data); err == nil || got != "" {
			t.Fatal("missing, duplicated or indirect runtime declaration accepted")
		}
	}
}
