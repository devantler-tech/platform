package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestStorageRecoveryRequiresTheThreeAccountsAndTheStorageKey(t *testing.T) {
	want := []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent", "infrastructure/email/storage"}
	for _, mode := range []string{"collect", "verify", "fixture"} {
		base, paths, ok := credentialMode(mode + "-email-storage")
		if !ok || base != mode || !slices.Equal(paths, want) {
			t.Fatal("storage recovery did not select exactly the four declared records")
		}
	}
	for _, mode := range []string{"image-email-storage", "verify-email-storage-storage", "collect-storage", "verify-email-storage\n"} {
		if _, _, ok := credentialMode(mode); ok {
			t.Fatal("storage recovery admitted an undeclared mode")
		}
	}
}

func TestStorageRecoveryComparesTheActualPassphraseAndRefusesAccountSubstitution(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent", "infrastructure/email/storage"}
	records := map[string]json.RawMessage{}
	for _, path := range credentialPaths[:3] {
		records[path] = json.RawMessage(`{"password":"` + strings.Repeat("a", 64) + `"}`)
	}
	valid := json.RawMessage(`{"passphrase":"` + strings.Repeat("b", 64) + `"}`)
	records[credentialPaths[3]] = valid
	if !recordsEqual(records, records) {
		t.Fatal("complete storage recovery records were refused")
	}
	actual := map[string]json.RawMessage{}
	for path, value := range records {
		actual[path] = value
	}
	for index, invalid := range []string{
		`{"passphrase":"` + strings.Repeat("c", 64) + `"}`,
		`{"password":"` + strings.Repeat("b", 64) + `"}`,
		`{"passphrase":"short"}`,
		`{"passphrase":"` + strings.Repeat("b", 64) + `","extra":"ignored"}`,
		`{"passphrase":"` + strings.Repeat("b", 64) + `","passphrase":"` + strings.Repeat("b", 64) + `"}`,
		`{"Passphrase":"` + strings.Repeat("b", 64) + `"}`,
	} {
		actual[credentialPaths[3]] = json.RawMessage(invalid)
		if recordsEqual(records, actual) {
			t.Fatal("storage recovery accepted an altered or ambiguous key")
		}
		if index > 0 && recordsEqual(actual, actual) {
			t.Fatal("matching malformed storage expectations were accepted")
		}
	}
	actual[credentialPaths[3]] = valid
	actual[credentialPaths[0]] = valid
	if recordsEqual(records, actual) {
		t.Fatal("storage key was accepted in place of an account credential")
	}
}
