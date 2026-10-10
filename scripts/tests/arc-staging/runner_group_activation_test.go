package arcstaging_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const arcActivationFile = "managed-resource-activation-policy-arc.yaml"
const arcRunnerGroupResource = "runnergroups.actions.github.m.upbound.io"

func validateARCRunnerGroupActivation(policy map[string]any, resources []any) error {
	if policy["apiVersion"] != "apiextensions.crossplane.io/v1alpha1" || policy["kind"] != "ManagedResourceActivationPolicy" {
		return fmt.Errorf("expected a provider activation policy")
	}
	if !reflect.DeepEqual(policy["metadata"], map[string]any{"name": "github-arc-runners"}) {
		return fmt.Errorf("ARC must own a distinct activation policy")
	}
	if !reflect.DeepEqual(policy["spec"], map[string]any{"activate": []any{arcRunnerGroupResource}}) {
		return fmt.Errorf("activate exactly the namespaced ARC RunnerGroup resource")
	}
	count := 0
	for _, resource := range resources {
		if resource == arcActivationFile {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("include the ARC activation policy exactly once")
	}
	return nil
}

func TestActivatedRunnerGroupKeepsSyncExporterCoverage(t *testing.T) {
	const component = "k8s/bases/infrastructure/coroot/components/crossplane-sync-exporter/"
	data := field(t, readYAML(t, component+"config-map.yaml"), "data").(map[string]any)
	row := "actions.github.m.upbound.io\tv1alpha1\tRunnerGroup\trunnergroups"
	if strings.Count(data["managed-resources.tsv"].(string), row) != 1 {
		t.Fatal("activated RunnerGroup is absent from the closed exporter inventory")
	}
	var state map[string]any
	if err := yaml.Unmarshal([]byte(data["custom-resource-state.yaml"].(string)), &state); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range field(t, state, "spec", "resources").([]any) {
		gvk := field(t, entry, "groupVersionKind")
		if reflect.DeepEqual(gvk, map[string]any{"group": "actions.github.m.upbound.io", "version": "v1alpha1", "kind": "RunnerGroup"}) {
			count++
		}
	}
	if count != 1 {
		t.Fatal("RunnerGroup needs exactly one condition collector")
	}
	want := map[string]any{"apiGroups": []any{"actions.github.m.upbound.io"}, "resources": []any{"repositorypermissions", "runnergroups"}, "verbs": []any{"get", "list", "watch"}}
	count = 0
	for _, rule := range field(t, readYAML(t, component+"cluster-role.yaml"), "rules").([]any) {
		if reflect.DeepEqual(rule, want) {
			count++
		}
	}
	if count != 1 {
		t.Fatal("RunnerGroup collector needs its exact read-only permission")
	}
}

func TestARCResourceIsExplicitlyActivated(t *testing.T) {
	const component = "k8s/providers/hetzner/infrastructure/crossplane/"
	policy := readYAML(t, component+arcActivationFile)
	resources := field(t, readYAML(t, component+"kustomization.yaml"), "resources").([]any)
	if err := validateARCRunnerGroupActivation(policy, resources); err != nil {
		t.Fatal(err)
	}
}

func TestARCActivationRejectsBroaderResourcesAndMissingInclusion(t *testing.T) {
	for _, test := range []struct {
		name       string
		activation []any
		resources  []any
	}{
		{"wildcard", []any{"*"}, []any{arcActivationFile}},
		{"cluster scoped", []any{"runnergroups.actions.github.upbound.io"}, []any{arcActivationFile}},
		{"extra resource", []any{arcRunnerGroupResource, "repositories.repo.github.m.upbound.io"}, []any{arcActivationFile}},
		{"duplicate resource", []any{arcRunnerGroupResource, arcRunnerGroupResource}, []any{arcActivationFile}},
		{"missing inclusion", []any{arcRunnerGroupResource}, nil},
		{"duplicate inclusion", []any{arcRunnerGroupResource}, []any{arcActivationFile, arcActivationFile}},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := map[string]any{
				"apiVersion": "apiextensions.crossplane.io/v1alpha1", "kind": "ManagedResourceActivationPolicy",
				"metadata": map[string]any{"name": "github-arc-runners"},
				"spec":     map[string]any{"activate": test.activation},
			}
			if err := validateARCRunnerGroupActivation(policy, test.resources); err == nil {
				t.Fatal("accepted an unsafe or unreconciled activation policy")
			}
		})
	}
}
