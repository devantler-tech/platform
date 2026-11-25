package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// planObjects renders the fixture plan and returns each operation by object.
func planObjects(t *testing.T) map[string]operation {
	t.Helper()
	plan, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	byObject := map[string]operation{}
	for _, op := range ops {
		if op.Object != "Account" {
			if _, seen := byObject[op.Object]; seen {
				t.Fatalf("%s is declared twice", op.Object)
			}
			byObject[op.Object] = op
		}
	}
	return byObject
}

// inboundOnly is the expression shape that applies a value only on the public
// inbound listener, which upstream defaults miss because it is not port 25.
func inboundOnly(value, otherwise string) map[string]any {
	return map[string]any{"match": map[string]any{"0": map[string]any{"if": "listener == 'smtp-inbound'", "then": value}}, "else": otherwise}
}

// TestSecureDefaultsAreDeclared pins every audited setting (#5387) so a later
// change cannot silently drop one back to the upstream default.
func TestSecureDefaultsAreDeclared(t *testing.T) {
	ops := planObjects(t)
	day := float64(24 * 60 * 60 * 1000)
	for object, want := range map[string]map[string]any{
		"SenderAuth": {
			"dkimVerify": inboundOnly("relaxed", "disable"), "dmarcVerify": inboundOnly("strict", "disable"),
			"arcVerify": inboundOnly("relaxed", "disable"), "dkimStrict": true,
			"spfEhloVerify": inboundOnly("relaxed", "disable"), "spfFromVerify": inboundOnly("relaxed", "disable"),
			"reverseIpVerify": inboundOnly("relaxed", "disable"),
		},
		"MtaStageData": {
			"addAuthResultsHeader": inboundOnly("true", "false"), "addReceivedHeader": inboundOnly("true", "false"),
			"addReturnPathHeader": inboundOnly("true", "false"),
		},
		"MtaStageEhlo":        {"rejectNonFqdn": inboundOnly("true", "false")},
		"MtaExtensions":       {"vrfy": map[string]any{"match": map[string]any{}, "else": "false"}, "expn": map[string]any{"match": map[string]any{}, "else": "false"}},
		"Security":            {"authBanPeriod": day, "abuseBanPeriod": day, "loiterBanPeriod": day, "scanBanPeriod": day},
		"SpamPyzor":           {"enable": false},
		"SpamClassifier":      {"holdSamplesFor": 30 * day},
		"DmarcReportSettings": {"failureSendFrequency": map[string]any{"match": map[string]any{}, "else": "false"}},
		"DkimReportSettings":  {"sendFrequency": map[string]any{"match": map[string]any{}, "else": "false"}},
		"SpfReportSettings":   {"sendFrequency": map[string]any{"match": map[string]any{}, "else": "false"}},
		"DataRetention":       {"expungeTrashAfter": 30 * day},
	} {
		op, ok := ops[object]
		if !ok || op.Type != "update" {
			t.Fatalf("%s is not declared as a singleton update", object)
		}
		for field, value := range want {
			if !reflect.DeepEqual(op.Value[field], value) {
				t.Fatalf("%s.%s = %v, want %v", object, field, op.Value[field], value)
			}
		}
	}
}

// senderVerification names every SenderAuth verification expression.
var senderVerification = []string{"dkimVerify", "dmarcVerify", "arcVerify", "spfEhloVerify", "spfFromVerify", "reverseIpVerify"}

// TestVerificationValuesAreBareConstants keeps verification values unquoted.
// The server ignores a quoted string and verifies relaxed on every listener.
func TestVerificationValuesAreBareConstants(t *testing.T) {
	op := planObjects(t)["SenderAuth"]
	for _, field := range senderVerification {
		expression, _ := op.Value[field].(map[string]any)
		values := []any{expression["else"]}
		branches, _ := expression["match"].(map[string]any)
		for _, branch := range branches {
			values = append(values, branch.(map[string]any)["then"])
		}
		for _, value := range values {
			text, ok := value.(string)
			if !ok || (text != "relaxed" && text != "strict" && text != "disable") {
				t.Fatalf("%s uses %v, not a bare verification constant", field, value)
			}
		}
	}
}

// singletonReadback serves one stored singleton, answering only the requested
// properties as the server does, and verifies the operation against it.
func singletonReadback(t *testing.T, op operation, stored map[string]any) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Methods [][]json.RawMessage `json:"methodCalls"`
		}
		var args struct {
			IDs        []string `json:"ids"`
			Properties []string `json:"properties"`
		}
		var method, tag string
		if json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 || len(call.Methods[0]) != 3 ||
			json.Unmarshal(call.Methods[0][0], &method) != nil || method != "x:"+op.Object+"/get" ||
			json.Unmarshal(call.Methods[0][1], &args) != nil || json.Unmarshal(call.Methods[0][2], &tag) != nil ||
			!reflect.DeepEqual(args.IDs, []string{"singleton"}) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		item := map[string]any{}
		for _, property := range args.Properties {
			if value, ok := stored[property]; ok {
				item[property] = value
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, map[string]any{"list": []any{item}, "notFound": []any{}}, tag}}})
	}))
	defer server.Close()
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
	return m.verify(context.Background(), append(raw, '\n'))
}

// TestSenderAuthReadbackRefusesWeakerVerification reads the inbound checks back,
// so a stored upstream default (keyed on local port 25, which no listener here
// uses) or a DMARC check that no longer enforces policy is reported as drift.
func TestSenderAuthReadbackRefusesWeakerVerification(t *testing.T) {
	op := planObjects(t)["SenderAuth"]
	portKeyed := map[string]any{"match": map[string]any{"0": map[string]any{"if": "local_port == 25", "then": "relaxed"}}, "else": "disable"}
	for _, tc := range []struct {
		name, field string
		value       any
		valid       bool
	}{
		{"stored as declared", "", nil, true},
		{"SPF on EHLO keyed on port 25", "spfEhloVerify", portKeyed, false},
		{"SPF on MAIL FROM keyed on port 25", "spfFromVerify", portKeyed, false},
		{"reverse IP keyed on port 25", "reverseIpVerify", portKeyed, false},
		{"reverse IP disabled", "reverseIpVerify", map[string]any{"match": map[string]any{}, "else": "disable"}, false},
		{"DMARC relaxed", "dmarcVerify", inboundOnly("relaxed", "disable"), false},
		{"DMARC strict on every listener", "dmarcVerify", map[string]any{"match": map[string]any{}, "else": "strict"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := map[string]any{"id": "singleton"}
			for field, value := range op.Value {
				stored[field] = value
			}
			if tc.field != "" {
				stored[tc.field] = tc.value
			}
			if (singletonReadback(t, op, stored) == nil) != tc.valid {
				t.Fatal("inbound verification readback accepted a weaker check or rejected the declared one")
			}
		})
	}
}

// TestSenderAuthIsTheSameOnTheCandidateSchema keeps the 1.x candidate's inbound
// checks equal: v1.0.3 names these fields and constants as v0.16.27 does.
func TestSenderAuthIsTheSameOnTheCandidateSchema(t *testing.T) {
	want := planObjects(t)["SenderAuth"]
	v := fixture()
	v.RuntimeVersion = "1.0.3"
	plan, err := render(v)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Object == "SenderAuth" {
			if !reflect.DeepEqual(op, want) {
				t.Fatalf("candidate inbound checks differ: %v", op.Value)
			}
			return
		}
	}
	t.Fatal("candidate plan declares no inbound checks")
}

// TestAuthenticationUpdateKeepsOnlyDefaultValues keeps the native role defaults.
// On a fresh store the server installs them at first normal start only while the
// Authentication settings have no stored row, and an update storing a
// non-default value creates one, leaving the administrator without permissions.
func TestAuthenticationUpdateKeepsOnlyDefaultValues(t *testing.T) {
	op := planObjects(t)["Authentication"]
	if op.Type != "update" || !reflect.DeepEqual(op.Value, map[string]any{"directoryId": nil}) {
		t.Fatalf("Authentication update stores more than its default values: %v", op.Value)
	}
}

// TestSeededObjectsAreDeclaredOnlyAsReviewed limits the plan to the reviewed
// forms of the objects the server seeds at first normal start. The setup runs
// before that start: a default-only update stores nothing and is overwritten by
// the seed (Asn), and a stored row stops the seed (Authentication role defaults,
// OidcProvider signing keys).
func TestSeededObjectsAreDeclaredOnlyAsReviewed(t *testing.T) {
	reviewed := map[string]bool{"Application": true, "Authentication": true, "MtaInboundThrottle": true, "MtaRoute": true, "MtaTlsStrategy": true, "NetworkListener": true, "SystemSettings": true, "Tracer": true}
	for _, seeded := range []string{"Application", "Asn", "Authentication", "Jmap", "MetricsStore", "MtaConnectionStrategy", "MtaDeliverySchedule", "MtaInboundThrottle", "MtaQueueQuota", "MtaRoute", "MtaTlsStrategy", "MtaVirtualQueue", "NetworkListener", "OidcProvider", "Role", "SpamRule", "SystemSettings", "Tracer", "TracingStore"} {
		if _, declared := planObjects(t)[seeded]; declared && !reviewed[seeded] {
			t.Fatalf("%s is seeded at first normal start and is not a reviewed declaration", seeded)
		}
	}
}

// TestInboundThrottlesAreDeclared declares the two server-seeded throttles with
// the submission limit, because the server seeds only an empty collection.
func TestInboundThrottlesAreDeclared(t *testing.T) {
	op := planObjects(t)["MtaInboundThrottle"]
	always := map[string]any{"match": map[string]any{}, "else": "true"}
	want := map[string]any{
		"sender-ip":        map[string]any{"description": "Sender IP throttle", "enable": true, "key": map[string]any{"remoteIp": true}, "match": always, "rate": map[string]any{"count": float64(5), "period": float64(1000)}},
		"sender-recipient": map[string]any{"description": "Sender address to recipient throttle", "enable": true, "key": map[string]any{"senderDomain": true, "rcpt": true}, "match": always, "rate": map[string]any{"count": float64(25), "period": float64(3600000)}},
		"submission-account": map[string]any{"description": "Submission per account", "enable": true, "key": map[string]any{"authenticatedAs": true},
			"match": map[string]any{"match": map[string]any{"0": map[string]any{"if": "listener == 'submission'", "then": "true"}}, "else": "false"}, "rate": map[string]any{"count": float64(100), "period": float64(3600000)}},
	}
	if op.Type != "upsert" || !reflect.DeepEqual(op.Value, want) {
		t.Fatalf("inbound throttles differ: %v", op.Value)
	}
}

// TestThrottleReadbackMatchesByDescription finds each throttle by its
// description and refuses a widened throttle key.
func TestThrottleReadbackMatchesByDescription(t *testing.T) {
	op := planObjects(t)["MtaInboundThrottle"]
	for _, tc := range []struct {
		name  string
		key   map[string]any
		valid bool
	}{
		{"stored as declared", map[string]any{"authenticatedAs": true}, true},
		{"widened throttle key", map[string]any{"authenticatedAs": true, "remoteIp": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := []any{}
			for ref, raw := range op.Value {
				item := map[string]any{"id": ref + "-id"}
				for k, v := range raw.(map[string]any) {
					item[k] = v
				}
				if item["description"] == "Submission per account" {
					item["key"] = tc.key
				}
				stored = append(stored, item)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				var method, tag string
				if json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 || json.Unmarshal(call.Methods[0][0], &method) != nil || json.Unmarshal(call.Methods[0][2], &tag) != nil || !strings.HasPrefix(method, "x:MtaInboundThrottle/") {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any = map[string]any{"list": stored, "notFound": []any{}}
				if strings.HasSuffix(method, "/query") {
					ids := []any{}
					for _, item := range stored {
						ids = append(ids, item.(map[string]any)["id"])
					}
					result = map[string]any{"ids": ids, "total": len(ids), "position": 0}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			raw, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
			if (m.verify(context.Background(), append(raw, '\n')) == nil) != tc.valid {
				t.Fatal("throttle readback accepted a widened key or rejected the declared state")
			}
		})
	}
}
