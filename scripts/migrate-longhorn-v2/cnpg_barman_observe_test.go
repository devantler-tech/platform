package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// barmanEnvironmentFixture supplies complete synthetic API inventories, without production credentials.
func barmanEnvironmentFixture(t *testing.T, mutation string) (client, string) {
	t.Helper()
	crd, pin := cnpgSchemaFixture()
	crd["metadata"].(object)["name"] = "objectstores.barmancloud.cnpg.io"
	pod, dep, rs, node := barmanProcessFixture()
	gets := map[string]int{}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if len(input) != 0 || program != "kubectl" || len(args) < 4 || args[2] != "get" || strings.Contains(strings.Join(args, " "), "selector") {
			t.Fatal("Barman collector attempted a mutation or filtered inventory")
		}
		kind := args[3]
		gets[kind]++
		if mutation == "API failure" && kind == "nodes" {
			return nil, errors.New("private diagnostic")
		}
		var result object
		switch kind {
		case "customresourcedefinition":
			if mutation == "schema change" && gets[kind] > 1 {
				crd["spec"].(object)["profile"] = "changed"
			}
			result = crd
		case "deployment":
			if mutation == "generation change" && gets[kind] > 1 {
				dep["metadata"].(object)["generation"] = float64(2)
				dep["status"].(object)["observedGeneration"] = float64(2)
			}
			result = dep
		case "replicasets":
			result = object{"metadata": object{"resourceVersion": "10"}, "items": []any{rs}}
		case "nodes":
			if mutation == "reboot" && gets[kind] > 1 {
				value(node, "status", "nodeInfo").(object)["bootID"] = "new-boot"
			}
			result = object{"metadata": object{"resourceVersion": "10"}, "items": []any{node}}
		case "pods":
			if mutation == "process replaced" && gets[kind] > 1 {
				pod["metadata"].(object)["uid"] = "new-process"
			}
			items := []any{pod}
			if mutation == "duplicate process" {
				items = append(items, pod)
			}
			if mutation == "missing process" {
				items = nil
			}
			if mutation == "mutable sidecar" {
				value(pod, "spec", "containers").([]any)[0].(object)["env"] = []any{object{"name": "SIDECAR_IMAGE", "valueFrom": object{"configMapKeyRef": object{"name": "plugin-barman-cloud-config", "key": "SIDECAR_IMAGE"}}}}
			}
			result = object{"metadata": object{"resourceVersion": "10"}, "items": items}
			if mutation == "partial inventory" {
				result["metadata"].(object)["continue"] = "more"
			}
		default:
			return nil, errors.New("unexpected native resource")
		}
		return json.Marshal(result)
	}}
	return c, pin
}

// TestBarmanEnvironmentBindsTheSinglePinnedProcessAndSchema verifies actual collection assembly.
func TestBarmanEnvironmentBindsTheSinglePinnedProcessAndSchema(t *testing.T) {
	c, pin := barmanEnvironmentFixture(t, "")
	got, err := collectBarmanEnvironment(context.Background(), c, pin)
	if err != nil || got.CRD.SpecSHA != pin || got.Operator.PodUID != "barman-pod-uid" || got.DeploymentGeneration != 1 {
		t.Fatalf("complete Barman environment refused: %+v %v", got, err)
	}
}

// TestBarmanEnvironmentRefusesPartialOrChangingProof prevents stale configuration approval.
func TestBarmanEnvironmentRefusesPartialOrChangingProof(t *testing.T) {
	for _, mutation := range []string{"API failure", "schema change", "generation change", "reboot", "process replaced", "duplicate process", "missing process", "mutable sidecar", "partial inventory"} {
		t.Run(mutation, func(t *testing.T) {
			c, pin := barmanEnvironmentFixture(t, mutation)
			got, err := collectBarmanEnvironment(context.Background(), c, pin)
			if err == nil || !reflect.DeepEqual(got, cnpgBarmanEnvironment{}) || strings.Contains(err.Error(), "private diagnostic") {
				t.Fatalf("unbound environment or private diagnostic returned: %+v %v", got, err)
			}
		})
	}
}

// TestBarmanEnvironmentRequiresProtectedContextAndTheReleasedSchema prevents caller-selected trust.
func TestBarmanEnvironmentRequiresProtectedContextAndTheReleasedSchema(t *testing.T) {
	foreign := client{context: "foreign", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("foreign context reached a command")
		return nil, nil
	}}
	if got, err := observeBarmanEnvironment(context.Background(), foreign); err == nil || !reflect.DeepEqual(got, cnpgBarmanEnvironment{}) {
		t.Fatal("foreign context accepted")
	}
	c, _ := barmanEnvironmentFixture(t, "")
	if got, err := observeBarmanEnvironment(context.Background(), c); err == nil || !reflect.DeepEqual(got, cnpgBarmanEnvironment{}) {
		t.Fatal("synthetic schema substituted for the compiled release")
	}
}
