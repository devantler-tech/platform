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
			"dkimVerify": inboundOnly("relaxed", "disable"), "dmarcVerify": inboundOnly("relaxed", "disable"),
			"arcVerify": inboundOnly("relaxed", "disable"), "dkimStrict": true,
		},
		"MtaStageData": {
			"addAuthResultsHeader": inboundOnly("true", "false"), "addReceivedHeader": inboundOnly("true", "false"),
			"addReturnPathHeader": inboundOnly("true", "false"),
		},
		"MtaStageEhlo":        {"rejectNonFqdn": inboundOnly("true", "false")},
		"MtaExtensions":       {"vrfy": map[string]any{"match": map[string]any{}, "else": "false"}, "expn": map[string]any{"match": map[string]any{}, "else": "false"}},
		"Security":            {"authBanPeriod": day, "abuseBanPeriod": day, "loiterBanPeriod": day, "scanBanPeriod": day},
		"SpamPyzor":           {"enable": false},
		"Asn":                 {"@type": "Disabled"},
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

// TestVerificationValuesAreBareConstants keeps verification values unquoted.
// The server ignores a quoted string and verifies relaxed on every listener.
func TestVerificationValuesAreBareConstants(t *testing.T) {
	op := planObjects(t)["SenderAuth"]
	for _, field := range []string{"dkimVerify", "dmarcVerify", "arcVerify"} {
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
