package arcstaging_test

import (
	"strings"
	"testing"
)

const transportPath = "k8s/providers/hetzner/infrastructure/controllers/openbao/transport/"

const publicListenerHCL = `listener "tcp" {
  address = "0.0.0.0:8204"
  cluster_address = "127.0.0.1:8205"
  tls_cert_file = "/openbao/arc-tls/tls.crt"
  tls_key_file = "/openbao/arc-tls/tls.key"
  tls_min_version = "tls12"
  tls_disable_client_certs = true
}
`

func TestPublicListenerNativeScanRunsAfterPinnedKSailSetup(t *testing.T) {
	workflow := readYAML(t, ".github/workflows/ci.yaml")
	steps := field(t, workflow, "jobs", "validate", "steps").([]any)
	setup, scan, scanCount := -1, -1, 0
	for index, value := range steps {
		step := value.(map[string]any)
		run, _ := step["run"].(string)
		if strings.TrimSpace(run) == ".github/scripts/setup-ksail.sh" {
			if setup != -1 {
				t.Fatal("manifest validation must install one pinned KSail release")
			}
			setup = index
			version, ok := field(t, step, "env", "KSAIL_VERSION").(string)
			if !ok || version == "" {
				t.Fatal("the native scanner must use the job's pinned KSail release")
			}
			if _, conditional := step["if"]; conditional {
				t.Fatal("the native scanner's KSail installation must be unconditional")
			}
		}
		for _, line := range strings.Split(run, "\n") {
			if strings.TrimSpace(line) == "bash scripts/tests/test-arc-listener-public-content.sh" {
				scan, scanCount = index, scanCount+1
				equal(t, step["if"], "needs.changes.outputs.k8s == 'true'")
			}
		}
	}
	if setup < 0 || scanCount != 1 || scan <= setup {
		t.Fatal("the mandatory public-listener native scan must run once after pinned KSail installation")
	}
}

func TestListenerCredentialFalsePositiveRequiresPublicContentEnforcement(t *testing.T) {
	config := readYAML(t, transportPath+"config-map-listener.yaml")
	equal(t, field(t, config, "metadata", "name"), "arc-openbao-listener")
	equal(t, field(t, config, "metadata", "namespace"), "openbao")
	data := field(t, config, "data").(map[string]any)
	if len(data) != 1 {
		t.Fatal("the excepted ConfigMap may contain only the public listener")
	}
	equal(t, data["listener.hcl"], publicListenerHCL)
	if _, exists := config["binaryData"]; exists {
		t.Fatal("the excepted ConfigMap must not contain opaque data")
	}
	exception := readYAML(t, "k8s/bases/infrastructure/cluster-security-exceptions/arc-openbao-listener.yaml")
	resources := field(t, exception, "spec", "match", "resources").([]any)
	controls := field(t, exception, "spec", "posture").([]any)
	if len(resources) != 1 || len(controls) != 1 {
		t.Fatal("only this ConfigMap's credential-text false positive may be excepted")
	}
	equal(t, field(t, resources[0], "kind"), "ConfigMap")
	equal(t, field(t, resources[0], "name"), "^arc-openbao-listener$")
	equal(t, field(t, controls[0], "controlID"), "C-0012")
	equal(t, field(t, controls[0], "action"), "ignore")
	policy := readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener.yaml")
	spec := field(t, policy, "spec").(map[string]any)
	if len(spec) != 2 || spec["background"] != true {
		t.Fatal("listener admission must retain its default admission mode and background audit")
	}
	rules := field(t, policy, "spec", "rules").([]any)
	if len(rules) != 1 {
		t.Fatal("public listener admission must have exactly one enforced rule")
	}
	rule := rules[0].(map[string]any)
	if len(rule) != 3 || len(field(t, rule, "validate").(map[string]any)) != 3 {
		t.Fatal("listener admission must not add a bypass or action override")
	}
	for _, forbidden := range []string{"exclude", "preconditions"} {
		if _, exists := rule[forbidden]; exists {
			t.Fatalf("public listener admission must not bypass %s", forbidden)
		}
	}
	equal(t, field(t, rule, "validate", "failureAction"), "Enforce")
	match := field(t, rule, "match", "any").([]any)
	if len(match) != 1 {
		t.Fatal("listener admission must have one global resource match")
	}
	selector := field(t, match[0], "resources").(map[string]any)
	if len(selector) != 2 {
		t.Fatal("listener admission must cover its name in every namespace")
	}
	kinds := field(t, selector, "kinds").([]any)
	names := field(t, selector, "names").([]any)
	if len(kinds) != 1 || len(names) != 1 {
		t.Fatal("listener admission resource scope differs from its disposition")
	}
	equal(t, kinds[0], "ConfigMap")
	equal(t, names[0], "arc-openbao-listener")
	conditions := field(t, rule, "validate", "deny", "conditions", "any").([]any)
	expected := map[string]any{
		"{{ request.object.metadata.namespace || '' }}":         "openbao",
		"{{ length(keys(request.object.data || `{}`)) }}":       1,
		"{{ length(keys(request.object.binaryData || `{}`)) }}": 0,
		"{{ request.object.data.\"listener.hcl\" || '' }}":      publicListenerHCL,
	}
	if len(conditions) != len(expected) {
		t.Fatal("listener admission must enforce every public-content premise")
	}
	for _, condition := range conditions {
		key := field(t, condition, "key").(string)
		want, exists := expected[key]
		if !exists {
			t.Fatal("listener admission has an unexpected or duplicate condition")
		}
		equal(t, field(t, condition, "operator"), "NotEquals")
		equal(t, field(t, condition, "value"), want)
		delete(expected, key)
	}
	aggregate := readYAML(t, "k8s/bases/infrastructure/cluster-policies/kustomization.yaml")
	count := 0
	for _, resource := range field(t, aggregate, "resources").([]any) {
		if resource == "best-practices/restrict-arc-openbao-listener.yaml" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("both providers must deploy the public-content admission policy once")
	}
}

func TestCredentialStoreRequiresDedicatedVerifiedTLS(t *testing.T) {
	store := readYAML(t, "k8s/bases/infrastructure/actions-runners/credentials/secret-store.yaml")
	vault := field(t, store, "spec", "provider", "vault")
	equal(t, field(t, vault, "server"), "https://openbao-arc.openbao.svc.cluster.local:8204")
	equal(t, field(t, vault, "caProvider", "type"), "ConfigMap")
	equal(t, field(t, vault, "caProvider", "name"), "arc-openbao-ca")
	equal(t, field(t, vault, "caProvider", "key"), "ca.crt")
}

func TestCredentialTLSPoliciesUseCiliumIdentityVisibleSelectors(t *testing.T) {
	egressPolicy := readYAML(t, transportPath+"cilium-network-policy-external-secrets.yaml")
	egressRules := field(t, egressPolicy, "spec", "egress").([]any)
	if len(egressRules) != 1 {
		t.Fatal("credential egress must have exactly one rule")
	}
	egressRule := egressRules[0].(map[string]any)
	if _, exists := egressRule["toEndpoints"]; exists {
		t.Fatal("credential egress must not depend on pod labels excluded from Cilium identities")
	}
	services := field(t, egressRule, "toServices").([]any)
	if len(services) != 1 {
		t.Fatal("credential egress must target exactly one Kubernetes Service")
	}
	equal(t, field(t, services[0], "k8sService", "serviceName"), "openbao-arc")
	equal(t, field(t, services[0], "k8sService", "namespace"), "openbao")
	ports := field(t, egressRule, "toPorts").([]any)
	if len(ports) != 1 {
		t.Fatal("credential egress must expose exactly one port rule")
	}
	portEntries := field(t, ports[0], "ports").([]any)
	if len(portEntries) != 1 {
		t.Fatal("credential egress must expose exactly one port")
	}
	equal(t, field(t, portEntries[0], "port"), "8204")
	equal(t, field(t, portEntries[0], "protocol"), "TCP")

	ingressPolicy := readYAML(t, transportPath+"cilium-network-policy.yaml")
	selector := field(t, ingressPolicy, "spec", "endpointSelector", "matchLabels").(map[string]any)
	if len(selector) != 2 {
		t.Fatal("credential ingress must select OpenBao only by stable Cilium identity labels")
	}
	equal(t, selector["app.kubernetes.io/name"], "openbao")
	equal(t, selector["app.kubernetes.io/instance"], "openbao")
	for _, excluded := range []string{"statefulset.kubernetes.io/pod-name", "apps.kubernetes.io/pod-index"} {
		if _, exists := selector[excluded]; exists {
			t.Fatalf("credential ingress selector uses identity-excluded label %s", excluded)
		}
	}
}

func TestCredentialAuthenticationStagingDoesNotReadAppKeys(t *testing.T) {
	stage := readYAML(t, "k8s/providers/hetzner/infrastructure/arc-credential-transport/kustomization.yaml")
	resources := field(t, stage, "resources").([]any)
	if len(resources) != 1 {
		t.Fatal("credential staging must contain only its authentication component")
	}
	equal(t, resources[0], "../../../../bases/infrastructure/actions-runners/credentials/")
	credentials := readYAML(t, "k8s/bases/infrastructure/actions-runners/credentials/kustomization.yaml")
	resources = field(t, credentials, "resources").([]any)
	if len(resources) != 2 {
		t.Fatal("authentication staging must not include any key reader or runner")
	}
	equal(t, resources[0], "service-account.yaml")
	equal(t, resources[1], "secret-store.yaml")
}

func TestCredentialTLSDoesNotAdvanceTheHeldRaftRollout(t *testing.T) {
	canary := readYAML(t, "k8s/providers/hetzner/infrastructure/controllers/openbao/patches/standby-oidc-canary.yaml")
	patches := field(t, canary, "spec", "postRenderers").([]any)
	patch := field(t, patches[0], "kustomize", "patches").([]any)[0]
	text := field(t, patch, "patch").(string)
	if !strings.Contains(text, "partition: 2") {
		t.Fatal("TLS must not advance the held lower Raft ordinals")
	}
	service := readYAML(t, transportPath+"service.yaml")
	equal(t, field(t, service, "spec", "selector", "statefulset.kubernetes.io/pod-name"), "openbao-2")
	ports := field(t, service, "spec", "ports").([]any)
	if len(ports) != 1 {
		t.Fatal("credential service must expose only TLS")
	}
	equal(t, field(t, ports[0], "port"), 8204)
	equal(t, field(t, ports[0], "targetPort"), 8204)
}

func TestCredentialTLSHasNarrowCertificateAndTrust(t *testing.T) {
	certificate := readYAML(t, transportPath+"certificate.yaml")
	equal(t, field(t, certificate, "metadata", "namespace"), "openbao")
	equal(t, field(t, certificate, "spec", "issuerRef", "name"), "arc-openbao-ca")
	equal(t, field(t, certificate, "spec", "secretName"), "arc-openbao-server-tls")
	names := field(t, certificate, "spec", "dnsNames").([]any)
	if len(names) != 1 || names[0] != "openbao-arc.openbao.svc.cluster.local" {
		t.Fatal("certificate must bind the one credential service identity")
	}
	bundle := readYAML(t, transportPath+"bundle.yaml")
	equal(t, field(t, bundle, "metadata", "name"), "arc-openbao-ca")
	equal(t, field(t, bundle, "spec", "target", "configMap", "key"), "ca.crt")
	equal(t, field(t, bundle, "spec", "target", "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"), "arc-runners")
	sources := field(t, bundle, "spec", "sources").([]any)
	if len(sources) != 1 {
		t.Fatal("credential trust must not include unrelated authorities")
	}
	equal(t, field(t, sources[0], "secret", "name"), "arc-openbao-ca")
	equal(t, field(t, sources[0], "secret", "key"), "tls.crt")
}

func TestCredentialTLSAppendsListenerWithoutReplacingStorage(t *testing.T) {
	config := readYAML(t, transportPath+"config-map-listener.yaml")
	text := field(t, config, "data", "listener.hcl").(string)
	for _, fragment := range []string{"listener \"tcp\"", "0.0.0.0:8204", "127.0.0.1:8205", "tls_min_version = \"tls12\"", "tls_cert_file", "tls_key_file"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing native TLS setting %s", fragment)
		}
	}
	for _, forbidden := range []string{"tls_disable =", "storage ", "audit ", "api_addr", "cluster_addr ="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("supplemental listener changes existing server setting %s", forbidden)
		}
	}
	patch := readYAML(t, transportPath+"patches/enable-arc-transport.yaml")
	equal(t, field(t, patch, "spec", "values", "server", "extraArgs"), "-config=/openbao/arc-transport/listener.hcl")
}

func TestCertificateReloadDoesNotReceiveAPIOrKeyMounts(t *testing.T) {
	patch := readYAML(t, transportPath+"patches/enable-arc-transport.yaml")
	server := field(t, patch, "spec", "values", "server")
	renderers := field(t, patch, "spec", "postRenderers").([]any)
	patches := field(t, renderers[0], "kustomize", "patches").([]any)
	if !strings.Contains(field(t, patches[0], "patch").(string), "automountServiceAccountToken: false") {
		t.Fatal("the post-rendered Pod must disable API token injection")
	}
	containers := field(t, server, "extraContainers").([]any)
	if len(containers) != 1 {
		t.Fatal("expected only the native TLS reload helper")
	}
	reloader := containers[0].(map[string]any)
	if _, ok := reloader["volumeMounts"]; ok {
		t.Fatal("reload helper must not receive credential mounts")
	}
	equal(t, field(t, reloader, "securityContext", "runAsUser"), 100)
	equal(t, field(t, reloader, "securityContext", "allowPrivilegeEscalation"), false)
	equal(t, field(t, reloader, "securityContext", "readOnlyRootFilesystem"), true)
}
