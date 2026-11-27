package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestActualBudgetUnstartedRestoreReusesOnlyItsOriginalPreparedScope(t *testing.T) {
	const run = "91022"
	const source = "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10"
	const namespaceUID = "cd426368-13da-451d-b61a-e50781ec7932"
	for _, scenario := range []string{"namespace only", "prepared policy", "replaced namespace", "changed owner", "existing Restore", "existing claim", "foreign policy", "earlier download", "changed modifier"} {
		t.Run(scenario, func(t *testing.T) {
			assets, err := actualBudgetRestoreAssets(run, "90718", "restore-worker")
			if err != nil {
				t.Fatal(err)
			}
			ns := text(assets[0], "metadata", "name")
			valueObject(assets[0], "metadata")["annotations"] = object{"storage.platform.devantler.tech/recovery-source": source}
			currentNamespace := cloneActualBudgetPreparation(t, assets[0])
			valueObject(currentNamespace, "metadata")["uid"] = namespaceUID
			currentNamespace["status"] = object{"phase": "Active"}
			switch scenario {
			case "replaced namespace":
				valueObject(currentNamespace, "metadata")["uid"] = "3d426368-13da-451d-b61a-e50781ec7932"
			case "changed owner":
				valueObject(currentNamespace, "metadata", "annotations")["storage.platform.devantler.tech/recovery-source"] = strings.Repeat("9", 40)
			}
			created := []string{}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 {
					return nil, errors.New("unexpected command")
				}
				if args[2] == "create" {
					var desired object
					if json.Unmarshal(input, &desired) != nil {
						return nil, errors.New("unexpected creation payload")
					}
					kind := text(desired, "kind")
					if kind != "NetworkPolicy" && kind != "ConfigMap" {
						t.Fatalf("preparation attempted to create %s", kind)
					}
					created = append(created, kind)
					return json.Marshal(cloneActualBudgetPreparation(t, desired))
				}
				if args[2] != "get" || input != nil {
					return nil, errors.New("unexpected mutation")
				}
				switch args[3] {
				case "namespace":
					if len(args) > 4 && args[4] == ns {
						return json.Marshal(currentNamespace)
					}
					return json.Marshal(object{"items": []any{currentNamespace}})
				case "persistentvolumes":
					return json.Marshal(object{"items": []any{object{"metadata": object{"name": "serving", "uid": "existing-pv"}}}})
				case "restores.velero.io":
					if scenario == "existing Restore" {
						return json.Marshal(cloneActualBudgetPreparation(t, assets[3]))
					}
					return nil, nil
				case "datadownloads.velero.io":
					if scenario == "earlier download" {
						return json.Marshal(object{"items": []any{object{
							"metadata": object{"name": "old-download", "uid": "old-download-uid"},
							"spec":     object{"targetVolume": object{"namespace": ns}},
						}}})
					}
				case "persistentvolumeclaims":
					if scenario == "existing claim" {
						return json.Marshal(object{"items": []any{object{"metadata": object{"name": actualBudgetClaim, "uid": "existing-claim"}}}})
					}
				case "ciliumnetworkpolicies.cilium.io":
					if scenario == "foreign policy" {
						return json.Marshal(object{"items": []any{object{"metadata": object{"name": "foreign-policy", "uid": "foreign-policy-uid"}}}})
					}
				case "networkpolicy.networking.k8s.io":
					if scenario == "prepared policy" {
						return json.Marshal(cloneActualBudgetPreparation(t, assets[1]))
					}
					return nil, nil
				case "configmap":
					if scenario == "changed modifier" {
						modifier := cloneActualBudgetPreparation(t, assets[2])
						modifier["data"] = object{"modifiers.json": "foreign"}
						return json.Marshal(modifier)
					}
					return nil, nil
				}
				return json.Marshal(object{"items": []any{}})
			}}
			_, err = prepareActualBudgetRestoreScope(t.Context(), c, source, &actualBudgetPriorRestore{run, source, namespaceUID}, assets)
			switch scenario {
			case "namespace only":
				if err != nil || strings.Join(created, ",") != "NetworkPolicy,ConfigMap" {
					t.Fatalf("the namespace-only interruption did not resume: %v", err)
				}
			case "prepared policy":
				if err != nil || strings.Join(created, ",") != "ConfigMap" {
					t.Fatalf("the previously verified isolation policy was not reused: %v", err)
				}
			default:
				if err == nil || len(created) != 0 {
					t.Fatalf("unsafe preparation admitted: %v, created %v", err, created)
				}
			}
		})
	}
}

func cloneActualBudgetPreparation(t *testing.T, source object) object {
	t.Helper()
	data, err := json.Marshal(source)
	var result object
	if err != nil || json.Unmarshal(data, &result) != nil {
		t.Fatal("invalid preparation fixture")
	}
	valueObject(result, "metadata")["uid"] = "631efa31-4537-410b-ab50-b7af5de78b1b"
	valueObject(result, "metadata")["resourceVersion"] = "1"
	return result
}
