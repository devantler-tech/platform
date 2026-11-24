package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The settings without their own alias list; the inventory file supplies it.
var addressSettings = strings.Replace(aliasesSettings, `,"maintainerAliases":["former-a","former-b"]`, "", 1)

func addressInputs(t *testing.T, settings, addresses string, mode os.FileMode) (string, string) {
	t.Helper()
	input, output := aliasInputs(t, settings, receivingInventory(nil, nil))
	if err := os.WriteFile(filepath.Join(input, "maintainer-addresses"), []byte(addresses), mode); err != nil {
		t.Fatal(err)
	}
	return input, output
}

func maintainerAliasesInPlan(t *testing.T, output string) (map[string]any, bool) {
	t.Helper()
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		// A retained maintainer (fixture id "retained") is updated in place.
		if op.Type == "update" {
			aliases, exists := op.Value["aliases"]
			if op.ID != "retained" && exists {
				t.Fatal("receiving aliases granted to another account")
			}
			if op.ID == "retained" {
				if !exists {
					return nil, false
				}
				return aliases.(map[string]any), true
			}
			continue
		}
		if op.Type != "create" {
			continue
		}
		for ref, value := range op.Value {
			account := value.(map[string]any)
			aliases, exists := account["aliases"]
			if ref != "maintainer" && exists {
				t.Fatal("receiving aliases granted to another account")
			}
			if ref == "maintainer" {
				if !exists {
					return nil, false
				}
				return aliases.(map[string]any), true
			}
		}
	}
	t.Fatal("maintainer account missing from the plan")
	return nil, false
}

func TestInventoryAddressesBecomeTheMaintainerAliases(t *testing.T) {
	input, output := addressInputs(t, addressSettings, "Maintainer@example.test, former-a@example.test,former-b@Example.Test\n", 0600)
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("approved inventory refused: %s", &log)
	}
	aliases, ok := maintainerAliasesInPlan(t, output)
	want := map[string]any{
		"0": map[string]any{"name": "former-a", "domainId": "#domain", "enabled": true, "description": nil},
		"1": map[string]any{"name": "former-b", "domainId": "#domain", "enabled": true, "description": nil},
	}
	if !ok || !reflect.DeepEqual(aliases, want) {
		t.Fatalf("inventory aliases = %v", aliases)
	}
	if strings.Contains(log.String(), "former-") {
		t.Fatal("private inventory leaked")
	}
}

func TestInventoryWithOnlyTheMaintainerManagesAnEmptyAliasSet(t *testing.T) {
	input, output := addressInputs(t, addressSettings, "maintainer@example.test", 0600)
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) != 0 {
		t.Fatal("maintainer-only inventory refused")
	}
	aliases, ok := maintainerAliasesInPlan(t, output)
	if !ok || len(aliases) != 0 {
		t.Fatalf("a maintainer-only inventory must manage an empty alias set, got %v (managed=%v)", aliases, ok)
	}
}

func TestInventoryRefusesIncompleteForeignOrAmbiguousLists(t *testing.T) {
	for name, tc := range map[string]struct {
		settings, addresses string
		mode                os.FileMode
	}{
		"missing maintainer":    {addressSettings, "former-a@example.test", 0600},
		"maintainer twice":      {addressSettings, "maintainer@example.test,Maintainer@example.test", 0600},
		"foreign domain":        {addressSettings, "maintainer@example.test,former-a@other.test", 0600},
		"subdomain":             {addressSettings, "maintainer@example.test,former-a@sub.example.test", 0600},
		"not an address":        {addressSettings, "maintainer@example.test,former-a", 0600},
		"empty local part":      {addressSettings, "maintainer@example.test,@example.test", 0600},
		"empty entry":           {addressSettings, "maintainer@example.test,,former-a@example.test", 0600},
		"duplicate alias":       {addressSettings, "maintainer@example.test,former-a@example.test,former-a@example.test", 0600},
		"agent address":         {addressSettings, "maintainer@example.test,agents@example.test", 0600},
		"administrator address": {addressSettings, "maintainer@example.test,operator@example.test", 0600},
		"invalid local part":    {addressSettings, "maintainer@example.test,a..b@example.test", 0600},
		"multiline":             {addressSettings, "maintainer@example.test\nformer-a@example.test", 0600},
		"empty file":            {addressSettings, "", 0600},
		"publicly readable":     {addressSettings, "maintainer@example.test", 0644},
		"settings plus inventory too many": {aliasesSettings, "maintainer@example.test," + strings.Join(func() []string {
			var many []string
			for _, c := range "abcdefghijklmno" {
				many = append(many, string(c)+"@example.test")
			}
			return many
		}(), ","), 0600},
		"too many": {addressSettings, "maintainer@example.test," + strings.Join(func() []string {
			var many []string
			for _, c := range "abcdefghijklmnopq" {
				many = append(many, string(c)+"@example.test")
			}
			return many
		}(), ","), 0600},
	} {
		t.Run(name, func(t *testing.T) {
			input, output := addressInputs(t, tc.settings, tc.addresses, tc.mode)
			if err := os.Chmod(filepath.Join(input, "maintainer-addresses"), tc.mode); err != nil {
				t.Fatal(err)
			}
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("invalid inventory accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused inventory produced a plan")
			}
		})
	}
}

// Production's settings already approve aliases. Adopting the inventory keeps
// them, adds every other inventory address, and counts an address in both once.
func TestInventoryKeepsSettingsAliasesAndAddsTheRest(t *testing.T) {
	input, output := addressInputs(t, aliasesSettings, "former-b@example.test,maintainer@example.test,later@example.test", 0600)
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("settings and inventory together refused: %s", &log)
	}
	aliases, ok := maintainerAliasesInPlan(t, output)
	want := map[string]any{
		"0": map[string]any{"name": "former-a", "domainId": "#domain", "enabled": true, "description": nil},
		"1": map[string]any{"name": "former-b", "domainId": "#domain", "enabled": true, "description": nil},
		"2": map[string]any{"name": "later", "domainId": "#domain", "enabled": true, "description": nil},
	}
	if !ok || !reflect.DeepEqual(aliases, want) {
		t.Fatalf("combined aliases = %v", aliases)
	}
	if strings.Contains(log.String(), "former-") || strings.Contains(log.String(), "later") {
		t.Fatal("private inventory leaked")
	}
}

// The production adoption case: the maintainer's account already holds the
// settings' approved aliases, and the inventory adds a new address.
func TestInventoryExtendsAnAlreadyPopulatedMaintainer(t *testing.T) {
	existing := map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b")}
	input, output := aliasInputs(t, aliasesSettings, receivingInventory([]map[string]any{receivingOwner("retained", "maintainer", existing)}, nil))
	if err := os.WriteFile(filepath.Join(input, "maintainer-addresses"), []byte("former-b@example.test,maintainer@example.test,later@example.test"), 0600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("adopting the inventory on a populated maintainer refused: %s", &log)
	}
	aliases, ok := maintainerAliasesInPlan(t, output)
	if !ok || len(aliases) != 3 {
		t.Fatalf("adopted aliases = %v", aliases)
	}
}

func TestUnionAliasesKeepsOrderAndDropsRepeats(t *testing.T) {
	got := unionAliases([]string{"b", "a"}, []string{"a", "c", "b", "d"})
	if !reflect.DeepEqual(got, []string{"b", "a", "c", "d"}) {
		t.Fatalf("union = %v", got)
	}
	if got := unionAliases(nil, []string{}); got == nil || len(got) != 0 {
		t.Fatalf("an empty union must stay a managed empty set, got %#v", got)
	}
}

func TestWithoutInventorySettingsAliasesStillApply(t *testing.T) {
	input, output := aliasInputs(t, aliasesSettings, receivingInventory(nil, nil))
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) != 0 {
		t.Fatal("settings aliases refused without an inventory")
	}
	if aliases, ok := maintainerAliasesInPlan(t, output); !ok || len(aliases) != 2 {
		t.Fatalf("settings aliases = %v", aliases)
	}
}
