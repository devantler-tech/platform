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
	"testing"
)

// namedInputs loads the bootstrap fixture inputs and applies deployment name
// flags as the commands do.
func namedInputs(t *testing.T, args ...string) inputs {
	t.Helper()
	v, err := loadInputs(bootstrapInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	var names deploymentNames
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		option := map[string]*hostnameOption{"hostname": &names.hostname, "web-hostname": &names.webHostname}[name]
		if option == nil || option.Set(value) != nil {
			t.Fatalf("fixture flag %s refused", arg)
		}
	}
	names.apply(&v)
	return v
}

// renderWithNames returns the named plan's operations by object.
func renderWithNames(t *testing.T, args ...string) map[string]operation {
	t.Helper()
	return planObjectsFor(t, namedInputs(t, args...))
}

// listenerPlan returns the named plan's declared listeners by name and its
// removals.
func listenerPlan(t *testing.T, args ...string) (map[string]map[string]any, []operation) {
	t.Helper()
	plan, err := render(namedInputs(t, args...))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]map[string]any{}
	var removals []operation
	for _, op := range ops {
		switch {
		case op.Type == "destroy":
			removals = append(removals, op)
		case op.Object == "NetworkListener":
			for _, raw := range op.Value {
				listener := raw.(map[string]any)
				declared[listener["name"].(string)] = listener
			}
		}
	}
	return declared, removals
}

// The setup image pinned before #6662 accepts exactly these listeners, in these
// profiles (inventory.go at d46048c56), and refuses any other. A plan written
// without --hostname must stay inside them, so a failed deployment can be
// healed back to that image.
var previousSetupListeners = map[string][3]any{
	"client-proxy": {"http", "127.0.0.1:18081", false},
	"management":   {"http", "0.0.0.0:8080", false},
	"smtp-inbound": {"smtp", "0.0.0.0:1025", false},
	"submission":   {"smtp", "0.0.0.0:1587", false},
	"imaps":        {"imap", "0.0.0.0:1993", true},
}

// Only --hostname serves implicit-TLS submission. Without it the plan declares
// only listeners the previous setup image accepts and removes a stored 465
// listener by its unique name; with it, the listener is declared and nothing is
// removed.
func TestImplicitSubmissionIsServedOnlyWithTheHostnameFlag(t *testing.T) {
	removal := operation{Type: "destroy", Object: "NetworkListener", Value: map[string]any{"name": "submissions"}}
	for _, args := range [][]string{nil, {"--web-hostname=web.example.test"}} {
		declared, removals := listenerPlan(t, args...)
		if len(declared) != len(previousSetupListeners) {
			t.Fatalf("%v: declared %d listeners, the previous setup accepts %d", args, len(declared), len(previousSetupListeners))
		}
		for name, listener := range declared {
			profile, known := previousSetupListeners[name]
			if !known || listener["protocol"] != profile[0] || !reflect.DeepEqual(listener["bind"], map[string]any{profile[1].(string): true}) || listener["tlsImplicit"] != profile[2] {
				t.Fatalf("%v: listener %s is not one the previous setup accepts: %v", args, name, listener)
			}
		}
		if len(removals) != 1 || !reflect.DeepEqual(removals[0], removal) {
			t.Fatalf("%v: the stored 465 listener must be removed by name, and nothing else: %v", args, removals)
		}
	}
	declared, removals := listenerPlan(t, "--hostname=mail.example.test")
	if len(removals) != 0 || declared["submissions"] == nil || len(declared) != len(previousSetupListeners)+1 {
		t.Fatalf("--hostname must declare the 465 listener and remove nothing: %v %v", declared, removals)
	}
}

// The readback of a removal requires that nothing of that name remains, and
// accepts only a plain string filter.
func TestRemovalReadbackRequiresTheListenerGone(t *testing.T) {
	listener := func(id, name string) map[string]any { return map[string]any{"id": id, "name": name} }
	removal := operation{Type: "destroy", Object: "NetworkListener", Value: map[string]any{"name": "submissions"}}
	for _, tc := range []struct {
		name   string
		stored []any
		op     operation
		valid  bool
	}{
		{"removed", []any{listener("a", "submission"), listener("b", "imaps")}, removal, true},
		{"nothing stored", []any{}, removal, true},
		{"still stored", []any{listener("a", "submission"), listener("c", "submissions")}, removal, false},
		{"empty filter", []any{listener("a", "submission")}, operation{Type: "destroy", Object: "NetworkListener", Value: map[string]any{}}, false},
		{"non-string filter", []any{listener("a", "submission")}, operation{Type: "destroy", Object: "NetworkListener", Value: map[string]any{"name": map[string]any{"submissions": true}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				var method, tag string
				if json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 || json.Unmarshal(call.Methods[0][0], &method) != nil || json.Unmarshal(call.Methods[0][2], &tag) != nil || !strings.HasPrefix(method, "x:NetworkListener/") {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any = map[string]any{"list": tc.stored, "notFound": []any{}}
				if strings.HasSuffix(method, "/query") {
					ids := []any{}
					for _, item := range tc.stored {
						ids = append(ids, item.(map[string]any)["id"])
					}
					result = map[string]any{"ids": ids, "total": len(ids), "position": 0}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
			if got := m.verifyOperation(context.Background(), tc.op, map[string]string{}) == nil; got != tc.valid {
				t.Fatalf("removal readback accepted=%v want %v", got, tc.valid)
			}
		})
	}
}

// The deployment's --hostname wins over the settings' hostname, and the
// settings' value is used unchanged without it.
func TestHostnameFlagTakesPrecedenceOverTheSettings(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "mail.example.test"},
		{[]string{"--hostname=smtp.example.test"}, "smtp.example.test"},
		{[]string{"--web-hostname=web.example.test"}, "mail.example.test"},
	} {
		got := renderWithNames(t, tc.args...)["SystemSettings"].Value["defaultHostname"]
		if got != tc.want {
			t.Fatalf("%v: default hostname %v, want %s", tc.args, got, tc.want)
		}
	}
}

// A name outside the mail domain, or the bare domain, is refused whichever
// source gives it.
func TestServerNamesMustBeHostsUnderTheMailDomain(t *testing.T) {
	for _, name := range []string{"example.test", "mail.other.test", "mail.example.test.other", "xexample.test"} {
		for _, field := range []string{"hostname", "webHostname"} {
			v := fixture()
			if field == "hostname" {
				v.Hostname = name
			} else {
				v.webHostname = name
			}
			if _, err := render(v); err == nil {
				t.Errorf("%s %q outside the mail domain was accepted", field, name)
			}
		}
	}
	v := fixture()
	v.Hostname, v.webHostname = "mail.example.test", "stalwart.platform.example.test"
	if _, err := render(v); err != nil {
		t.Fatal("hosts under the mail domain were refused")
	}
}

func TestHostnameOptionAcceptsOnlyALowercaseDNSName(t *testing.T) {
	for value, want := range map[string]bool{
		"mail.example.test":                  true,
		"stalwart.platform.example.test":     true,
		"":                                   false,
		"mail":                               false,
		"MAIL.example.test":                  false,
		"mail.example.test.":                 false,
		"-mail.example.test":                 false,
		"mail..example.test":                 false,
		"mail.example.test' || true || 'x":   false,
		"mail.example.test/path":             false,
		"mail.example.test:465":              false,
		" mail.example.test":                 false,
		"https://mail.example.test":          false,
		strings.Repeat("a", 63) + ".example": true,
		strings.Repeat("a", 64) + ".example": false,
	} {
		var o hostnameOption
		if got := o.Set(value) == nil; got != want {
			t.Errorf("Set(%q) accepted=%v want=%v", value, got, want)
		}
	}
}

// Every command that renders or checks the plan takes the name flags and
// refuses an invalid one without repeating it.
func TestServerNameFlagsAreValidatedWithoutDisclosure(t *testing.T) {
	for _, arg := range []string{"--hostname=Name-Canary.example.test", "--web-hostname=name-canary.example.test' || true", "--hostname=name-canary"} {
		for name, run := range map[string]func([]string, *bytes.Buffer) int{
			"bootstrap": func(args []string, log *bytes.Buffer) int { return bootstrapCommand(args, log) },
			"check":     func(args []string, log *bytes.Buffer) int { return checkCommand(args, log) },
			"plan":      func(args []string, log *bytes.Buffer) int { return command(args, log) },
		} {
			var log bytes.Buffer
			if run([]string{arg}, &log) == 0 || strings.Contains(strings.ToLower(log.String()), "canary") {
				t.Fatalf("%s accepted or disclosed %s: %q", name, arg, log.String())
			}
		}
	}
}

// The documents that set up mail apps advertise exactly what the server
// serves: no POP3 or ManageSieve, IMAP and SMTP at the mail hostname with
// implicit TLS only (993 and 465), and the HTTP services at the web host.
func TestAdvertisedServicesAreOnlyTheServedOnes(t *testing.T) {
	mail := map[string]any{"hostname": nil, "cleartext": false}
	web := map[string]any{"hostname": "web.example.test", "cleartext": false}
	want := map[string]any{"imap": mail, "smtp": mail, "jmap": web, "caldav": web, "carddav": web, "webdav": web}
	got := renderWithNames(t, "--hostname=smtp.example.test", "--web-hostname=web.example.test")["SystemSettings"].Value["services"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("advertised services differ: %v", got)
	}
	// Without a web host, every service follows the default hostname.
	got = renderWithNames(t)["SystemSettings"].Value["services"]
	for name, raw := range got.(map[string]any) {
		if !reflect.DeepEqual(raw, mail) {
			t.Fatalf("%s names a host without a web host flag: %v", name, raw)
		}
	}
	if len(got.(map[string]any)) != 6 {
		t.Fatalf("unexpected advertised services: %v", got)
	}
}

// Readback compares the stored services exactly: a POP3 entry that an older
// configuration or the upstream default left in place is drift.
func TestServicesReadbackRefusesAStoredPOP3Entry(t *testing.T) {
	declared := renderWithNames(t, "--web-hostname=web.example.test")["SystemSettings"].Value
	stored := map[string]any{}
	for key, value := range declared {
		stored[key] = value
	}
	if !policyMatches(declared, stored, "") {
		t.Fatal("the declared services were refused")
	}
	services := map[string]any{"pop3": map[string]any{"hostname": nil, "cleartext": false}}
	for name, value := range declared["services"].(map[string]any) {
		services[name] = value
	}
	stored["services"] = services
	if policyMatches(declared, stored, "") {
		t.Fatal("a stored POP3 service was accepted")
	}
}

// collectStrings returns every string value in a decoded plan value.
func collectStrings(value any, into *[]string) {
	switch x := value.(type) {
	case string:
		*into = append(*into, x)
	case map[string]any:
		for _, item := range x {
			collectStrings(item, into)
		}
	case []any:
		for _, item := range x {
			collectStrings(item, into)
		}
	}
}

// No expression may name one submission listener without the other: port 465
// would otherwise accept mail without sign-in or with sender checks off. The
// four rules that require sign-in, offer it, check the sender and throttle
// submission must exist, so the check cannot pass on an empty plan.
func TestEverySubmissionRuleCoversEverySubmissionListener(t *testing.T) {
	objects := planObjects(t)
	var expressions []string
	for _, op := range objects {
		collectStrings(map[string]any(op.Value), &expressions)
	}
	mentions := 0
	for _, expression := range expressions {
		named := 0
		for _, listener := range []string{"'submission'", "'submissions'"} {
			if strings.Contains(expression, listener) {
				named++
			}
		}
		if named == 1 {
			t.Errorf("expression names only one submission listener: %s", expression)
		}
		if named == 2 {
			mentions++
		}
	}
	required := map[string]any{
		"sign-in required": objects["MtaStageAuth"].Value["require"],
		"sign-in offered":  objects["MtaStageAuth"].Value["saslMechanisms"],
		"sender checked":   objects["MtaStageMail"].Value["isSenderAllowed"],
		"throttled":        objects["MtaInboundThrottle"].Value["submission-account"],
	}
	for rule, value := range required {
		var found []string
		collectStrings(value, &found)
		joined := strings.Join(found, "\n")
		if !strings.Contains(joined, "'submission'") || !strings.Contains(joined, "'submissions'") {
			t.Errorf("%s does not cover both submission listeners: %v", rule, value)
		}
	}
	if mentions < len(required) {
		t.Fatalf("only %d expressions cover the submission listeners", mentions)
	}
	// The exact sign-in and sender rules, so a negation cannot slip in.
	both := "(listener == 'submission' || listener == 'submissions')"
	if !reflect.DeepEqual(objects["MtaStageAuth"].Value["require"], map[string]any{"match": map[string]any{}, "else": both}) ||
		!reflect.DeepEqual(objects["MtaStageMail"].Value["isSenderAllowed"], map[string]any{"match": map[string]any{}, "else": "(listener != 'submission' && listener != 'submissions') || authenticated_as != ''"}) ||
		!reflect.DeepEqual(objects["MtaStageAuth"].Value["saslMechanisms"], map[string]any{"match": map[string]any{"0": map[string]any{"if": both + " && is_tls", "then": "[plain, login]"}}, "else": "false"}) {
		t.Fatalf("submission sign-in or sender rules differ: %v %v", objects["MtaStageAuth"].Value, objects["MtaStageMail"].Value)
	}
}

// The listeners the rules call submission are exactly the served SMTP
// listeners other than inbound, and 465 uses implicit TLS on its own port.
func TestSubmissionListenersAreTheServedSubmissionPorts(t *testing.T) {
	served := map[string]map[string]any{}
	for _, raw := range planObjects(t)["NetworkListener"].Value {
		listener := raw.(map[string]any)
		if listener["protocol"] == "smtp" && listener["name"] != "smtp-inbound" {
			served[listener["name"].(string)] = listener
		}
	}
	names := make([]string, 0, len(served))
	for name := range served {
		names = append(names, name)
	}
	sort.Strings(names)
	listed := append([]string(nil), submissionListeners...)
	sort.Strings(listed)
	if !reflect.DeepEqual(names, listed) {
		t.Fatalf("served submission listeners %v differ from the rules' %v", names, listed)
	}
	implicit := served["submissions"]
	if implicit == nil || !reflect.DeepEqual(implicit["bind"], map[string]any{"0.0.0.0:1465": true}) || implicit["useTls"] != true || implicit["tlsImplicit"] != true {
		t.Fatalf("implicit-TLS submission is not on 1465 with implicit TLS: %v", implicit)
	}
	starttls := served["submission"]
	if starttls == nil || !reflect.DeepEqual(starttls["bind"], map[string]any{"0.0.0.0:1587": true}) || starttls["tlsImplicit"] != false {
		t.Fatalf("STARTTLS submission changed: %v", starttls)
	}
	if !reflect.DeepEqual(implicit["overrideProxyTrustedNetworks"], map[string]any{"10.0.1.250": true}) {
		t.Fatalf("implicit-TLS submission must trust only the mail load balancer: %v", implicit["overrideProxyTrustedNetworks"])
	}
}

// The census accepts the 465 listener only in its exact profile.
func TestInventoryAcceptsOnlyTheExactImplicitSubmissionProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bind     string
		implicit bool
		want     bool
	}{
		{"exact", "0.0.0.0:1465", true, true},
		{"STARTTLS instead of implicit TLS", "0.0.0.0:1465", false, false},
		{"other port", "0.0.0.0:1587", true, false},
		{"privileged port", "0.0.0.0:465", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := inventoryFixture()
			listener := inventoryListener(v)
			listener["name"], listener["protocol"], listener["bind"] = "submissions", "smtp", map[string]bool{tc.bind: true}
			listener["useTls"], listener["tlsImplicit"] = true, tc.implicit
			data, err := json.Marshal(withApplicationInventory(v))
			if err != nil {
				t.Fatal(err)
			}
			if got := validateInventory(data) == nil; got != tc.want {
				t.Fatalf("accepted=%v want=%v", got, tc.want)
			}
		})
	}
}

func uploadExpression(t *testing.T, ids map[string]string) any {
	t.Helper()
	v := productionFixture()
	v.accountIDs = ids
	return planObjectsFor(t, v)["Http"].Value["allowedEndpoints"]
}

// Uploads are refused everywhere except the maintainer's own upload URL.
func TestUploadsAreAllowedOnlyAtTheMaintainersUploadURL(t *testing.T) {
	refuse := func(condition string) map[string]any {
		return map[string]any{"match": map[string]any{"0": map[string]any{"if": condition, "then": "403"}}, "else": "200"}
	}
	got := uploadExpression(t, map[string]string{"operator": "b", "maintainer": "c", "agents": "d"})
	if !reflect.DeepEqual(got, refuse("starts_with(path, '/jmap/upload/') && path != '/jmap/upload/c/'")) {
		t.Fatalf("upload rule differs: %v", got)
	}
	var text []string
	collectStrings(got, &text)
	for _, other := range []string{"/jmap/upload/b/", "/jmap/upload/d/"} {
		if strings.Contains(strings.Join(text, " "), other) {
			t.Fatalf("upload rule names another account's upload URL %s", other)
		}
	}
	// Before the maintainer's account exists, and for an ID that is not a
	// native JMAP ID, uploads stay refused for every account.
	for _, ids := range []map[string]string{
		nil,
		{"agents": "d"},
		{"maintainer": "c' || true || '"},
		{"maintainer": "C"},
		{"maintainer": "c/"},
		{"maintainer": "c4"},
		{"maintainer": strings.Repeat("a", 14)},
	} {
		if got := uploadExpression(t, ids); !reflect.DeepEqual(got, refuse("starts_with(path, '/jmap/upload/')")) {
			t.Fatalf("%v: uploads were not refused for every account: %v", ids, got)
		}
	}
}

// A heal returns to manifests without --hostname while the store still holds
// the 465 listener a flagged setup wrote. The unflagged plan command accepts
// that stored listener and plans its removal; the flagged one keeps it.
func TestUnflaggedSetupAcceptsAndRemovesAStoredImplicitSubmissionListener(t *testing.T) {
	v := receivingInventory(nil, nil)
	methods := v["methodResponses"].([]any)
	ids, listeners := []string{}, []any{}
	for _, profile := range []struct {
		name, protocol, bind string
		tls, implicit        bool
	}{
		{"management", "http", "0.0.0.0:8080", false, false},
		{"client-proxy", "http", "127.0.0.1:18081", false, false},
		{"smtp-inbound", "smtp", "0.0.0.0:1025", true, false},
		{"submission", "smtp", "0.0.0.0:1587", true, false},
		{"submissions", "smtp", "0.0.0.0:1465", true, true},
		{"imaps", "imap", "0.0.0.0:1993", true, true},
	} {
		ids = append(ids, profile.name+"-id")
		listeners = append(listeners, map[string]any{"id": profile.name + "-id", "name": profile.name, "protocol": profile.protocol, "bind": map[string]bool{profile.bind: true}, "useTls": profile.tls, "tlsImplicit": profile.implicit, "overrideProxyTrustedNetworks": map[string]bool{}, "tlsDisableProtocols": map[string]bool{}})
	}
	methods[0].([]any)[1] = map[string]any{"ids": ids, "total": len(ids), "position": 0}
	methods[1].([]any)[1] = map[string]any{"list": listeners, "notFound": []string{}}
	inventory, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args    []string
		removed bool
	}{
		{nil, true},
		{[]string{"--hostname=mail.example.test"}, false},
	} {
		dir := bootstrapInputs(t)
		if err := os.WriteFile(filepath.Join(dir, "listener-inventory.json"), inventory, 0600); err != nil {
			t.Fatal(err)
		}
		work := t.TempDir()
		if err := os.Chmod(work, 0700); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(work, "plan.ndjson")
		var log bytes.Buffer
		if command(append([]string{"--input-dir", dir, "--output-file", output}, tc.args...), &log) != 0 {
			t.Fatalf("%v: the plan refused a store holding the 465 listener: %s", tc.args, &log)
		}
		plan, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		ops, err := planOperations(plan)
		if err != nil {
			t.Fatal(err)
		}
		removed, declared := false, false
		for _, op := range ops {
			if op.Type == "destroy" {
				removed = reflect.DeepEqual(op.Value, map[string]any{"name": "submissions"}) && op.Object == "NetworkListener"
			}
			if op.Object == "NetworkListener" && op.Type == "upsert" {
				_, declared = op.Value["submissions"]
			}
		}
		if removed != tc.removed || declared == tc.removed {
			t.Fatalf("%v: removed=%v declared=%v", tc.args, removed, declared)
		}
	}
}
