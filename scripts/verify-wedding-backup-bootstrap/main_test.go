package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

func TestFixtureAcceptsMaintainedServerAndRefusesArchivedForeignOrUnpinnedImages(t *testing.T) {
	r := recipeForTest(t)
	c := at(r.minio, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	c["image"] = "docker.io/chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"
	if _, err := fixture("1234", r, "fixtureAccess123456", "fixturePassword1234567890"); err != nil {
		t.Fatal("maintained pinned server cannot be used for the disposable fixture")
	}
	for _, image := range []string{
		"docker.io/chrislusf/seaweedfs:latest",
		"docker.io/chrislusf/seaweedfs:4.48",
		"docker.io/bitnamilegacy/minio:latest",
		"docker.io/bitnamilegacy/minio:2025.7.23-debian-12-r1@sha256:ba958aa5e12c8b1426dd95de61d8f1ed14741efa0ec730019ed57086930a4299",
		"docker.io/foreign/minio:2025.4.22-debian-12-r1@sha256:d7cd0e172c4cc0870f4bdc3142018e2a37be9acf04d68f386600daad427e0cab",
	} {
		c["image"] = image
		if validateRecipe(r) == nil {
			t.Fatal("unreviewed server image accepted")
		}
	}
}

func TestServerStartupRefusesMissingEmptyOrUnsafeCredentialFiles(t *testing.T) {
	for _, tc := range []struct {
		name, access, password string
		missing, valid         bool
	}{
		{"local", "minio", "minio-local-development-only", false, true},
		{"generated", "fixtureAccess123456", "fixturePassword1234567890", false, true},
		{"missing", "", "", true, false},
		{"empty-access", "", "fixturePassword1234567890", false, false},
		{"empty-secret", "fixtureAccess123456", "", false, false},
		{"short-access", "ab", "fixturePassword1234567890", false, false},
		{"short-secret", "fixtureAccess123456", "1234567", false, false},
		{"oversized-secret", "fixtureAccess123456", strings.Repeat("a", 129), false, false},
		{"quoted", "fixtureAccess123456", "bad\"secret-value", false, false},
		{"backslash", "fixtureAccess123456", `bad\secret-value`, false, false},
		{"newline", "fixtureAccess123456", "secret-value\n", false, false},
		{"oversized", strings.Repeat("a", 65), "fixturePassword1234567890", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recipeForTest(t)
			c := at(r.minio, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
			args := at(c, "args").([]any)
			if len(args) != 1 {
				t.Fatal("server has no fail-closed startup script")
			}
			dir := t.TempDir()
			config := filepath.Join(dir, "s3.json")
			invoked := filepath.Join(dir, "invoked")
			if !tc.missing {
				if err := os.WriteFile(filepath.Join(dir, "rootUser"), []byte(tc.access), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "rootPassword"), []byte(tc.password), 0600); err != nil {
					t.Fatal(err)
				}
			}
			weed := filepath.Join(dir, "weed")
			if err := os.WriteFile(weed, []byte("#!/bin/sh\n: > '"+invoked+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/etc/minio-credentials", dir, "/tmp/s3.json", config, "/usr/bin/weed", weed).Replace(args[0].(string))
			cmd := exec.Command("/bin/sh", "-ec", script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
			err := cmd.Run()
			_, called := os.Stat(invoked)
			if !tc.valid {
				if err == nil || !os.IsNotExist(called) {
					t.Fatal("unsafe credentials launched the S3 server")
				}
				return
			}
			if err != nil || called != nil {
				t.Fatal("valid file credentials could not launch server")
			}
			b, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			var got object
			if json.Unmarshal(b, &got) != nil {
				t.Fatal("invalid S3 configuration")
			}
			want := object{"identities": []any{object{"name": "fixture", "credentials": []any{object{"accessKey": tc.access, "secretKey": tc.password}}, "actions": []any{"Admin", "Read", "List", "Tagging", "Write"}}}}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("server configuration has another or anonymous identity")
			}
			info, err := os.Stat(config)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("generated credential file is not private")
			}
		})
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
