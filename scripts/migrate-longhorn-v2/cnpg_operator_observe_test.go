package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// operatorEnvironmentFixture supplies two coherent metadata collections around current startup logs.
func operatorEnvironmentFixture(t *testing.T, mutation string) (client, string, *int) {
	t.Helper()
	crd, pin := cnpgSchemaFixture()
	pod, dep, rs, node := operatorProcessFixture()
	second, err := cloneRecoverySpec(pod)
	if err != nil {
		t.Fatal(err)
	}
	second["metadata"].(object)["name"] = "cloudnative-pg-fixture-two"
	second["metadata"].(object)["uid"] = "pod-two"
	value(second, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("b", 64)
	cm := resource("cnpg-controller-manager-config")
	cm["apiVersion"], cm["kind"] = "v1", "ConfigMap"
	cm["metadata"].(object)["namespace"] = "cnpg-system"
	logs := 0
	gets := map[string]int{}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if len(input) != 0 || program != "kubectl" || len(args) < 4 {
			t.Fatal("operator collector attempted an unexpected command")
		}
		if strings.Contains(strings.Join(args, " "), "selector") {
			t.Fatal("operator collector used a filtered inventory")
		}
		if args[2] == "logs" {
			logs++
			records, _ := cnpgStartupFixture()
			if mutation == "different startup" && logs == 2 {
				records[2]["configuration"].(object)["includePlugins"] = "foreign"
			}
			if mutation == "service startup differs" && logs == 2 {
				records[2]["configuration"].(object)["createAnyService"] = true
			}
			return encodeStartupFixture(t, records), nil
		}
		if args[2] != "get" {
			t.Fatal("operator collector attempted a mutation")
		}
		kind := args[3]
		gets[kind]++
		var result object
		switch kind {
		case "customresourcedefinition":
			if mutation == "schema changed" && gets[kind] > 1 {
				crd["spec"].(object)["profile"] = "changed"
			}
			result = crd
		case "deployment":
			if mutation == "deployment changed" && gets[kind] > 1 {
				dep["metadata"].(object)["generation"] = float64(2)
				value(dep, "status").(object)["observedGeneration"] = float64(2)
			}
			result = dep
		case "configmap":
			if mutation == "configuration changed" && gets[kind] > 1 {
				cm["metadata"].(object)["resourceVersion"] = "11"
			}
			result = cm
		case "secret":
			if args[4] != "cnpg-pull-secret" || !strings.Contains(strings.Join(args, " "), "--ignore-not-found") {
				t.Fatal("operator collector selected another secret or omitted absence semantics")
			}
			if mutation == "pull read failed" {
				return nil, errors.New("private-pull-read-canary")
			}
			if mutation == "pull secret exists" || mutation == "pull appeared" && gets[kind] > 1 {
				return json.Marshal(resource("cnpg-pull-secret"))
			}
			return nil, nil
		case "replicasets":
			if mutation == "parent revision changed" && gets[kind] > 1 {
				rs["metadata"].(object)["generation"] = float64(2)
				value(rs, "status").(object)["observedGeneration"] = float64(2)
			}
			result = object{"metadata": object{"resourceVersion": "10"}, "items": []any{rs}}
		case "nodes":
			if mutation == "node reboot" && gets[kind] > 1 {
				value(node, "status", "nodeInfo").(object)["bootID"] = "new-boot"
			}
			result = object{"metadata": object{"resourceVersion": "10"}, "items": []any{node}}
		case "pods":
			items := []any{pod, second}
			if mutation == "missing pod" {
				items = items[:1]
			}
			if mutation == "process replaced" && gets[kind] > 1 {
				second["metadata"].(object)["uid"] = "replacement"
			}
			result = object{"metadata": object{"resourceVersion": "10"}, "items": items}
			if mutation == "partial inventory" {
				result["metadata"].(object)["continue"] = "unread-page"
			}
		default:
			return nil, errors.New("unexpected native query")
		}
		return json.Marshal(result)
	}}
	return c, pin, &logs
}

// TestCNPGEnvironmentCollectsBothCurrentOperatorProcessesBeforeReturningDefaults proves the read sequence.
func TestCNPGEnvironmentCollectsBothCurrentOperatorProcessesBeforeReturningDefaults(t *testing.T) {
	c, pin, logs := operatorEnvironmentFixture(t, "")
	environment, err := collectCNPGAdmissionEnvironment(context.Background(), c, pin)
	if err != nil || len(environment.Operators) != 2 || *logs != 2 || environment.Defaults != (recoveryAdmissionDefaults{Version: "1.30.2", ConfigMapQueries: true}) {
		t.Fatalf("complete environment refused: %v", err)
	}
}

// TestCNPGEnvironmentRejectsPartialInventoriesAndChangesAcrossStartupCollection prevents stale flags.
func TestCNPGEnvironmentRejectsPartialInventoriesAndChangesAcrossStartupCollection(t *testing.T) {
	for _, mutation := range []string{"different startup", "service startup differs", "schema changed", "configuration changed", "deployment changed", "parent revision changed", "node reboot", "missing pod", "process replaced", "partial inventory", "pull secret exists", "pull read failed", "pull appeared"} {
		t.Run(mutation, func(t *testing.T) {
			c, pin, _ := operatorEnvironmentFixture(t, mutation)
			bound, err := collectCNPGAdmissionEnvironment(context.Background(), c, pin)
			if err == nil || len(bound.Operators) != 0 || bound.Defaults.Version != "" {
				t.Fatal("partial or changed operator environment returned defaults")
			}
		})
	}
}

// TestCNPGEnvironmentRefusesAnotherContextBeforeAccess enforces the protected workflow's fixed context.
func TestCNPGEnvironmentRefusesAnotherContextBeforeAccess(t *testing.T) {
	c := client{context: "foreign", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("foreign context reached a command")
		return nil, nil
	}}
	if _, err := observeCNPGAdmissionEnvironment(context.Background(), c); err == nil {
		t.Fatal("foreign context accepted")
	}
}
