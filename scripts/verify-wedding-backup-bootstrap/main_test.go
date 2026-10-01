package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTemporaryESOAccessIsBoundToOneFixtureAndOwnedNamespace(t *testing.T) {
	p, err := esoPolicy("1234", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	if str(p, "metadata", "namespace") != "external-secrets" || str(p, "metadata", "name") != "wedding-bootstrap-1234" || str(p, "spec", "endpointSelector", "matchLabels", "app.kubernetes.io/name") != "external-secrets" {
		t.Fatal("policy grants a different controller access")
	}
	rules := at(p, "spec", "egress").([]any)
	if len(rules) != 1 || str(rules[0].(map[string]any), "toEndpoints") == "*" {
		t.Fatal("broad egress")
	}
	rule := rules[0].(map[string]any)
	endpoints := at(rule, "toEndpoints").([]any)
	if len(endpoints) != 1 || str(endpoints[0].(map[string]any), "matchLabels", "k8s:io.kubernetes.pod.namespace") != "wedding-bootstrap-1234" {
		t.Fatal("policy escapes fixture")
	}
	ports := at(rule, "toPorts").([]any)[0].(map[string]any)["ports"].([]any)
	if !reflect.DeepEqual(ports, []any{object{"port": "8200", "protocol": "TCP"}}) {
		t.Fatal("extra ports allowed")
	}
	owners := at(p, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 || str(owners[0].(map[string]any), "uid") != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal("policy outlives fixture")
	}
	if _, err = esoPolicy("../other", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err == nil {
		t.Fatal("bad run accepted")
	}
}

func TestCleanupRefusesForeignResourcesAndCarriesDeleteUIDPreconditions(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		deleted := 0
		k := client{ctx: context.Background(), run: "1234", command: func(_ context.Context, args []string, input []byte) ([]byte, error) {
			for _, a := range args {
				if a == "delete" {
					deleted++
					var body object
					_ = json.Unmarshal(input, &body)
					if str(body, "preconditions", "uid") != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
						t.Fatal("deletion has no UID guard")
					}
					return []byte(`{}`), nil
				}
			}
			if deleted > 0 {
				return nil, nil
			}
			o := object{"metadata": object{"name": "wedding-bootstrap-1234", "uid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "labels": object{ownerKey: "1234"}}}
			if foreign {
				at(o, "metadata", "labels").(map[string]any)[ownerKey] = "5678"
			}
			return json.Marshal(o)
		}}
		err := removeOwned(k, "", "namespaces", "wedding-bootstrap-1234", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
		if foreign && (err == nil || deleted != 0) {
			t.Fatal("foreign namespace deleted")
		}
		if !foreign && (err != nil || deleted != 1) {
			t.Fatal("owned namespace not deleted")
		}
	}
}

func recipeForTest(t *testing.T) recipe {
	t.Helper()
	r, err := loadRecipe("../..")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cloneForTest(o object) object {
	b, _ := json.Marshal(o)
	var result object
	_ = json.Unmarshal(b, &result)
	return result
}

// A shared-path regression must be refused before the fixture can claim success.
func TestProductionRecipeRefusesCredentialAndDestinationDrift(t *testing.T) {
	r := recipeForTest(t)
	if err := validateRecipe(r); err != nil {
		t.Fatalf("current source refused: %v", err)
	}
	for _, mutate := range []func(recipe){
		func(r recipe) { r.push["spec"].(map[string]any)["refreshInterval"] = "0" },
		func(r recipe) {
			at(r.push, "spec", "selector", "secret").(map[string]any)["name"] = "variables-cluster"
		},
		func(r recipe) {
			at(r.push, "spec", "data").([]any)[0].(map[string]any)["match"].(map[string]any)["remoteRef"].(map[string]any)["remoteKey"] = "infrastructure/backup/r2"
		},
		func(r recipe) {
			at(r.pull, "spec", "data").([]any)[1].(map[string]any)["remoteRef"].(map[string]any)["key"] = "infrastructure/backup/r2"
		},
		func(r recipe) {
			at(r.store, "spec", "configuration").(map[string]any)["destinationPath"] = "s3://platform-backups/cnpg/wedding-db"
		},
		func(r recipe) {
			at(r.store, "spec", "configuration", "s3Credentials", "accessKeyId").(map[string]any)["name"] = "wedding-db-backup-r2"
		},
	} {
		bad := recipe{cloneForTest(r.push), cloneForTest(r.pull), cloneForTest(r.store), cloneForTest(r.minio)}
		mutate(bad)
		if validateRecipe(bad) == nil {
			t.Fatal("drift accepted")
		}
	}
}

func TestFixturePreservesProductionMappingsAndNeverUsesProductionCredentials(t *testing.T) {
	r := recipeForTest(t)
	before := cloneForTest(r.push)
	items, err := fixture("1234", r, "fixtureAccess123456", "fixturePassword1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, r.push) {
		t.Fatal("builder mutated production recipe")
	}
	byKind := map[string]object{}
	for _, o := range items {
		byKind[str(o, "kind")] = o
	}
	push, pull := byKind["PushSecret"], byKind["ExternalSecret"]
	if !reflect.DeepEqual(at(push, "spec", "data"), at(r.push, "spec", "data")) || !reflect.DeepEqual(at(pull, "spec", "data"), at(r.pull, "spec", "data")) {
		t.Fatal("fixture changes secret mappings")
	}
	if str(push, "metadata", "namespace") != "wedding-bootstrap-1234" || str(push, "spec", "refreshInterval") != "5s" || str(pull, "spec", "secretStoreRef", "kind") != "SecretStore" {
		t.Fatal("wrong fixture adapter")
	}
	secret := byKind["Secret"]
	if str(secret, "metadata", "name") != "wedding-db-backup-r2-bootstrap" || str(secret, "stringData", "access_key_id") != "fixtureAccess123456" {
		t.Fatal("bootstrap source not created")
	}
	for _, o := range items {
		if str(o, "kind") != "Namespace" && str(o, "metadata", "namespace") != "wedding-bootstrap-1234" {
			t.Fatal("fixture escapes namespace")
		}
	}
}

func TestInvocationRefusesAnyImplicitOrUntrustedActivation(t *testing.T) {
	valid := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "1234", "WEDDING_BOOTSTRAP_CONFIRM": "verify-wedding-backup-bootstrap"}
	if run, err := invocation(func(k string) string { return valid[k] }); err != nil || run != "1234" {
		t.Fatal("protected dispatch refused")
	}
	for key, wrong := range map[string]string{"GITHUB_REPOSITORY": "other/platform", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/codex/test", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_RUN_ID": "../1234", "WEDDING_BOOTSTRAP_CONFIRM": ""} {
		bad := map[string]string{}
		for k, v := range valid {
			bad[k] = v
		}
		bad[key] = wrong
		if _, err := invocation(func(k string) string { return bad[k] }); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
}

func TestLoadRecipeRefusesMissingOrSymlinkedSource(t *testing.T) {
	root := t.TempDir()
	if _, err := loadRecipe(root); err == nil {
		t.Fatal("missing source accepted")
	}
	rel := "k8s/bases/infrastructure/vault-seed/push-secret-seed-wedding-db-backup-r2.yaml"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("../..", rel), filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRecipe(root); err == nil {
		t.Fatal("symlink accepted")
	}
}
