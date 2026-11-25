package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// planMaintainerAliases returns the alias set the plan gives the maintainer:
// from the create operation on a fresh store, or from the update of the
// retained account (fixture id "retained"). Any other account receiving
// aliases fails the test.
func planMaintainerAliases(t *testing.T, output string) (map[string]any, bool) {
	t.Helper()
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	managed := false
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		switch op.Type {
		case "update":
			aliases, exists := op.Value["aliases"]
			if op.ID != "retained" && exists {
				t.Fatal("receiving aliases granted to another account")
			}
			if op.ID == "retained" && exists {
				found, managed = aliases.(map[string]any), true
			}
		case "create":
			for ref, value := range op.Value {
				aliases, exists := value.(map[string]any)["aliases"]
				if ref != "maintainer" && exists {
					t.Fatal("receiving aliases granted to another account")
				}
				if ref == "maintainer" && exists {
					found, managed = aliases.(map[string]any), true
				}
			}
		}
	}
	return found, managed
}

func nativeAliasSet(names ...string) map[string]any {
	set := map[string]any{}
	for i, name := range names {
		set[string(rune('0'+i))] = map[string]any{"name": name, "domainId": "#domain", "enabled": true, "description": nil}
	}
	return set
}

// The settings without their own alias list.
var settingsWithoutAliases = strings.Replace(aliasesSettings, `,"maintainerAliases":["former-a","former-b"]`, "", 1)

func aliasListInputs(t *testing.T, settings, list string, mode os.FileMode, owners []map[string]any) (string, string) {
	t.Helper()
	input, output := aliasInputs(t, settings, receivingInventory(owners, nil))
	path := filepath.Join(input, "maintainer-aliases")
	if err := os.WriteFile(path, []byte(list), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return input, output
}

func TestOpenBaoAliasListFormsBecomeTheMaintainerAliases(t *testing.T) {
	for name, list := range map[string]string{
		"json array":        `["former-a@example.test","former-b@example.test"]`,
		"comma separated":   "former-a@example.test,former-b@example.test",
		"comma and spaces":  " former-a@example.test , Former-B@Example.Test \n",
		"one per line":      "former-a@example.test\nformer-b@example.test\n",
		"maintainer listed": "maintainer@example.test,former-a@example.test,former-b@example.test",
	} {
		t.Run(name, func(t *testing.T) {
			input, output := aliasListInputs(t, settingsWithoutAliases, list, 0600, nil)
			var log bytes.Buffer
			if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
				t.Fatalf("approved list refused: %s", &log)
			}
			if got, ok := planMaintainerAliases(t, output); !ok || !reflect.DeepEqual(got, nativeAliasSet("former-a", "former-b")) {
				t.Fatalf("aliases = %v", got)
			}
			if strings.Contains(log.String(), "former-") {
				t.Fatal("private alias list leaked")
			}
		})
	}
}

// The OpenBao list is the single source: when it is mounted, aliases still in
// the settings are replaced, not merged.
func TestOpenBaoAliasListReplacesSettingsAliases(t *testing.T) {
	input, output := aliasListInputs(t, aliasesSettings, "later@example.test", 0600, nil)
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) != 0 {
		t.Fatal("list refused next to settings aliases")
	}
	if got, ok := planMaintainerAliases(t, output); !ok || !reflect.DeepEqual(got, nativeAliasSet("later")) {
		t.Fatalf("aliases = %v", got)
	}
}

// The production adoption case: the retained maintainer already holds the
// settings' aliases, and the list keeps them and adds one.
func TestOpenBaoAliasListGrowsAPopulatedMaintainer(t *testing.T) {
	existing := map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b")}
	owners := []map[string]any{receivingOwner("retained", "maintainer", existing)}
	input, output := aliasListInputs(t, aliasesSettings, "former-b@example.test,former-a@example.test,later@example.test", 0600, owners)
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("adoption on a populated maintainer refused: %s", &log)
	}
	if got, ok := planMaintainerAliases(t, output); !ok || !reflect.DeepEqual(got, nativeAliasSet("former-b", "former-a", "later")) {
		t.Fatalf("aliases = %v", got)
	}
}

// Removing an address from the list would drop a live alias, so it is refused
// until that removal is made deliberately.
func TestOpenBaoAliasListCannotSilentlyDropALiveAlias(t *testing.T) {
	existing := map[string]any{"0": nativeAlias("former-a"), "1": nativeAlias("former-b")}
	owners := []map[string]any{receivingOwner("retained", "maintainer", existing)}
	for name, list := range map[string]string{"one removed": "former-a@example.test", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			input, output := aliasListInputs(t, aliasesSettings, list, 0600, owners)
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("a list dropping a live alias produced a plan")
			}
		})
	}
}

func TestOpenBaoAliasListRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		list string
		mode os.FileMode
	}{
		"foreign domain":       {"former-a@other.test", 0600},
		"subdomain":            {"former-a@sub.example.test", 0600},
		"not an address":       {"former-a", 0600},
		"empty local part":     {"@example.test", 0600},
		"repeated":             {"former-a@example.test,Former-A@example.test", 0600},
		"maintainer twice":     {"maintainer@example.test,maintainer@example.test", 0600},
		"agent name":           {"agents@example.test", 0600},
		"administrator name":   {"operator@example.test", 0600},
		"invalid local part":   {"a..b@example.test", 0600},
		"broken json":          {`["former-a@example.test"`, 0600},
		"json with non-string": {`["former-a@example.test", 7]`, 0600},
		"nul byte":             {"former-a@example.test\x00", 0600},
		"publicly readable":    {"former-a@example.test", 0644},
		"too many": {strings.Join(func() []string {
			var many []string
			for _, c := range "abcdefghijklmnopq" {
				many = append(many, string(c)+"@example.test")
			}
			return many
		}(), ","), 0600},
	} {
		t.Run(name, func(t *testing.T) {
			input, output := aliasListInputs(t, settingsWithoutAliases, tc.list, tc.mode, nil)
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("invalid list accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused list produced a plan")
			}
		})
	}
}

func TestWithoutTheListSettingsAliasesStillApply(t *testing.T) {
	input, output := aliasInputs(t, aliasesSettings, receivingInventory(nil, nil))
	if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) != 0 {
		t.Fatal("settings aliases refused without the list")
	}
	if got, ok := planMaintainerAliases(t, output); !ok || !reflect.DeepEqual(got, nativeAliasSet("former-a", "former-b")) {
		t.Fatalf("aliases = %v", got)
	}
}
