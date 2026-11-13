package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// The reviewed voucher-collection grants, written out independently of the
// implementation so a widened production list cannot also widen this test.
var reviewedBookkeepingPermissions = []string{
	"authenticate",
	"jmapEmailGet", "jmapEmailQuery", "jmapEmailUpdate",
	"jmapMailboxCreate", "jmapMailboxGet",
}

const bookkeepingSettings = `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator","bookkeeping":"vouchers"}`

func bookkeepingFixture() inputs {
	v := fixture()
	v.Bookkeeping = "vouchers"
	v.bookkeepingPassword = strings.Repeat("D", 40)
	return v
}

func renderedOperations(t *testing.T, v inputs) []map[string]any {
	t.Helper()
	b, err := render(v)
	if err != nil {
		t.Fatal("the reviewed configuration was refused")
	}
	var ops []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var op map[string]any
		if err := json.Unmarshal([]byte(line), &op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return ops
}

func reviewedBookkeepingAccount(password string) map[string]any {
	enabled := map[string]any{}
	for _, name := range reviewedBookkeepingPermissions {
		enabled[name] = true
	}
	return map[string]any{
		"@type": "User", "name": "vouchers", "domainId": "#domain",
		"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}},
		"roles":       map[string]any{"@type": "User"},
		"permissions": map[string]any{"@type": "Replace", "enabledPermissions": enabled, "disabledPermissions": map[string]any{}},
		"aliases":     map[string]any{},
	}
}

func TestBookkeepingPermissionsAreTheReviewedMinimum(t *testing.T) {
	var granted []string
	for name := range bookkeepingPermissions() {
		granted = append(granted, name)
	}
	sort.Strings(granted)
	want := append([]string(nil), reviewedBookkeepingPermissions...)
	sort.Strings(want)
	if !reflect.DeepEqual(granted, want) {
		t.Fatalf("voucher-collection permissions changed without review: %v", granted)
	}
	// Names the collector must never hold, so a reviewer sees the intent.
	for _, forbidden := range []string{
		"emailSend", "emailReceive", "authenticateWithAlias",
		"imapAuthenticate", "pop3Authenticate", "sieveAuthenticate",
		"jmapEmailSubmissionCreate", "jmapIdentityCreate", "jmapEmailCreate", "jmapEmailImport", "jmapEmailCopy",
		"jmapEmailDestroy", "jmapMailboxUpdate", "jmapMailboxDestroy", "jmapBlobUpload", "jmapBlobCopy",
		"jmapSieveScriptCreate", "jmapVacationResponseUpdate", "jmapPushSubscriptionCreate",
		"sysAccountPasswordUpdate", "sysAppPasswordCreate", "sysApiKeyCreate",
	} {
		if bookkeepingPermissions()[forbidden] {
			t.Fatalf("voucher collection holds %s", forbidden)
		}
	}
	// Every grant is an audited normal-user permission of each supported runtime,
	// so no profile depends on a name its server does not define.
	for _, version := range []string{"0.16.27", "1.0.2", "1.0.3"} {
		profile, err := schemaFor(version)
		if err != nil {
			t.Fatal(err)
		}
		audited := mailboxPermissionsFor(profile)
		for name := range bookkeepingPermissions() {
			if !audited[name] {
				t.Fatalf("runtime %s does not define the voucher-collection permission %s", version, name)
			}
		}
	}
	first := bookkeepingPermissions()
	first["emailSend"] = true
	if bookkeepingPermissions()["emailSend"] {
		t.Fatal("one plan's permission map leaked into the next")
	}
}

func TestBookkeepingAccountExistsOnlyWhenChosen(t *testing.T) {
	for _, op := range renderedOperations(t, fixture()) {
		if raw, err := json.Marshal(op); err != nil || bytes.Contains(raw, []byte("vouchers")) || bytes.Contains(raw, []byte(`"bookkeeping"`)) {
			t.Fatal("an unchosen voucher-collection account reached the plan")
		}
	}
	for _, change := range []func(*inputs){
		func(v *inputs) { v.bookkeepingPassword = "" },
		func(v *inputs) { v.Bookkeeping = "" },
	} {
		v := bookkeepingFixture()
		change(&v)
		if _, err := render(v); err == nil {
			t.Fatal("a voucher-collection account without its credential, or the reverse, was accepted")
		}
	}
}

// Adding the account must leave every other policy, account and listener in the
// plan byte-for-byte equal, and give the account only its reviewed fields.
func TestBookkeepingAddsOnlyItsOwnRestrictedAccount(t *testing.T) {
	for _, version := range []string{"", "1.0.3"} {
		t.Run("runtime "+version, func(t *testing.T) {
			base := fixture()
			base.RuntimeVersion = version
			with := bookkeepingFixture()
			with.RuntimeVersion = version
			before, after := renderedOperations(t, base), renderedOperations(t, with)
			if len(before) != len(after) {
				t.Fatal("adding voucher collection added or removed a policy operation")
			}
			found := false
			for i := range after {
				if after[i]["@type"] == "create" && after[i]["object"] == "Account" {
					accounts := after[i]["value"].(map[string]any)
					if !reflect.DeepEqual(accounts["bookkeeping"], reviewedBookkeepingAccount(strings.Repeat("D", 40))) {
						t.Fatalf("voucher-collection account differs from its reviewed shape: %v", accounts["bookkeeping"])
					}
					delete(accounts, "bookkeeping")
					found = true
				}
				if !reflect.DeepEqual(before[i], after[i]) {
					t.Fatalf("adding voucher collection changed another %v operation", after[i]["object"])
				}
			}
			if !found {
				t.Fatal("first initialization does not create the chosen account")
			}
		})
	}
}

func TestUnsafeBookkeepingInputsAreRefused(t *testing.T) {
	for name, change := range map[string]func(*inputs){
		"maintainer name":     func(v *inputs) { v.Bookkeeping = v.Maintainer },
		"agent name":          func(v *inputs) { v.Bookkeeping = v.Agent },
		"administrator name":  func(v *inputs) { v.Bookkeeping = v.Administrator },
		"maintainer alias":    func(v *inputs) { v.MaintainerAliases = []string{"vouchers"} },
		"upper case":          func(v *inputs) { v.Bookkeeping = "Vouchers" },
		"subaddress":          func(v *inputs) { v.Bookkeeping = "vouchers+tag" },
		"expression":          func(v *inputs) { v.Bookkeeping = "vouchers' || true" },
		"double dot":          func(v *inputs) { v.Bookkeeping = "a..b" },
		"trailing dot":        func(v *inputs) { v.Bookkeeping = "vouchers." },
		"short password":      func(v *inputs) { v.bookkeepingPassword = "short" },
		"newline password":    func(v *inputs) { v.bookkeepingPassword += "\n" },
		"agent password":      func(v *inputs) { v.bookkeepingPassword = v.agentPassword },
		"maintainer password": func(v *inputs) { v.bookkeepingPassword = v.maintainerPassword },
		"admin password":      func(v *inputs) { v.bookkeepingPassword = v.administratorPassword },
	} {
		t.Run(name, func(t *testing.T) {
			v := bookkeepingFixture()
			change(&v)
			if _, err := render(v); err == nil {
				t.Fatal("unsafe voucher-collection input accepted")
			}
		})
	}
}

func TestChosenBookkeepingCredentialIsRequired(t *testing.T) {
	for name, scenario := range map[string]struct {
		settings string
		prepare  func(t *testing.T, input string)
		valid    bool
	}{
		"chosen with credential": {bookkeepingSettings, func(*testing.T, string) {}, true},
		"unchosen without credential": {replaySettings, func(t *testing.T, input string) {
			if err := os.Remove(filepath.Join(input, "bookkeeping-password")); err != nil {
				t.Fatal(err)
			}
		}, true},
		"chosen without credential": {bookkeepingSettings, func(t *testing.T, input string) {
			if err := os.Remove(filepath.Join(input, "bookkeeping-password")); err != nil {
				t.Fatal(err)
			}
		}, false},
		"chosen with readable credential": {bookkeepingSettings, func(t *testing.T, input string) {
			if err := os.Chmod(filepath.Join(input, "bookkeeping-password"), 0644); err != nil {
				t.Fatal(err)
			}
		}, false},
		"chosen with invalid credential": {bookkeepingSettings, func(t *testing.T, input string) {
			if err := os.WriteFile(filepath.Join(input, "bookkeeping-password"), []byte("private-canary"), 0600); err != nil {
				t.Fatal(err)
			}
		}, false},
		"explicit empty name": {strings.Replace(bookkeepingSettings, `"vouchers"`, `""`, 1), func(*testing.T, string) {}, false},
		"null name":           {strings.Replace(bookkeepingSettings, `"vouchers"`, `null`, 1), func(*testing.T, string) {}, false},
		"duplicate name":      {strings.Replace(bookkeepingSettings, `}`, `,"bookkeeping":"invoices"}`, 1), func(*testing.T, string) {}, false},
	} {
		t.Run(name, func(t *testing.T) {
			input, output := aliasInputs(t, scenario.settings, receivingInventory(nil, nil))
			scenario.prepare(t, input)
			var log bytes.Buffer
			if (command([]string{"--input-dir", input, "--output-file", output}, &log) == 0) != scenario.valid {
				t.Fatal("voucher-collection credential handling changed")
			}
			if _, err := os.Stat(output); scenario.valid == os.IsNotExist(err) {
				t.Fatal("plan presence does not match the credential verdict")
			}
			if strings.Contains(log.String(), "private-canary") || strings.Contains(log.String(), strings.Repeat("D", 40)) || strings.Contains(log.String(), "vouchers") {
				t.Fatal("private voucher-collection input appeared in output")
			}
		})
	}
}

func bookkeepingPlan(t *testing.T, owners []map[string]any) []operation {
	t.Helper()
	input, output := aliasInputs(t, bookkeepingSettings, receivingInventory(owners, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatal("the owned voucher-collection census was refused")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestBookkeepingReplayUpdatesOnlyItsOwnedAccount(t *testing.T) {
	ops := bookkeepingPlan(t, []map[string]any{
		receivingOwner("existing-admin", "operator", map[string]any{}),
		receivingOwner("existing-human", "maintainer", map[string]any{}),
		receivingOwner("existing-agent", "agents", map[string]any{}),
		receivingOwner("existing-vouchers", "vouchers", map[string]any{"0": nativeAlias("widened")}),
	})
	want := reviewedBookkeepingAccount("")
	delete(want, "credentials")
	want["credentials/0/secret"] = strings.Repeat("D", 40)
	updated := 0
	for _, op := range ops {
		if op.Object != "Account" {
			continue
		}
		if op.Type != "update" {
			t.Fatal("a recovered voucher-collection account can be recreated")
		}
		if op.ID == "existing-vouchers" {
			if !reflect.DeepEqual(op.Value, want) {
				t.Fatalf("replay does not restore the reviewed account exactly: %v", op.Value)
			}
			updated++
		}
	}
	if updated != 1 {
		t.Fatal("replay is not bound to exactly one owned voucher-collection account")
	}
}

func TestPartialRecoveryCreatesOnlyTheChosenBookkeepingAccount(t *testing.T) {
	ops := bookkeepingPlan(t, []map[string]any{
		receivingOwner("existing-admin", "operator", map[string]any{}),
		receivingOwner("existing-human", "maintainer", map[string]any{}),
		receivingOwner("existing-agent", "agents", map[string]any{}),
	})
	created := 0
	for _, op := range ops {
		if op.Object == "Account" && op.Type == "create" {
			if len(op.Value) != 1 || !reflect.DeepEqual(op.Value["bookkeeping"], reviewedBookkeepingAccount(strings.Repeat("D", 40))) {
				t.Fatal("adding voucher collection recreates or changes another account")
			}
			created++
		}
	}
	if created != 1 {
		t.Fatal("the chosen account was not created exactly once")
	}
}

func TestBookkeepingOwnershipMustBeProven(t *testing.T) {
	for _, scenario := range []string{"duplicate", "wrong-type", "case-variant", "foreign-alias"} {
		t.Run(scenario, func(t *testing.T) {
			owners := []map[string]any{receivingOwner("existing-vouchers", "vouchers", map[string]any{})}
			switch scenario {
			case "duplicate":
				owners = append(owners, receivingOwner("second-vouchers", "vouchers", map[string]any{}))
			case "wrong-type":
				owners[0]["@type"] = "Group"
			case "case-variant":
				owners[0]["name"] = "Vouchers"
			case "foreign-alias":
				owners = []map[string]any{receivingOwner("foreign", "foreign", map[string]any{"0": nativeAlias("vouchers")})}
			}
			input, output := aliasInputs(t, bookkeepingSettings, receivingInventory(owners, nil))
			var log bytes.Buffer
			if command([]string{"--input-dir", input, "--output-file", output}, &log) == 0 {
				t.Fatal("unproven voucher-collection ownership produced a credential-changing plan")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("refused voucher-collection ownership left an executable plan")
			}
		})
	}
}

// Readback compares the whole permission and alias collections, so a grant or
// receiving alias added outside the plan fails initialization instead of staying.
func TestBookkeepingReadbackRejectsWiderAccess(t *testing.T) {
	var update operation
	for _, op := range bookkeepingPlan(t, []map[string]any{receivingOwner("existing-vouchers", "vouchers", map[string]any{})}) {
		if op.Object == "Account" && op.ID == "existing-vouchers" {
			update = op
		}
	}
	plan, err := json.Marshal(update)
	if err != nil || update.ID == "" {
		t.Fatal("the voucher-collection update is missing")
	}
	exact := func() map[string]any {
		account := reviewedBookkeepingAccount("")
		delete(account, "credentials")
		account["id"] = "existing-vouchers"
		account["domainId"] = "#domain"
		return account
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"exact", func(map[string]any) {}, true},
		{"receiving", func(a map[string]any) {
			a["permissions"].(map[string]any)["enabledPermissions"].(map[string]any)["emailReceive"] = true
		}, false},
		{"sending", func(a map[string]any) {
			a["permissions"].(map[string]any)["enabledPermissions"].(map[string]any)["emailSend"] = true
		}, false},
		{"deletion", func(a map[string]any) {
			a["permissions"].(map[string]any)["enabledPermissions"].(map[string]any)["jmapEmailDestroy"] = true
		}, false},
		{"disabled entry", func(a map[string]any) {
			a["permissions"].(map[string]any)["disabledPermissions"] = map[string]any{"jmapEmailUpdate": true}
		}, false},
		{"missing grant", func(a map[string]any) {
			delete(a["permissions"].(map[string]any)["enabledPermissions"].(map[string]any), "jmapEmailUpdate")
		}, false},
		{"inherited role", func(a map[string]any) { a["permissions"] = map[string]any{"@type": "Inherit"} }, false},
		{"administrator role", func(a map[string]any) { a["roles"] = map[string]any{"@type": "Admin"} }, false},
		{"receiving alias", func(a map[string]any) { a["aliases"] = map[string]any{"0": nativeAlias("widened")} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				if r.URL.Path != "/jmap" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Methods) != 1 || len(request.Methods[0]) != 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var method string
				var args struct {
					IDs        []string `json:"ids"`
					Properties []string `json:"properties"`
				}
				if json.Unmarshal(request.Methods[0][0], &method) != nil || method != "x:Account/get" || json.Unmarshal(request.Methods[0][1], &args) != nil || !reflect.DeepEqual(args.IDs, []string{"existing-vouchers"}) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for _, property := range args.Properties {
					if strings.HasPrefix(property, "credentials") {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				reads.Add(1)
				actual := exact()
				// The native server returns the resolved domain, not the reference.
				actual["domainId"] = ""
				tc.change(actual)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, map[string]any{"list": []any{actual}, "notFound": []string{}}, "verify"}}})
			}))
			defer server.Close()
			m := management{url: server.URL, password: t.Name(), client: server.Client()}
			if (m.verify(context.Background(), plan) == nil) != tc.valid {
				t.Fatal("voucher-collection readback accepted wider access or refused the exact account")
			}
			if reads.Load() != 1 {
				t.Fatal("the account was not read back; an earlier refusal cannot satisfy this control")
			}
		})
	}
}
