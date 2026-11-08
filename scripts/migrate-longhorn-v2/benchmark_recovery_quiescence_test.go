package main

import "testing"

func detachedBenchmarkFixture() (object, []object, []object, []object) {
	volume := resource("scratch-backend")
	volume["spec"] = object{"dataEngine": "v1"}
	volume["status"] = object{"state": "detached"}
	attachment := resource("scratch-attachment")
	attachment["spec"] = object{"volume": "scratch-backend", "attachmentTickets": object{}}
	attachment["status"] = object{"attachmentTicketStatuses": object{}}
	engine := resource("scratch-engine")
	engine["spec"] = object{"volumeName": "scratch-backend", "desireState": "stopped"}
	engine["status"] = object{"currentState": "stopped"}
	return volume, []object{attachment}, []object{engine}, []object{}
}

func TestBenchmarkDetachProofRejectsPendingAndActiveAttachment(t *testing.T) {
	volume, attachments, engines, frontends := detachedBenchmarkFixture()
	if err := verifyRecoveredBenchmarkDetached(volume, attachments, engines, frontends); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"pending node", "current node", "migration node", "pending ticket", "unsatisfied ticket", "running engine", "pending engine", "running frontend", "unknown engine association", "unread attachments", "unread engines", "unread frontends", "malformed node", "unknown state"} {
		t.Run(scenario, func(t *testing.T) {
			v, a, e, f := detachedBenchmarkFixture()
			switch scenario {
			case "pending node":
				v["spec"].(object)["nodeID"] = "another-node"
			case "current node":
				v["status"].(object)["currentNodeID"] = "another-node"
			case "migration node":
				v["spec"].(object)["migrationNodeID"] = "another-node"
			case "pending ticket":
				a[0]["spec"].(object)["attachmentTickets"] = object{"pending": object{"nodeID": "another-node"}}
			case "unsatisfied ticket":
				a[0]["status"].(object)["attachmentTicketStatuses"] = object{"pending": object{"satisfied": false}}
			case "running engine":
				e[0]["status"].(object)["currentState"] = "running"
			case "pending engine":
				e[0]["spec"].(object)["desireState"] = "running"
			case "running frontend":
				f = append(f, resource("frontend"))
				f[0]["spec"] = object{"volumeName": "scratch-backend", "desireState": "running"}
				f[0]["status"] = object{"currentState": "stopped"}
			case "unknown engine association":
				delete(e[0]["spec"].(object), "volumeName")
			case "unread attachments":
				a = nil
			case "unread engines":
				e = nil
			case "unread frontends":
				f = nil
			case "malformed node":
				v["spec"].(object)["nodeID"] = object{}
			case "unknown state":
				delete(v["status"].(object), "state")
			}
			if verifyRecoveredBenchmarkDetached(v, a, e, f) == nil {
				t.Fatal("unproven detached state accepted")
			}
		})
	}
}

func TestBenchmarkBackendNameCascadeRequiresNaturalStatusClearance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status object
		ok     bool
	}{
		{"clear", object{"kubernetesStatus": object{}}, true},
		{"PV still named", object{"kubernetesStatus": object{"pvName": "recreated-pv"}}, false},
		{"PVC still active", object{"kubernetesStatus": object{"pvcName": "fio-data", "namespace": "scratch"}}, false},
		{"historical PVC reference", object{"kubernetesStatus": object{"pvcName": "fio-data", "namespace": "scratch", "lastPVCRefAt": "2026-11-02T14:00:00Z"}}, true},
		{"malformed timestamp", object{"kubernetesStatus": object{"pvcName": "fio-data", "lastPVCRefAt": "not-a-time"}}, false},
		{"missing Kubernetes state", object{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume := resource("scratch-backend")
			volume["status"] = tc.status
			if err := verifyRecoveredBackendCascadeCleared(volume); (err == nil) != tc.ok {
				t.Fatalf("cascade clearance=%v want=%v err=%v", err == nil, tc.ok, err)
			}
		})
	}
}
