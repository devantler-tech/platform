package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture() inputs {
	return inputs{settings: settings{Domain: "example.test", Hostname: "mail.example.test", Maintainer: "maintainer", Agent: "agents", Administrator: "operator"}, administratorPassword: strings.Repeat("A", 40), maintainerPassword: strings.Repeat("B", 40), agentPassword: strings.Repeat("C", 40)}
}

func TestPrivateInputsCannotChangePolicyOrInjectExpressions(t *testing.T) {
	for _, change := range []func(*inputs){
		func(v *inputs) { v.Domain = "example.test' || true" },
		func(v *inputs) {
			v.Domain = "example.invalid"
			v.Hostname = "mail.example.invalid"
			v.Maintainer = "denied"
		},
		func(v *inputs) { v.Hostname = "mail.other.test" },
		func(v *inputs) { v.Maintainer = "maintainer+tag" },
		func(v *inputs) { v.Agent = v.Maintainer },
		func(v *inputs) { v.Administrator = v.Agent },
		func(v *inputs) { v.agentPassword = "short" },
		func(v *inputs) { v.maintainerPassword += "\n" },
		func(v *inputs) { v.administratorPassword = v.agentPassword },
	} {
		v := fixture()
		change(&v)
		if _, err := render(v); err == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
}

func TestPlanPreservesIdentitiesAndRestrictsLocalRecipients(t *testing.T) {
	b, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var operations []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var op map[string]any
		if err = json.Unmarshal([]byte(line), &op); err != nil {
			t.Fatal(err)
		}
		if op["@type"] == "destroy" {
			t.Fatal("configuration destroys existing mail")
		}
		operations = append(operations, op)
	}
	if len(operations) < 8 {
		t.Fatal("configuration families missing")
	}
	for _, want := range []string{
		`"@type":"Replace"`,
		`"jmapEmailGet":true`, `"mustMatchSender":{"else":"true","match":{}}`,
		`"tlsImplicit":true`, `"useTls":true`,
		`authenticated_as == 'agents@example.test'`, `rcpt == 'maintainer@example.test'`,
		`'denied@example.invalid'`, `starts_with(path, '/jmap/upload/')`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing policy %s", want)
		}
	}
	var servicePermissions map[string]any
	for _, op := range operations {
		if op["object"] == "Account" && op["id"] == "#agents" {
			servicePermissions = op["value"].(map[string]any)["permissions"].(map[string]any)
		}
	}
	if servicePermissions["@type"] != "Replace" {
		t.Fatal("service identity retained inherited permissions")
	}
	enabled := servicePermissions["enabledPermissions"].(map[string]any)
	for _, forbidden := range []string{"jmapEmailCreate", "jmapEmailUpdate", "jmapEmailDestroy", "jmapEmailImport", "jmapEmailSubmissionCreate", "sysAccountUpdate", "sysAppPasswordCreate"} {
		if _, present := enabled[forbidden]; present {
			t.Fatalf("unsafe service permission %s", forbidden)
		}
	}
	for _, forbidden := range []string{`"emailTtl"`, `"autoExpunge"`} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("unsafe permission or personal retention %s", forbidden)
		}
	}
}
