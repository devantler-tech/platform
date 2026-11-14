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
		"Authentication": {"passwordMinLength": float64(20), "passwordMinStrength": "four"},
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

// TestSecurityEventsAreLoggedAtInfo raises the failed sign-in event to the
// console tracer's level; the server emits it at debug.
func TestSecurityEventsAreLoggedAtInfo(t *testing.T) {
	op := planObjects(t)["EventTracingLevel"]
	if op.Type != "upsert" || len(op.Value) != 1 || !reflect.DeepEqual(op.Value["auth-failed"], map[string]any{"event": "auth.failed", "level": "info"}) {
		t.Fatal("failed sign-ins are not raised to the console level")
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

// TestCollectionReadbackMatchesByIdentity finds throttles by description and
// tracing levels by event, and refuses a widened throttle key.
func TestCollectionReadbackMatchesByIdentity(t *testing.T) {
	ops := planObjects(t)
	throttles := []any{}
	for id, raw := range ops["MtaInboundThrottle"].Value {
		item := map[string]any{"id": id + "-id"}
		for k, v := range raw.(map[string]any) {
			item[k] = v
		}
		throttles = append(throttles, item)
	}
	level := map[string]any{"id": "level-id", "event": "auth.failed", "level": "info"}
	for _, tc := range []struct {
		name   string
		mutate func(throttles []any, level map[string]any)
		valid  bool
	}{
		{"stored as declared", func([]any, map[string]any) {}, true},
		{"widened throttle key", func(throttles []any, _ map[string]any) {
			for _, raw := range throttles {
				item := raw.(map[string]any)
				if item["description"] == "Submission per account" {
					item["key"] = map[string]any{"authenticatedAs": true, "remoteIp": true}
				}
			}
		}, false},
		{"lowered event level", func(_ []any, level map[string]any) { level["level"] = "debug" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := make([]any, len(throttles))
			for i, raw := range throttles {
				item := map[string]any{}
				for k, v := range raw.(map[string]any) {
					item[k] = v
				}
				stored[i] = item
			}
			storedLevel := map[string]any{}
			for k, v := range level {
				storedLevel[k] = v
			}
			tc.mutate(stored, storedLevel)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				if json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var method, tag string
				_ = json.Unmarshal(call.Methods[0][0], &method)
				_ = json.Unmarshal(call.Methods[0][2], &tag)
				var list []any
				switch {
				case strings.HasPrefix(method, "x:MtaInboundThrottle/"):
					list = stored
				case strings.HasPrefix(method, "x:EventTracingLevel/"):
					list = []any{storedLevel}
				default:
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any = map[string]any{"list": list, "notFound": []any{}}
				if strings.HasSuffix(method, "/query") {
					ids := []any{}
					for _, item := range list {
						ids = append(ids, item.(map[string]any)["id"])
					}
					result = map[string]any{"ids": ids, "total": len(ids), "position": 0}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			var plan []byte
			for _, object := range []string{"MtaInboundThrottle", "EventTracingLevel"} {
				raw, err := json.Marshal(ops[object])
				if err != nil {
					t.Fatal(err)
				}
				plan = append(append(plan, raw...), '\n')
			}
			m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
			if (m.verify(context.Background(), plan) == nil) != tc.valid {
				t.Fatal("collection readback accepted drift or rejected the declared state")
			}
		})
	}
}
