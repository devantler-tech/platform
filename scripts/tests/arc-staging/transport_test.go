package arcstaging_test

import (
	"strings"
	"testing"
)

const transportPath = "k8s/providers/hetzner/infrastructure/controllers/openbao/transport/"

func TestCredentialStoreRequiresDedicatedVerifiedTLS(t *testing.T) {
	store := readYAML(t, "k8s/bases/infrastructure/actions-runners/secret-store.yaml")
	vault := field(t, store, "spec", "provider", "vault")
	equal(t, field(t, vault, "server"), "https://openbao-arc.openbao.svc.cluster.local:8204")
	equal(t, field(t, vault, "caProvider", "type"), "ConfigMap")
	equal(t, field(t, vault, "caProvider", "name"), "arc-openbao-ca")
	equal(t, field(t, vault, "caProvider", "key"), "ca.crt")
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
	bundle := readYAML(t, transportPath+"trust-bundle.yaml")
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
	config := readYAML(t, transportPath+"listener-config-map.yaml")
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
	patch := readYAML(t, transportPath+"helm-release-patch.yaml")
	equal(t, field(t, patch, "spec", "values", "server", "extraArgs"), "-config=/openbao/arc-transport/listener.hcl")
}

func TestCertificateReloadDoesNotReceiveAPIOrKeyMounts(t *testing.T) {
	patch := readYAML(t, transportPath+"helm-release-patch.yaml")
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
