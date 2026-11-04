package trivyruntimedispositions

import (
	"fmt"
	"strings"
)

func checkWorldRenderedOwner(key string, obj map[string]any) error {
	meta, _ := obj["metadata"].(map[string]any)
	kind, _ := obj["kind"].(string)
	related := meta["namespace"] == "world-at-ruin" || kind == "Namespace" && meta["name"] == "world-at-ruin"
	spec, _ := obj["spec"].(map[string]any)
	if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
		ref, _ := obj["roleRef"].(map[string]any)
		related = related || ref["name"] == "world-at-ruin-zone-trial-operator"
		subjects, _ := obj["subjects"].([]any)
		for _, v := range subjects {
			subject, _ := v.(map[string]any)
			related = related || subject["kind"] == "ServiceAccount" && subject["namespace"] == "world-at-ruin"
			related = related || subject["kind"] == "Group" && subject["name"] == "system:serviceaccounts:world-at-ruin"
			if subject["kind"] == "User" {
				name, _ := subject["name"].(string)
				related = related || strings.HasPrefix(name, "system:serviceaccount:world-at-ruin:")
			}
		}
	}
	if kind == "OCIRepository" {
		url, _ := spec["url"].(string)
		related = related || strings.Contains(url, "ghcr.io/devantler-tech/world-at-ruin/zone-manifests")
	}
	if kind == "Kustomization" || kind == "HelmRelease" {
		related = related || spec["targetNamespace"] == "world-at-ruin" || spec["serviceAccountName"] == "world-at-ruin"
	}
	if kind == "ClusterPolicy" || kind == "Policy" {
		related = related || targetsWorld(spec)
	}
	if related && !effectiveKeys[key] {
		return fmt.Errorf("%s: unreviewed effective World trial resource/grant/consumer", key)
	}
	return nil
}
func targetsWorld(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if v["namespace"] == "world-at-ruin" || v["targetNamespace"] == "world-at-ruin" {
			return true
		}
		if ns, ok := v["namespaces"].([]any); ok {
			for _, n := range ns {
				if n == "world-at-ruin" {
					return true
				}
			}
		}
		for _, child := range v {
			if targetsWorld(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if targetsWorld(child) {
				return true
			}
		}
	}
	return false
}
