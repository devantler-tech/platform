package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const replaySettings = `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator"}`

func TestCredentialPatchReadbackNeverRequestsCredentialValues(t *testing.T) {
	for _, actualName := range []string{"maintainer", "foreign-account"} {
		t.Run(actualName, func(t *testing.T) {
			var requestedCredential atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Calls [][]json.RawMessage `json:"methodCalls"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Calls) != 1 || len(body.Calls[0]) != 3 {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				var args struct {
					Properties []string `json:"properties"`
				}
				if json.Unmarshal(body.Calls[0][1], &args) != nil {
					http.Error(w, "invalid fixture properties", http.StatusBadRequest)
					return
				}
				for _, key := range args.Properties {
					if key == "credentials" || strings.HasPrefix(key, "credentials/") {
						requestedCredential.Store(true)
						http.Error(w, "credential read forbidden", http.StatusBadRequest)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{"x:Account/get", map[string]any{"list": []any{map[string]any{"id": "owned", "name": actualName}}, "notFound": []string{}}, "verify"}}})
			}))
			defer server.Close()
			m := management{client: server.Client(), url: server.URL, password: strings.Repeat("A", 40)}
			plan := []byte(`{"@type":"update","object":"Account","id":"owned","value":{"name":"maintainer","credentials/0/secret":"synthetic-managed-value"}}`)
			err := m.verify(context.Background(), plan)
			if requestedCredential.Load() || (err == nil) != (actualName == "maintainer") {
				t.Fatal("readback exposed credentials or stopped verifying the account identity")
			}
		})
	}
}

func TestReplayUpdatesOnlyTheManagedCredentialValue(t *testing.T) {
	owners := []map[string]any{
		receivingOwner("existing-admin", "operator", map[string]any{}),
		receivingOwner("existing-human", "maintainer", map[string]any{}),
		receivingOwner("existing-agent", "agents", map[string]any{}),
	}
	input, output := aliasInputs(t, replaySettings, receivingInventory(owners, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatal("complete owned account census was refused")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"existing-admin": strings.Repeat("A", 40), "existing-human": strings.Repeat("B", 40), "existing-agent": strings.Repeat("C", 40)}
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		if op.Type != "update" {
			t.Fatal("existing accounts can still receive a replacement credential collection")
		}
		if _, full := op.Value["credentials"]; full {
			t.Fatal("replay replaces independently managed client credentials")
		}
		for key := range op.Value {
			if strings.HasPrefix(key, "credentials/") && key != "credentials/0/secret" {
				t.Fatal("replay changes independently owned credential metadata")
			}
		}
		if password, present := op.Value["credentials/0/secret"]; present {
			if want[op.ID] == "" || password != want[op.ID] || seen[op.ID] {
				t.Fatal("managed password is not bound to one owned account ID")
			}
			seen[op.ID] = true
		}
		if _, permissions := op.Value["permissions"]; permissions && op.ID == "#agents" {
			t.Fatal("existing service permissions still use a creation-only reference")
		}
	}
	if len(seen) != 3 {
		t.Fatal("managed account password updates are incomplete")
	}
}

func TestFirstAccountCreationCannotOverwriteAConcurrentAccount(t *testing.T) {
	input, output := aliasInputs(t, replaySettings, receivingInventory(nil, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatal("complete empty account census was refused")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	wantNames := map[string]string{"administrator": "operator", "maintainer": "maintainer", "agents": "agents"}
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		if op.Type == "upsert" {
			t.Fatal("new-account preparation can replace an account created after the census")
		}
		if op.Type == "create" {
			created += len(op.Value)
			for ref, rawAccount := range op.Value {
				account, ok := rawAccount.(map[string]any)
				if !ok || wantNames[ref] == "" || account["name"] != wantNames[ref] || account["domainId"] != "#domain" || account["@type"] != "User" {
					t.Fatal("initial account creation changed its approved identity")
				}
			}
		}
	}
	if created != 3 {
		t.Fatal("first initialization does not create exactly its three managed accounts")
	}
}

func TestPartialRecoveryCreatesOnlyItsMissingAccount(t *testing.T) {
	input, output := aliasInputs(t, replaySettings, receivingInventory([]map[string]any{
		receivingOwner("existing-admin", "operator", map[string]any{}),
		receivingOwner("existing-human", "maintainer", map[string]any{}),
	}, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatal("partially recovered account census was refused")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	created, updated := 0, 0
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		switch op.Type {
		case "create":
			account, ok := op.Value["agents"].(map[string]any)
			if len(op.Value) != 1 || !ok || account["name"] != "agents" || account["domainId"] != "#domain" {
				t.Fatal("partial recovery recreates an existing account")
			}
			created++
		case "update":
			if _, secret := op.Value["credentials/0/secret"]; secret {
				if op.ID != "existing-admin" && op.ID != "existing-human" {
					t.Fatal("partial recovery updates an unowned credential")
				}
				updated++
			}
		default:
			t.Fatal("partial recovery can overwrite a concurrently created account")
		}
	}
	if created != 1 || updated != 2 {
		t.Fatal("partial recovery lost an existing or missing identity")
	}
}

func TestReplayRefusesUnprovenAccountOwnership(t *testing.T) {
	for _, scenario := range []string{"missing-census", "duplicate", "wrong-type", "case-variant", "missing-domain", "wrong-domain"} {
		t.Run(scenario, func(t *testing.T) {
			owner := receivingOwner("existing-human", "maintainer", map[string]any{})
			owners := []map[string]any{owner}
			switch scenario {
			case "duplicate":
				owners = append(owners, receivingOwner("second-human", "maintainer", map[string]any{}))
			case "wrong-type":
				owner["@type"] = "Group"
			case "case-variant":
				owner["name"] = "Maintainer"
			case "missing-domain":
				delete(owner, "domainId")
			case "wrong-domain":
				owner["domainId"] = "foreign"
			}
			inventory := receivingInventory(owners, nil)
			if scenario == "wrong-domain" {
				methods := inventory["methodResponses"].([]any)
				methods[5].([]any)[1] = map[string]any{"ids": []string{"domain", "foreign"}, "position": 0, "total": 2}
				get := methods[6].([]any)[1].(map[string]any)
				get["list"] = append(get["list"].([]any), map[string]any{"id": "foreign", "name": "other.test", "aliases": map[string]bool{}, "directoryId": nil})
			}
			if scenario == "missing-census" {
				inventory = withApplicationInventory(inventoryFixture())
			}
			input, output := aliasInputs(t, replaySettings, inventory)
			var log bytes.Buffer
			if command([]string{"--input-dir", input, "--output-file", output}, &log) == 0 {
				t.Fatal("unproven account identity produced a credential-changing plan")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused account identity left an executable plan")
			}
		})
	}
}
