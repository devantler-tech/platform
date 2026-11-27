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

func TestCNPGObservationCoversEveryInstanceOfSmallerSources(t *testing.T) {
	for _, scope := range []struct {
		namespace, name string
		instances       int
	}{
		{"ate-system", "substrate-db", 2},
		{"kagent", "kagent-db", 2},
		{"customer-identity", "customer-identity-db", 2},
		{"business-site", "business-site-portal-db", 2},
		{"business-site-dev", "portal-preview-db", 1},
	} {
		t.Run(scope.name, func(t *testing.T) {
			before := cnpgObservationFixture(t)
			before.Pods = before.Pods[:scope.instances]
			meta := before.Cluster["metadata"].(object)
			meta["name"], meta["namespace"] = scope.name, scope.namespace
			before.Cluster["spec"].(object)["instances"] = float64(scope.instances)
			status := before.Cluster["status"].(object)
			status["instances"], status["readyInstances"] = float64(scope.instances), float64(scope.instances)
			status["currentPrimary"], status["targetPrimary"] = scope.name+"-1", scope.name+"-1"
			healthy := make([]any, 0, scope.instances)
			states := make(map[string]postgresState, scope.instances)
			for _, pod := range before.Pods {
				podMeta := pod["metadata"].(object)
				oldName := podMeta["name"].(string)
				newName := scope.name + "-" + oldName[len(oldName)-1:]
				podMeta["name"], podMeta["namespace"] = newName, scope.namespace
				podMeta["labels"].(object)["cnpg.io/cluster"] = scope.name
				podMeta["ownerReferences"].([]any)[0].(object)["name"] = scope.name
				states[newName] = before.States[oldName]
				healthy = append(healthy, newName)
			}
			before.States = states
			status["instancesStatus"] = object{"healthy": healthy}
			after := cloneCNPGObservation(t, before)
			if err := validateCNPGObservation(before, after); err != nil {
				t.Fatalf("complete healthy source was rejected: %v", err)
			}
			foreign := cloneCNPGObservation(t, before)
			valueObject(foreign.Cluster, "metadata")["namespace"] = "unreviewed"
			for _, pod := range foreign.Pods {
				valueObject(pod, "metadata")["namespace"] = "unreviewed"
			}
			if validateCNPGObservation(foreign, foreign) == nil {
				t.Fatal("an identically named source in a foreign namespace was accepted")
			}
			delete(after.States, text(after.Pods[len(after.Pods)-1], "metadata", "name"))
			if validateCNPGObservation(before, after) == nil {
				t.Fatal("missing physical coverage was accepted")
			}
		})
	}
}

func umamiV2TransitionObservation(t *testing.T) cnpgObservation {
	t.Helper()
	before := cnpgObservationFixture(t)
	valueObject(before.Cluster, "spec")["instances"] = float64(4)
	valueObject(before.Cluster, "spec")["storage"] = object{
		"storageClass": "longhorn-v2", "resizeInUseVolumes": false, "size": "5Gi",
	}
	status := valueObject(before.Cluster, "status")
	status["instances"], status["readyInstances"] = float64(4), float64(4)
	valueObject(status, "instancesStatus")["healthy"] = []any{"umami-db-1", "umami-db-2", "umami-db-3", "umami-db-4"}
	fourth := cloneCNPGObservation(t, before).Pods[2]
	valueObject(fourth, "metadata")["name"] = "umami-db-4"
	valueObject(fourth, "metadata")["uid"] = "88888888-8888-4888-8888-888888888888"
	value(fourth, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("e", 64)
	before.Pods = append(before.Pods, fourth)
	before.States["umami-db-4"] = before.States["umami-db-3"]
	return before
}

func TestCNPGObservationCoversFourInstancesDuringUmamiV2Transition(t *testing.T) {
	before := umamiV2TransitionObservation(t)
	after := cloneCNPGObservation(t, before)
	if err := validateCNPGObservation(before, after); err != nil {
		t.Fatal(err)
	}

	delete(after.States, "umami-db-4")
	if validateCNPGObservation(before, after) == nil {
		t.Fatal("an unobserved spare was accepted")
	}
	valueObject(before.Cluster, "spec", "storage")["storageClass"] = "longhorn-wffc"
	if validateCNPGObservation(before, before) == nil {
		t.Fatal("a fourth instance outside the V2 transition was accepted")
	}
}
