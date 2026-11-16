package bookkeeper_test

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func render(t *testing.T, paths ...string) []map[string]any {
	t.Helper()
	path := "k8s/providers/hetzner/apps"
	if len(paths) > 0 {
		path = paths[0]
	}
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(root(), path)).CombinedOutput()
	if err != nil {
		t.Fatalf("render production apps: %v: %s", err, out)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	var objects []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return objects
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
}

func field(t *testing.T, object map[string]any, keys ...string) any {
	t.Helper()
	var current any = object
	for _, key := range keys {
		mapping, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%v: expected mapping at %s, got %T", keys, key, current)
		}
		current = mapping[key]
	}
	return current
}

func object(t *testing.T, objects []map[string]any, kind, namespace, name string) map[string]any {
	t.Helper()
	for _, item := range objects {
		metadata, _ := item["metadata"].(map[string]any)
		if item["kind"] == kind && metadata["namespace"] == namespace && metadata["name"] == name {
			return item
		}
	}
	t.Fatalf("missing %s %s/%s in production render", kind, namespace, name)
	return nil
}

func TestCollectorsHaveSeparateConfigurationAndStaySuspended(t *testing.T) {
	for environment, namespace := range map[string]string{"prod": "bookkeeper", "dev": "bookkeeper-dev"} {
		t.Run(environment, func(t *testing.T) {
			objects := render(t, "k8s/providers/hetzner/apps/"+namespace)
			release := object(t, objects, "HelmRelease", namespace, "bookkeeper")
			if field(t, release, "spec", "values", "suspend") != true {
				t.Fatal("collection must remain suspended until mailbox and company acceptance")
			}
			if field(t, release, "spec", "values", "image", "digest") != "sha256:90904b258bed9c66b56d5c32b78d1b85f10c2bc2690a0210cc77f305059c6711" {
				t.Fatal("collector image must be the verified publication")
			}
			if field(t, release, "spec", "values", "securityContext", "runAsNonRoot") != true {
				t.Fatal("container must independently satisfy pod-security admission")
			}
			if field(t, release, "spec", "values", "existingSecret", "name") != "bookkeeper" {
				t.Fatal("collector must mount only its namespace-local credential Secret")
			}
			refs := field(t, release, "spec", "valuesFrom").([]any)
			got := map[string]string{}
			for _, item := range refs {
				ref := item.(map[string]any)
				if ref["kind"] != "Secret" || ref["name"] != "bookkeeper" || ref["optional"] == true {
					t.Fatal("mailbox and start date must come from required local configuration")
				}
				got[ref["targetPath"].(string)] = ref["valuesKey"].(string)
			}
			if !reflect.DeepEqual(got, map[string]string{"mail.user": "mail-user", "since": "since"}) {
				t.Fatalf("unexpected runtime configuration projection: %v", got)
			}
			secret := object(t, objects, "ExternalSecret", namespace, "bookkeeper")
			sources := field(t, secret, "spec", "dataFrom").([]any)
			if len(sources) != 1 || field(t, sources[0].(map[string]any), "extract", "key") != "apps/bookkeeper/"+environment {
				t.Fatal("dev and prod must read disjoint OpenBao entries")
			}
			if field(t, secret, "spec", "secretStoreRef", "kind") != "SecretStore" {
				t.Fatal("collector must never use the shared cluster credential reader")
			}
			store := object(t, objects, "SecretStore", namespace, "openbao")
			if field(t, store, "spec", "provider", "vault", "auth", "kubernetes", "role") != "bookkeeper-"+environment {
				t.Fatal("each environment needs its own auth role")
			}
			sa := object(t, objects, "ServiceAccount", namespace, "bookkeeper-secret-reader")
			if sa["automountServiceAccountToken"] != false {
				t.Fatal("secret-reader identity must not automatically mount API credentials")
			}
		})
	}
}

func TestCollectorChartTrustIsImmutableAndRepositorySpecific(t *testing.T) {
	for _, namespace := range []string{"bookkeeper", "bookkeeper-dev"} {
		source := object(t, render(t, "k8s/providers/hetzner/apps/"+namespace), "OCIRepository", namespace, "bookkeeper")
		if field(t, source, "spec", "url") != "oci://ghcr.io/devantler-tech/charts/bookkeeper" ||
			field(t, source, "spec", "ref", "digest") != "sha256:9f0858b8d0cdb9eb16d401e72fd7d8dba0748eda763673442d0e8b3c0494f170" {
			t.Fatal("chart source must select exactly the verified immutable chart")
		}
		identities := field(t, source, "spec", "verify", "matchOIDCIdentity").([]any)
		if len(identities) != 1 || field(t, source, "spec", "verify", "provider") != "cosign" {
			t.Fatal("chart must require one exact keyless signing identity")
		}
		identity := identities[0].(map[string]any)
		issuer := regexp.MustCompile(identity["issuer"].(string))
		subject := regexp.MustCompile(identity["subject"].(string))
		if !issuer.MatchString("https://token.actions.githubusercontent.com") || issuer.MatchString("https://token.actions.githubusercontent.com.attacker.example") {
			t.Fatal("issuer must bind GitHub's exact token service")
		}
		if !subject.MatchString("https://github.com/devantler-tech/bookkeeper/.github/workflows/publish.yaml@refs/tags/v1.0.0") {
			t.Fatal("verified chart signer is refused")
		}
		for _, refused := range []string{
			"https://github.com/devantler-tech/bookkeeper/.github/workflows/publish.yaml@refs/heads/main",
			"https://github.com/devantler-tech/other/.github/workflows/publish.yaml@refs/tags/v1.0.0",
			"https://github.com/devantler-tech/bookkeeper/.github/workflows/publish.yaml@refs/tags/v1.0.1",
		} {
			if subject.MatchString(refused) {
				t.Fatalf("chart trust admits %s", refused)
			}
		}
	}
}

func TestCollectorEgressIsLimitedToMailNuminaAndDNS(t *testing.T) {
	for _, namespace := range []string{"bookkeeper", "bookkeeper-dev"} {
		objects := render(t, "k8s/providers/hetzner/apps/"+namespace)
		deny := object(t, objects, "NetworkPolicy", namespace, "default-deny")
		if !reflect.DeepEqual(field(t, deny, "spec", "policyTypes"), []any{"Ingress", "Egress"}) {
			t.Fatal("every collector pod must start with ingress and egress denied")
		}
		policy := object(t, objects, "CiliumNetworkPolicy", namespace, "bookkeeper")
		if field(t, policy, "spec", "endpointSelector", "matchLabels", "app.kubernetes.io/name") != "bookkeeper" {
			t.Fatal("egress grant must apply only to collector pods")
		}
		rules := field(t, policy, "spec", "egress").([]any)
		if len(rules) != 2 {
			t.Fatal("collector must have exactly HTTPS destinations and restricted DNS")
		}
		https := rules[0].(map[string]any)
		wantNames := []any{map[string]any{"matchName": "mail.${domain}"}, map[string]any{"matchName": "api.numina.app"}}
		if len(https) != 2 || !reflect.DeepEqual(https["toFQDNs"], wantNames) ||
			!reflect.DeepEqual(https["toPorts"], []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}}) {
			t.Fatal("HTTPS egress must admit only the mail service and Numina on 443")
		}
		dns := rules[1].(map[string]any)
		if len(dns) != 2 || !reflect.DeepEqual(dns["toEndpoints"], []any{map[string]any{"matchLabels": map[string]any{
			"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns",
		}}}) {
			t.Fatal("DNS must reach only cluster DNS")
		}
		ports := dns["toPorts"].([]any)
		if len(ports) != 1 || !reflect.DeepEqual(field(t, ports[0].(map[string]any), "rules", "dns"), wantNames) {
			t.Fatal("DNS proxy must observe only the two allowed destination names")
		}
	}
}

func TestCollectorsWaitForReviewedActivation(t *testing.T) {
	collectorNamespaces := map[string]bool{"bookkeeper": true, "bookkeeper-dev": true}
	for environment, path := range map[string]string{
		"prod":  "k8s/providers/hetzner/apps",
		"local": "k8s/providers/docker/apps",
	} {
		t.Run(environment, func(t *testing.T) {
			for _, item := range render(t, path) {
				metadata, _ := item["metadata"].(map[string]any)
				namespace, _ := metadata["namespace"].(string)
				name, _ := metadata["name"].(string)
				if collectorNamespaces[namespace] || (item["kind"] == "Namespace" && collectorNamespaces[name]) {
					t.Fatal("collectors must remain outside active apps until configuration and activation are reviewed")
				}
			}
		})
	}
}
