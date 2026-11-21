package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestNativeSeedWorkspacePermitsOnlyItsExplicitBackup(t *testing.T) {
	namespace := nativeSeedNamespaceManifest(t)
	t.Run("reviewed manifest permits an explicit backup", func(t *testing.T) {
		if value(namespace, "metadata", "labels", "velero.io/exclude-from-backup") != nil {
			t.Fatal("Velero namespace exclusion conflicts with the explicit seed backup")
		}
	})
	for _, excluded := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit backup allowed", true: "old namespace exclusion refused"}[excluded], func(t *testing.T) {
			labels := value(namespace, "metadata", "labels").(object)
			delete(labels, "velero.io/exclude-from-backup")
			if excluded {
				labels["velero.io/exclude-from-backup"] = "true"
			}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if len(input) != 0 || program != "kubectl" || !slices.Equal(args, []string{"--context=admin@prod", "--request-timeout=20s", "get", "namespace", benchmarkNamespace, "--output=json"}) {
					t.Fatal("workspace qualification crossed its read-only boundary")
				}
				return json.Marshal(namespace)
			}}
			if _, err := observeBenchmarkWorkspace(t.Context(), c, "workspace-uid"); (err == nil) == excluded {
				t.Fatal("workspace qualification did not preserve explicit-backup eligibility:", err)
			}
		})
	}
}

func nativeSeedNamespaceManifest(t *testing.T) object {
	t.Helper()
	raw, err := os.ReadFile("../../k8s/providers/hetzner/infrastructure/controllers/longhorn/namespace-benchmark.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.YAMLToJSONStrict(raw)
	var namespace object
	if err != nil || json.Unmarshal(data, &namespace) != nil {
		t.Fatal("benchmark namespace is not strict YAML")
	}
	namespace["metadata"].(object)["uid"] = "workspace-uid"
	namespace["metadata"].(object)["resourceVersion"] = "17"
	namespace["status"] = object{"phase": "Active"}
	return namespace
}
