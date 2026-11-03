package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// cnpgObservationFixture supplies native-shaped healthy processes and replay positions.
func cnpgObservationFixture(t *testing.T) cnpgObservation {
	t.Helper()
	c, pods, _, _ := cnpgFixture("longhorn-wffc", "longhorn-wffc", "longhorn-wffc")
	c["metadata"].(object)["generation"] = float64(1)
	c["spec"].(object)["storage"].(object)["storageClass"] = "longhorn-wffc"
	states := map[string]postgresState{}
	for i, p := range pods {
		p["status"].(object)["containerStatuses"] = []any{object{"name": "postgres", "ready": true, "restartCount": float64(0), "imageID": "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64), "containerID": "containerd://" + strings.Repeat(string(rune('b'+i)), 64)}}
		state := postgresState{SystemID: "1234567890123456789", Timeline: "7", ReplayLSN: "2/101", InRecovery: true}
		if i == 0 {
			state.FlushLSN, state.InRecovery = "2/100", false
		}
		states[text(p, "metadata", "name")] = state
	}
	return cnpgObservation{Cluster: c, Pods: pods, States: states}
}

// cloneCNPGObservation keeps mutation controls independent from their baseline.
func cloneCNPGObservation(t *testing.T, original cnpgObservation) cnpgObservation {
	t.Helper()
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var result cnpgObservation
	if err = json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestCNPGObservationAcceptsOnlyStablePhysicalReplication brackets every SQL read.
func TestCNPGObservationAcceptsOnlyStablePhysicalReplication(t *testing.T) {
	before := cnpgObservationFixture(t)
	after := cloneCNPGObservation(t, before)
	if err := validateCNPGObservation(before, after); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*cnpgObservation){
		"cluster replacement": func(s *cnpgObservation) { s.Cluster["metadata"].(object)["uid"] = "replacement" },
		"source spec changed": func(s *cnpgObservation) { s.Cluster["spec"].(object)["imageName"] = "other" },
		"switchover started":  func(s *cnpgObservation) { s.Cluster["status"].(object)["targetPrimary"] = "umami-db-2" },
		"pod replacement":     func(s *cnpgObservation) { s.Pods[1]["metadata"].(object)["uid"] = "replacement" },
		"process restart": func(s *cnpgObservation) {
			value(s.Pods[1], "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
		},
		"incomplete physical coverage": func(s *cnpgObservation) { delete(s.States, "umami-db-3") },
		"foreign physical coverage":    func(s *cnpgObservation) { s.States["other"] = s.States["umami-db-3"] },
		"lagging standby": func(s *cnpgObservation) {
			p := s.States["umami-db-3"]
			p.ReplayLSN = "2/FF"
			s.States["umami-db-3"] = p
		},
		"divergent standby": func(s *cnpgObservation) {
			p := s.States["umami-db-2"]
			p.SystemID = "1234567890123456788"
			s.States["umami-db-2"] = p
		},
		"timeline changed":    func(s *cnpgObservation) { p := s.States["umami-db-1"]; p.Timeline = "8"; s.States["umami-db-1"] = p },
		"primary in recovery": func(s *cnpgObservation) { p := s.States["umami-db-1"]; p.InRecovery = true; s.States["umami-db-1"] = p },
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneCNPGObservation(t, before)
			change(&changed)
			if validateCNPGObservation(before, changed) == nil {
				t.Fatal("unproven physical replication accepted")
			}
		})
	}
}
