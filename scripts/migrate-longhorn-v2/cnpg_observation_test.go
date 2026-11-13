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
		state := postgresState{SystemID: "1234567890123456789", Timeline: "7", ReplayLSN: "2/101", InRecovery: true, PostmasterStart: "2026-11-01 08:00:00.000000+00"}
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
		"in-place server restart": func(s *cnpgObservation) {
			p := s.States["umami-db-2"]
			p.PostmasterStart = "2026-11-07 16:00:00.000000+00"
			s.States["umami-db-2"] = p
		},
		"server start unobserved": func(s *cnpgObservation) {
			p := s.States["umami-db-3"]
			p.PostmasterStart = ""
			s.States["umami-db-3"] = p
		},
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

// TestCNPGObservationCoversTheCurrentTwoInstanceSources requires both members
// and the same native physical replay evidence as the three-instance sources.
func TestCNPGObservationCoversTheCurrentTwoInstanceSources(t *testing.T) {
	for _, scope := range []struct{ namespace, name string }{{"ate-system", "substrate-db"}, {"kagent", "kagent-db"}} {
		t.Run(scope.name, func(t *testing.T) {
			before := cnpgObservationFixture(t)
			before.Pods = before.Pods[:2]
			meta := before.Cluster["metadata"].(object)
			meta["name"], meta["namespace"] = scope.name, scope.namespace
			before.Cluster["spec"].(object)["instances"] = float64(2)
			status := before.Cluster["status"].(object)
			status["instances"], status["readyInstances"] = float64(2), float64(2)
			status["currentPrimary"], status["targetPrimary"] = scope.name+"-1", scope.name+"-1"
			status["instancesStatus"] = object{"healthy": []any{scope.name + "-1", scope.name + "-2"}}
			delete(before.States, "umami-db-3")
			for _, pod := range before.Pods {
				podMeta := pod["metadata"].(object)
				oldName := podMeta["name"].(string)
				newName := scope.name + "-" + oldName[len(oldName)-1:]
				podMeta["name"], podMeta["namespace"] = newName, scope.namespace
				podMeta["labels"].(object)["cnpg.io/cluster"] = scope.name
				podMeta["ownerReferences"].([]any)[0].(object)["name"] = scope.name
				before.States[newName] = before.States[oldName]
				delete(before.States, oldName)
			}
			after := cloneCNPGObservation(t, before)
			if err := validateCNPGObservation(before, after); err != nil {
				t.Fatalf("complete healthy two-instance source was rejected: %v", err)
			}
			delete(after.States, scope.name+"-2")
			if validateCNPGObservation(before, after) == nil {
				t.Fatal("missing physical coverage was accepted")
			}
		})
	}
}
