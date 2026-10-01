package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRootRevocationPreservesOnlyTheScopedFixtureToken(t *testing.T) {
	orphan := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body object
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/sys/init":
			_, _ = w.Write([]byte(`{"keys_base64":["fixture-unseal"],"root_token":"fixture-root"}`))
		case "/v1/sys/unseal":
			_, _ = w.Write([]byte(`{"sealed":false}`))
		case "/v1/auth/token/create":
			orphan = body["no_parent"] == true && body["no_default_policy"] == true
			_, _ = w.Write([]byte(`{"auth":{"client_token":"fixture-scoped"}}`))
		case "/v1/auth/token/revoke-self":
			if !orphan {
				t.Error("root revocation would revoke the ESO token")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	b := bao{url: server.URL, ctx: context.Background(), http: server.Client()}
	if token, err := b.initialize(); err != nil || token != "fixture-scoped" {
		t.Fatal("fixture initialization failed")
	}
}

func TestProjectionRequiresControllerOwnershipAndExactDedicatedCredentials(t *testing.T) {
	es := object{"metadata": object{"uid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}
	s := object{"metadata": meta(projectedSecret, "wedding-bootstrap-1234", "1234"), "data": object{}}
	s["metadata"].(map[string]any)["uid"] = "11111111-2222-3333-4444-555555555555"
	s["metadata"].(map[string]any)["ownerReferences"] = []any{object{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "name": projectedSecret, "uid": str(es, "metadata", "uid"), "controller": true}}
	for key, value := range map[string]string{"ACCESS_KEY_ID": "fixtureAccess123456", "SECRET_ACCESS_KEY": "fixturePassword1234567890", "REGION": "auto"} {
		s["data"].(map[string]any)[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	if !projection(s, es, "1234", "fixtureAccess123456", "fixturePassword1234567890") {
		t.Fatal("valid projection refused")
	}
	for _, mutate := range []func(object){
		func(o object) { at(o, "metadata", "ownerReferences").([]any)[0].(map[string]any)["uid"] = "foreign" },
		func(o object) {
			o["data"].(map[string]any)["SECRET_ACCESS_KEY"] = base64.StdEncoding.EncodeToString([]byte("wrong"))
		},
		func(o object) { o["data"].(map[string]any)["EXTRA"] = "secret" },
	} {
		bad := copyObject(s)
		mutate(bad)
		if projection(bad, es, "1234", "fixtureAccess123456", "fixturePassword1234567890") {
			t.Fatal("foreign or malformed projection accepted")
		}
	}
}

func TestProbeConsumesOnlyTheControllerProjectedSecret(t *testing.T) {
	p, err := probePod("1234", recipeForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	volumes := at(p, "spec", "volumes").([]any)
	if len(volumes) != 2 || str(volumes[0].(map[string]any), "secret", "secretName") != projectedSecret {
		t.Fatal("probe bypasses ESO")
	}
	encoded, _ := json.Marshal(p)
	for _, forbidden := range []string{"minio-root-credentials", "wedding-db-backup-r2-bootstrap", "variables-cluster", "envFrom"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("probe contains %s", forbidden)
		}
	}
}

func TestReseedCannotClaimUnchangedSourceAfterAnInPlaceRewrite(t *testing.T) {
	before := object{"metadata": object{"uid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "resourceVersion": "123"}, "data": object{"access_key_id": "fixture-access", "secret_access_key": "fixture-password"}}
	if !unchangedSource(before, copyObject(before)) {
		t.Fatal("unchanged source refused")
	}
	for _, mutate := range []func(object){
		func(o object) { o["metadata"].(map[string]any)["resourceVersion"] = "124" },
		func(o object) { o["data"].(map[string]any)["secret_access_key"] = "replacement" },
	} {
		after := copyObject(before)
		mutate(after)
		if unchangedSource(before, after) {
			t.Fatal("in-place source rewrite accepted")
		}
	}
}
