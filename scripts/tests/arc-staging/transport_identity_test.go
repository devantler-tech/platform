package arcstaging_test

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// Cilium's default identity projection excludes generated ordinal labels.
// Keep the Kubernetes Service selector separate from this security identity.
func TestCredentialTLSSelectorsMatchCiliumSecurityIdentity(t *testing.T) {
	patch := readYAML(t, transportPath+"patches/enable-arc-transport.yaml")
	renderers := field(t, patch, "spec", "postRenderers").([]any)
	patches := field(t, renderers[0], "kustomize", "patches").([]any)
	var statefulSet map[string]any
	if err := yaml.Unmarshal([]byte(field(t, patches[0], "patch").(string)), &statefulSet); err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{
		"app.kubernetes.io/name":     "openbao",
		"app.kubernetes.io/instance": "openbao",
	}
	template := field(t, statefulSet, "spec", "template").(map[string]any)
	if metadata, ok := template["metadata"].(map[string]any); ok {
		if labels, ok := metadata["labels"].(map[string]any); ok {
			for key, value := range labels {
				switch key {
				case "statefulset.kubernetes.io/pod-name", "apps.kubernetes.io/pod-index", "controller-revision-hash":
					// These Pod labels are absent from native Cilium identities.
				default:
					identity[key] = value
				}
			}
		}
	}
	ingress := readYAML(t, transportPath+"cilium-network-policy.yaml")
	egress := readYAML(t, transportPath+"cilium-network-policy-external-secrets.yaml")
	equal(t, field(t, ingress, "metadata", "namespace"), "openbao")
	equal(t, field(t, egress, "metadata", "namespace"), "external-secrets")
	ingressSelector := field(t, ingress, "spec", "endpointSelector", "matchLabels").(map[string]any)
	rules := field(t, egress, "spec", "egress").([]any)
	if len(rules) != 1 {
		t.Fatal("TLS egress must retain one bounded rule")
	}
	destinations := field(t, rules[0], "toEndpoints").([]any)
	if len(destinations) != 1 {
		t.Fatal("TLS egress must retain one bounded destination")
	}
	egressSelector := field(t, destinations[0], "matchLabels").(map[string]any)
	identity["k8s:io.kubernetes.pod.namespace"] = "openbao"
	for name, selector := range map[string]map[string]any{"ingress": ingressSelector, "egress": egressSelector} {
		t.Run(name, func(t *testing.T) {
			if !transportIdentityMatches(selector, identity) {
				t.Fatal("TLS allow rule cannot match the declared Cilium security identity")
			}
			for _, key := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance", "platform.devantler.tech/arc-transport"} {
				without := make(map[string]any, len(identity))
				for label, value := range identity {
					if label != key {
						without[label] = value
					}
				}
				if transportIdentityMatches(selector, without) {
					t.Fatalf("TLS allow rule also matches a workload missing %s", key)
				}
			}
		})
	}
	otherNamespace := make(map[string]any, len(identity))
	for key, value := range identity {
		otherNamespace[key] = value
	}
	otherNamespace["k8s:io.kubernetes.pod.namespace"] = "arc-runners"
	if transportIdentityMatches(egressSelector, otherNamespace) {
		t.Fatal("TLS egress must not select another namespace")
	}
}

func transportIdentityMatches(selector, identity map[string]any) bool {
	for key, value := range selector {
		if identity[key] != value {
			return false
		}
	}
	return len(selector) > 0
}
