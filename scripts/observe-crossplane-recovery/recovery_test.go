package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func goodEvidence() recoveryEvidence {
	return recoveryEvidence{
		Configuration: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"crossplane-sync-exporter","namespace":"observability"},"data":{"managed-resources.tsv":"sample-v1"}}`),
		Deployment:    []byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"crossplane-sync-exporter","namespace":"observability","generation":8},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"platform.devantler.tech/config-checksum":"reviewed-checksum"}}}},"status":{"observedGeneration":8,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1,"conditions":[{"type":"Progressing","status":"True","reason":"NewReplicaSetAvailable","lastUpdateTime":"2026-10-27T23:00:00Z"},{"type":"Available","status":"True"}]}}`),
		Cron:          []byte(`{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"crossplane-sync-alerter","namespace":"observability"},"spec":{"suspend":false},"status":{"lastScheduleTime":"2026-10-27T23:05:00Z","lastSuccessfulTime":"2026-10-27T23:05:20Z"}}`),
		Peers:         [2][]byte{[]byte("[]"), []byte("[]")},
	}
}

var observationTime = time.Date(2026, 10, 27, 23, 6, 0, 0, time.UTC)

func TestRecoveryRequiresCompleteFreshEvidence(t *testing.T) {
	count, err := assessRecovery(goodEvidence(), map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime)
	if err != nil || count != 0 {
		t.Fatalf("complete healthy evidence should recover: count=%d err=%v", count, err)
	}
}

func TestRecoveryDoesNotClearSilencedOrInhibitedAlerts(t *testing.T) {
	for _, state := range []string{"active", "suppressed", "unprocessed"} {
		for _, peer := range []int{0, 1} {
			t.Run(state+string(rune('0'+peer)), func(t *testing.T) {
				e := goodEvidence()
				e.Peers[peer] = []byte(`[{"labels":{"alertname":"CrossplaneManagedKindsUncovered"},"annotations":{},"status":{"state":"` + state + `","silencedBy":["silence"],"inhibitedBy":["inhibition"]},"startsAt":"2026-10-27T22:55:00Z","endsAt":"2026-10-27T23:10:00Z","updatedAt":"2026-10-27T22:55:00Z","receivers":[{"name":"default"}],"fingerprint":"synthetic","generatorURL":""}]`)
				count, err := assessRecovery(e, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime)
				if err != nil || count != 1 {
					t.Fatalf("a retained Crossplane alert must remain present: count=%d err=%v", count, err)
				}
			})
		}
	}
}

func TestRecoveryRejectsIncompleteEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*recoveryEvidence)
	}{
		{"empty peer", func(e *recoveryEvidence) { e.Peers[1] = nil }},
		{"null peer", func(e *recoveryEvidence) { e.Peers[0] = []byte("null") }},
		{"trailing document", func(e *recoveryEvidence) { e.Peers[1] = []byte("[] []") }},
		{"malformed peer", func(e *recoveryEvidence) { e.Peers[0] = []byte("{") }},
		{"missing alert identity", func(e *recoveryEvidence) { e.Peers[0] = []byte(`[{"labels":{},"status":{"state":"active"}}]`) }},
		{"unknown alert state", func(e *recoveryEvidence) {
			e.Peers[0] = []byte(`[{"labels":{"alertname":"Other"},"status":{"state":"unknown"}}]`)
		}},
		{"too many alerts", func(e *recoveryEvidence) {
			e.Peers[0] = []byte("[" + strings.Repeat(`{"labels":{"alertname":"Other"},"status":{"state":"active"}},`, 500) + `{"labels":{"alertname":"Other"},"status":{"state":"active"}}]`)
		}},
		{"oversized peer", func(e *recoveryEvidence) { e.Peers[0] = []byte(strings.Repeat(" ", 1048577)) }},
		{"changed configuration", func(e *recoveryEvidence) {
			e.Configuration = []byte(`{"metadata":{"name":"crossplane-sync-exporter","namespace":"observability"},"data":{"managed-resources.tsv":"drift"}}`)
		}},
		{"unloaded config", func(e *recoveryEvidence) {
			e.Deployment = []byte(strings.ReplaceAll(string(e.Deployment), "reviewed-checksum", "old-checksum"))
		}},
		{"stale generation", func(e *recoveryEvidence) {
			e.Deployment = []byte(strings.ReplaceAll(string(e.Deployment), `"observedGeneration":8`, `"observedGeneration":7`))
		}},
		{"unready exporter", func(e *recoveryEvidence) {
			e.Deployment = []byte(strings.ReplaceAll(string(e.Deployment), `"readyReplicas":1`, `"readyReplicas":0`))
		}},
		{"suspended checker", func(e *recoveryEvidence) {
			e.Cron = []byte(strings.ReplaceAll(string(e.Cron), `"suspend":false`, `"suspend":true`))
		}},
		{"stale checker", func(e *recoveryEvidence) { e.Cron = []byte(strings.ReplaceAll(string(e.Cron), "23:05:20", "22:45:20")) }},
		{"pre-rollout checker", func(e *recoveryEvidence) { e.Cron = []byte(strings.ReplaceAll(string(e.Cron), "23:05:20", "22:59:59")) }},
		{"future checker", func(e *recoveryEvidence) { e.Cron = []byte(strings.ReplaceAll(string(e.Cron), "23:05:20", "23:09:00")) }},
		{"missing successful check", func(e *recoveryEvidence) {
			var c map[string]any
			_ = json.Unmarshal(e.Cron, &c)
			delete(c["status"].(map[string]any), "lastSuccessfulTime")
			e.Cron, _ = json.Marshal(c)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := goodEvidence()
			c.mutate(&e)
			if _, err := assessRecovery(e, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime); err == nil {
				t.Fatal("incomplete evidence must be UNKNOWN")
			}
		})
	}
}

func TestRecoveryPreservesOtherAlertFamilies(t *testing.T) {
	e := goodEvidence()
	e.Peers[0] = []byte(`[{"labels":{"alertname":"UnrelatedWarning"},"annotations":{},"status":{"state":"active","silencedBy":[],"inhibitedBy":[]},"startsAt":"2026-10-27T22:00:00Z","endsAt":"2026-10-28T00:00:00Z","updatedAt":"2026-10-27T22:00:00Z","receivers":[],"fingerprint":"synthetic","generatorURL":""}]`)
	count, err := assessRecovery(e, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime)
	if err != nil || count != 0 {
		t.Fatalf("only the named component's recovery is judged: count=%d err=%v", count, err)
	}
}
