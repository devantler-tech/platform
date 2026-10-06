package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHealthRequiresActualUnsealedCanary(t *testing.T) {
	for _, test := range []struct {
		body string
		ok   bool
	}{
		{`{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"canary"}`, true},
		{`{"initialized":false,"sealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":true,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.5.4","cluster_id":"canary"}`, false},
		{`{"initialized":true,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":""}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"canary"} {}`, false},
		{`{"initialized":true,"sealed":true,"sealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":true,"\u0073ealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":true,"Sealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":false,"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"Initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.5.4","version":"2.6.3","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"other","cluster_id":"canary"}`, false},
		{`{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"canary","native":{"Label":"one","label":"two"}}`, true},
	} {
		if err := verifyHealth(strings.NewReader(test.body)); (err == nil) != test.ok {
			t.Errorf("health accepted=%v want=%v", err == nil, test.ok)
		}
	}
}

func TestDenialRejectsAmbiguousProofFields(t *testing.T) {
	start, _ := time.Parse(time.RFC3339Nano, "2026-10-06T12:00:00Z")
	want := target{pod: "probe", source: "10.1.1.1", destination: "10.1.1.2", node: "prod/node", port: 8204, since: start, until: start.Add(5 * time.Second)}
	valid := `{"flow":{"verdict":"DROPPED","drop_reason_desc":"POLICY_DENIED","traffic_direction":"EGRESS","time":"2026-10-06T12:00:01Z","node_name":"prod/node","source":{"namespace":"arc-runners","pod_name":"probe"},"destination":{"namespace":"openbao","pod_name":"openbao-2"},"IP":{"source":"10.1.1.1","destination":"10.1.1.2"},"l4":{"TCP":{"destination_port":8204,"flags":{"SYN":true}}}}}`
	for _, body := range []string{
		strings.Replace(valid, `"flow":`, `"flow":{"verdict":"FORWARDED"},"flow":`, 1),
		strings.Replace(valid, `"verdict":"DROPPED"`, `"verdict":"FORWARDED","verdict":"DROPPED"`, 1),
		strings.Replace(valid, `"verdict":"DROPPED"`, `"verdict":"FORWARDED","Verdict":"DROPPED"`, 1),
		strings.Replace(valid, `"verdict":`, `"Verdict":`, 1),
		strings.Replace(valid, `"source":"10.1.1.1"`, `"source":"other","source":"10.1.1.1"`, 1),
		strings.Replace(valid, `"destination_port":8204`, `"destination_port":8200,"destination_port":8204`, 1),
		strings.Replace(valid, `"SYN":true`, `"SYN":false,"syn":true`, 1),
		strings.Replace(valid, `"namespace":"arc-runners"`, `"Namespace":"arc-runners"`, 1),
		strings.Replace(valid, `"IP":`, `"ip":`, 1),
		strings.Replace(valid, `"TCP":`, `"tcp":`, 1),
		strings.Replace(valid, `"node_name":"prod/node"`, `"node_name":"other","node_name":"prod/node"`, 1),
		strings.Replace(valid, `"time":"2026-10-06T12:00:01Z"`, `"time":"2026-10-06T11:59:59Z","time":"2026-10-06T12:00:01Z"`, 1),
	} {
		if err := verifyDenial(strings.NewReader(body), want); err == nil {
			t.Errorf("ambiguous proof accepted: %s", body)
		}
	}
	withNative := strings.Replace(valid, `"flow":{`, `"flow":{"native":{"Label":"one","label":"two"},`, 1)
	if err := verifyDenial(strings.NewReader(withNative), want); err != nil {
		t.Fatalf("unrelated native fields rejected: %v", err)
	}
	deepNative := strings.Replace(valid, `"flow":{`, `"flow":{"native":`+strings.Repeat(`[`, 65)+`0`+strings.Repeat(`]`, 65)+`,`, 1)
	if err := verifyDenial(strings.NewReader(deepNative), want); err == nil {
		t.Fatal("excessively nested native evidence accepted")
	}
	ambiguous := strings.Replace(valid, `"verdict":"DROPPED"`, `"verdict":"FORWARDED","verdict":"DROPPED"`, 1)
	if err := verifyDenial(strings.NewReader(valid+"\n"+ambiguous), want); err == nil {
		t.Fatal("ambiguous later document hidden by a valid match")
	}
}

func TestDenialMustJoinTheActualAttempt(t *testing.T) {
	start, _ := time.Parse(time.RFC3339Nano, "2026-10-06T12:00:00Z")
	want := target{pod: "probe", source: "10.1.1.1", destination: "10.1.1.2", node: "prod/node", port: 8204, since: start, until: start.Add(5 * time.Second)}
	valid := map[string]any{"verdict": "DROPPED", "drop_reason_desc": "POLICY_DENIED", "traffic_direction": "EGRESS", "time": "2026-10-06T12:00:01Z", "node_name": "prod/node",
		"source": map[string]any{"namespace": "arc-runners", "pod_name": "probe"}, "destination": map[string]any{"namespace": "openbao", "pod_name": "openbao-2"},
		"IP": map[string]any{"source": "10.1.1.1", "destination": "10.1.1.2"}, "l4": map[string]any{"TCP": map[string]any{"destination_port": 8204, "flags": map[string]any{"SYN": true}}}}
	encode := func(flow map[string]any) string {
		data, _ := json.Marshal(map[string]any{"flow": flow})
		return string(data)
	}
	for _, direction := range []string{"EGRESS", "INGRESS"} {
		valid["traffic_direction"] = direction
		if err := verifyDenial(strings.NewReader(encode(valid)), want); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []func(map[string]any){
		func(v map[string]any) { v["verdict"] = "FORWARDED" },
		func(v map[string]any) { v["drop_reason_desc"] = "CT_TRUNCATED_OR_INVALID_HEADER" },
		func(v map[string]any) { v["traffic_direction"] = "UNKNOWN" },
		func(v map[string]any) { v["node_name"] = "prod/other" },
		func(v map[string]any) { v["time"] = "2026-10-06T11:59:59Z" },
		func(v map[string]any) { v["time"] = "2026-10-06T12:00:06Z" },
		func(v map[string]any) {
			v["source"] = map[string]any{"namespace": "external-secrets", "pod_name": "probe"}
		},
		func(v map[string]any) { v["source"] = map[string]any{"namespace": "arc-runners", "pod_name": "other"} },
		func(v map[string]any) {
			v["destination"] = map[string]any{"namespace": "openbao", "pod_name": "openbao-0"}
		},
		func(v map[string]any) { v["IP"] = map[string]any{"source": "10.1.1.3", "destination": "10.1.1.2"} },
		func(v map[string]any) { v["IP"] = map[string]any{"source": "10.1.1.1", "destination": "10.1.1.4"} },
		func(v map[string]any) {
			v["l4"] = map[string]any{"TCP": map[string]any{"destination_port": 8200, "flags": map[string]any{"SYN": true}}}
		},
		func(v map[string]any) {
			v["l4"] = map[string]any{"TCP": map[string]any{"destination_port": 8204, "flags": map[string]any{"SYN": false}}}
		},
	} {
		var changed map[string]any
		_ = json.Unmarshal([]byte(mustJSON(valid)), &changed)
		mutation(changed)
		if err := verifyDenial(strings.NewReader(encode(changed)), want); err == nil {
			t.Fatal("uncorrelated flow accepted")
		}
	}
	for _, extra := range []string{`{"lost_events":{}}`, `{"lostEvents":{}}`, `{"node_status":{}}`, `{"nodeStatus":{}}`, `{}`, `garbage`, strings.Repeat(" ", 4<<20)} {
		if err := verifyDenial(strings.NewReader(encode(valid)+"\n"+extra), want); err == nil {
			t.Fatal("incomplete evidence accepted")
		}
	}
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
