package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const aliasesSettings = `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator","maintainerAliases":["former-a","former-b"]}`

func aliasInputs(t *testing.T, settings string, inventory map[string]any) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "inputs")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"settings.json": settings, "listener-inventory.json": string(raw),
		"administrator-password": strings.Repeat("A", 40), "maintainer-password": strings.Repeat("B", 40), "agent-password": strings.Repeat("C", 40),
	} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return input, filepath.Join(dir, "plan.ndjson")
}

func nativeAlias(name string) map[string]any {
	return map[string]any{"name": name, "domainId": "domain", "enabled": true, "description": nil}
}

func receivingOwner(id, name string, aliases map[string]any) map[string]any {
	return map[string]any{"id": id, "name": name, "domainId": "domain", "@type": "User", "aliases": aliases}
}

func receivingInventory(accounts, lists []map[string]any) map[string]any {
	v := withApplicationInventory(inventoryFixture())
	for i, objects := range [][]map[string]any{accounts, lists} {
		object, tag := "Account", "accounts"
		if i == 1 {
			object, tag = "MailingList", "mailing-lists"
		}
		ids, entries := []string{}, []any{}
		for _, item := range objects {
			ids = append(ids, item["id"].(string))
			entries = append(entries, item)
		}
		v["methodResponses"] = append(v["methodResponses"].([]any),
			[]any{"x:" + object + "/query", map[string]any{"ids": ids, "position": 0, "total": len(ids)}, tag + "-query"},
			[]any{"x:" + object + "/get", map[string]any{"list": entries, "notFound": []string{}}, tag + "-get"})
	}
	return v
}

func TestConfiguredAliasesBecomeOnlyTheMaintainerReceivingSet(t *testing.T) {
	input, output := aliasInputs(t, aliasesSettings, receivingInventory(nil, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("approved aliases refused: %s", &log)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"0": map[string]any{"name": "former-a", "domainId": "#domain", "enabled": true, "description": nil},
		"1": map[string]any{"name": "former-b", "domainId": "#domain", "enabled": true, "description": nil},
	}
	found := false
	for _, op := range ops {
		if op.Object != "Account" || op.Type != "upsert" {
			continue
		}
		for ref, value := range op.Value {
			account := value.(map[string]any)
			aliases, exists := account["aliases"]
			if ref == "maintainer" {
				found = exists && reflect.DeepEqual(aliases, want)
			} else if exists {
				t.Fatal("receiving aliases granted to another account")
			}
		}
	}
	if !found {
		t.Fatal("maintainer receiving aliases missing or broadened")
	}
	if strings.Contains(log.String(), "former-") || strings.Contains(log.String(), strings.Repeat("B", 40)) {
		t.Fatal("private settings leaked")
	}
}

func TestAliasConfigurationRejectsInvalidOrAmbiguousIdentities(t *testing.T) {
	for _, value := range []string{`null`, `"former-a"`, `[1]`, `["former-a","former-a"]`, `["Former-a"]`, `["agents"]`, `["operator"]`, `["maintainer"]`, `["other@example.test"]`, `[""]`, `["trailing."]`, `["a..b"]`, `["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]`} {
		t.Run(value, func(t *testing.T) {
			settings := strings.Replace(aliasesSettings, `["former-a","former-b"]`, value, 1)
			input, output := aliasInputs(t, settings, receivingInventory(nil, nil))
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("invalid aliases accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused alias input produced a plan")
			}
		})
	}
}

func TestOmittedAliasesDoNotManageExistingReceivingAddresses(t *testing.T) {
	v := fixture()
	raw, err := render(v)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Object != "Account" || op.Type != "upsert" {
			continue
		}
		for _, value := range op.Value {
			if _, ok := value.(map[string]any)["aliases"]; ok {
				t.Fatal("omission would replace existing aliases")
			}
		}
	}
}

func TestAliasReadbackRejectsAnExtraReceivingIdentity(t *testing.T) {
	want := map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b")}
	extra := map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b"), "2": nativeAlias("unapproved")}
	if policyMatches(want, extra, "aliases") {
		t.Fatal("extra receiving address passed readback")
	}
	for _, field := range []string{"name", "domainId", "enabled", "description"} {
		changed := nativeAlias("former-b")
		changed[field] = map[string]any{"name": "unapproved", "domainId": "other-domain", "enabled": false, "description": "unexpected"}[field]
		if policyMatches(want, map[string]any{"0": nativeAlias("former-a"), "1": changed}, "aliases") {
			t.Fatalf("changed %s passed readback", field)
		}
	}
	if !policyMatches(want, map[string]any{"0": nativeAlias("former-b"), "1": nativeAlias("former-a")}, "aliases") {
		t.Fatal("equivalent native ordering refused")
	}
}

func TestAliasPlanRefusesExistingOwnersAndIncompleteCensus(t *testing.T) {
	for _, kind := range []string{"account-primary", "account-alias", "disabled-account-alias", "group-primary", "list-primary", "list-alias", "maintainer-extra", "maintainer-partial", "maintainer-wrong-type", "missing-account", "partial-account", "partial-list", "duplicate-account-id", "domain-alias"} {
		t.Run(kind, func(t *testing.T) {
			maintainer := receivingOwner("retained", "maintainer", map[string]any{})
			other := receivingOwner("competing", "other", map[string]any{})
			accounts, lists := []map[string]any{maintainer}, []map[string]any{}
			switch kind {
			case "account-primary":
				other["name"] = "former-a"
				accounts = append(accounts, other)
			case "group-primary":
				other["name"] = "former-a"
				other["@type"] = "Group"
				accounts = append(accounts, other)
			case "account-alias", "disabled-account-alias":
				alias := nativeAlias("former-a")
				if kind == "disabled-account-alias" {
					alias["enabled"] = false
				}
				other["aliases"] = map[string]any{"0": alias}
				accounts = append(accounts, other)
			case "list-primary":
				other["name"] = "former-a"
				lists = append(lists, other)
			case "list-alias":
				other["aliases"] = map[string]any{"0": nativeAlias("former-a")}
				lists = append(lists, other)
			case "maintainer-extra":
				maintainer["aliases"] = map[string]any{"0": nativeAlias("unapproved")}
			case "maintainer-partial":
				maintainer["aliases"] = map[string]any{"0": nativeAlias("former-a")}
			case "maintainer-wrong-type":
				maintainer["@type"] = "Group"
			}
			v := receivingInventory(accounts, lists)
			methods := v["methodResponses"].([]any)
			switch kind {
			case "missing-account":
				methods[10].([]any)[1].(map[string]any)["notFound"] = []string{"retained"}
			case "partial-account":
				methods[9].([]any)[1].(map[string]any)["total"] = 2
			case "partial-list":
				methods[11].([]any)[1].(map[string]any)["total"] = 1
			case "duplicate-account-id":
				methods[9].([]any)[1].(map[string]any)["ids"] = []string{"retained", "retained"}
				methods[9].([]any)[1].(map[string]any)["total"] = 2
			case "domain-alias":
				inventoryDomain(v)["name"] = "other.test"
				inventoryDomain(v)["aliases"] = map[string]bool{"example.test": true}
			}
			input, output := aliasInputs(t, aliasesSettings, v)
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("ambiguous ownership produced a plan")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused ownership produced a plan")
			}
		})
	}
}

func TestAliasPlanReplaysOnlyTheAlreadyApprovedReceivingSet(t *testing.T) {
	for _, aliases := range []map[string]any{{}, {"0": nativeAlias("former-b"), "1": nativeAlias("former-a")}} {
		input, output := aliasInputs(t, aliasesSettings, receivingInventory([]map[string]any{receivingOwner("retained", "maintainer", aliases)}, nil))
		var log bytes.Buffer
		if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
			t.Fatalf("safe replay refused: %s", &log)
		}
	}
}

func TestConfiguredAliasesRequireTheCompleteRecipientCensus(t *testing.T) {
	input, output := aliasInputs(t, aliasesSettings, withApplicationInventory(inventoryFixture()))
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
		t.Fatal("old listener-only census authorized new receiving identities")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("missing ownership census produced a plan")
	}
}

func TestEmptyAliasSettingCannotEraseExistingAliases(t *testing.T) {
	settings := strings.Replace(aliasesSettings, `["former-a","former-b"]`, `[]`, 1)
	input, output := aliasInputs(t, settings, receivingInventory([]map[string]any{receivingOwner("retained", "maintainer", map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b")})}, nil))
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
		t.Fatal("explicit empty list erased an existing receiving identity")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("refused clearing produced a plan")
	}
}
