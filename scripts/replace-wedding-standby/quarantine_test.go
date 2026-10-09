package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRecoveryChecksClaimIdentityRatherThanReusableName checks ordinal reuse.
// A name is not a storage identity:
// only a newly bound claim can safely use that name after the old PV is retained.
func TestRecoveryChecksClaimIdentityRatherThanReusableName(t *testing.T) {
	for _, sameUID := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh claim", true: "retained claim"}[sameUID], func(t *testing.T) {
			s := fixture()
			oldClaim := id(s.claims[0])
			p := plan{primary: id(s.pods[1]), healthy: id(s.pods[2]), target: id(s.pods[0])}
			at(s.pods[0], "metadata")["uid"] = "replacement-pod"
			at(s.pods[0], "status")["phase"] = "Running"
			at(s.pods[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
			at(s.cluster, "status")["readyInstances"] = float64(3)
			appendCondition(s.cluster, object{"type": "Ready", "status": "True"})
			if !sameUID {
				at(s.claims[0], "metadata")["uid"] = "replacement-claim"
				at(s.claims[0], "spec")["volumeName"] = "replacement-pv"
				pv := object{"metadata": object{"name": "replacement-pv", "uid": "replacement-volume", "resourceVersion": "1"}}
				pv["spec"] = object{"claimRef": object{"name": targetName, "namespace": namespace, "uid": "replacement-claim"}}
				pv["status"] = object{"phase": "Bound"}
				s.volumes["replacement-pv"] = pv
			}
			c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" && args[1] == "pvc" && args[2] == targetName {
					return json.Marshal(s.claims[0])
				}
				return fakeRead(s, args[1:])
			}}
			ok, err := c.recovered(context.Background(), s, testOptions(), p, oldClaim)
			if sameUID {
				if ok || err == nil {
					t.Fatal("retained claim reuse was accepted")
				}
			} else if !ok || err != nil {
				t.Fatalf("new claim identity under the missing ordinal was refused: %v", err)
			}
		})
	}
}

// completedFixture models the bound, completed-bootstrap two-peer HOLD.
func completedFixture() (inventory, storageGuards) {
	s := fixture()
	at(s.claims[0], "metadata")["ownerReferences"] = []any{}
	at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
	at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	at(s.volumes["old-pv"], "spec")["csi"] = object{"driver": "driver.longhorn.io", "volumeHandle": "original-volume"}
	j := object{"metadata": object{"name": targetName + "-join", "namespace": namespace, "uid": "job-uid", "resourceVersion": "10", "ownerReferences": value(s.pods[0], "metadata", "ownerReferences"), "labels": object{"cnpg.io/jobRole": "join", "cnpg.io/instanceName": targetName}}, "status": object{"succeeded": float64(1), "completionTime": testNow.Add(-time.Minute).Format(time.RFC3339)}, "spec": object{"template": object{"spec": value(s.pods[0], "spec")}}}
	s.jobs = []object{j}
	at(s.pods[0], "metadata")["name"] = targetName + "-join-fixture"
	at(s.pods[0], "metadata")["ownerReferences"] = []any{object{"apiVersion": "batch/v1", "kind": "Job", "name": id(j).name, "uid": id(j).uid, "controller": true}}
	at(s.pods[0], "status")["phase"] = "Succeeded"
	at(s.pods[0], "status")["containerStatuses"] = []any{object{"name": "join", "state": object{"terminated": object{"reason": "Completed", "exitCode": float64(0)}}}}
	return s, storageGuards{"job-uid", "claim-uid", "pv-uid"}
}

// leaderFixture supplies one live lease and its ready, version-bound operator.
func leaderFixture() (object, object) {
	lease := object{"metadata": object{"name": leaderLease, "namespace": "cnpg-system", "uid": "lease-uid", "resourceVersion": "10"}, "spec": object{"holderIdentity": "cloudnative-pg-fixture_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "leaseDurationSeconds": float64(15), "leaseTransitions": float64(1), "renewTime": testNow.Add(-time.Second).Format(time.RFC3339Nano)}}
	pod := object{"metadata": object{"name": "cloudnative-pg-fixture", "namespace": "cnpg-system", "uid": "leader-uid", "resourceVersion": "10", "labels": object{"app.kubernetes.io/instance": "cloudnative-pg", "app.kubernetes.io/name": "cloudnative-pg"}}, "spec": object{"containers": []any{object{"name": "manager", "image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"}}}, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}}
	return lease, pod
}

// pauseRecord models the operator's fresh cluster-specific pause acknowledgment.
func pauseRecord() object {
	return object{"level": "warning", "ts": testNow.Format(time.RFC3339Nano), "msg": pauseMessage, "namespace": namespace, "name": clusterName, "Cluster": object{"namespace": namespace, "name": clusterName}, "reconcileID": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}
}

// TestCompletedJoinPlanRejectsUnsafeHold preserves every recovery admission gate.
func TestCompletedJoinPlanRejectsUnsafeHold(t *testing.T) {
	for _, failure := range []string{"", "claim UID", "volume UID", "job UID", "job active", "job owner", "pod running", "wrong container", "unready peer", "nonrunning peer", "stale backup", "active backup", "fence", "pause", "different image", "missing CSI", "empty handle", "attached claim", "foreign claim", "unknown phase"} {
		t.Run(failure, func(t *testing.T) {
			s, g := completedFixture()
			switch failure {
			case "claim UID":
				g.claimUID = "other"
			case "volume UID":
				g.volumeUID = "other"
			case "job UID":
				g.jobUID = "other"
			case "job active":
				at(s.jobs[0], "status")["active"] = float64(1)
			case "job owner":
				list(s.jobs[0], "metadata", "ownerReferences")[0]["uid"] = "other"
			case "pod running":
				at(s.pods[0], "status")["phase"] = "Running"
			case "wrong container":
				list(s.pods[0], "status", "containerStatuses")[0]["name"] = "other"
			case "unready peer":
				list(s.pods[2], "status", "conditions")[0]["status"] = "False"
			case "nonrunning peer":
				at(s.pods[2], "status")["phase"] = "Succeeded"
			case "stale backup":
				at(s.backups[0], "status")["stoppedAt"] = testNow.Add(-25 * time.Hour).Format(time.RFC3339)
			case "active backup":
				at(s.backups[0], "status")["phase"] = "running"
			case "fence":
				at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
			case "pause":
				at(s.cluster, "metadata", "annotations")[pauseKey] = "disabled"
			case "different image":
				list(s.operator, "spec", "template", "spec", "containers")[0]["image"] = "unknown"
			case "missing CSI":
				delete(at(s.volumes["old-pv"], "spec"), "csi")
			case "empty handle":
				at(s.volumes["old-pv"], "spec", "csi")["volumeHandle"] = ""
			case "attached claim":
				at(s.claims[0], "metadata")["ownerReferences"] = value(s.pods[1], "metadata", "ownerReferences")
			case "foreign claim":
				at(s.claims[0], "metadata", "labels")["cnpg.io/cluster"] = "other"
			case "unknown phase":
				at(s.claims[0], "status")["phase"] = "Unknown"
			}
			_, err := completedJoinPlan(s, testOptions(), g)
			if (err == nil) != (failure == "") {
				t.Fatalf("failure=%q accepted=%v: %v", failure, err == nil, err)
			}
		})
	}
}

// TestPauseAcknowledgmentIsFreshAndBound rejects stale or unrelated controller logs.
func TestPauseAcknowledgmentIsFreshAndBound(t *testing.T) {
	for _, failure := range []string{"", "stale", "future", "wrong namespace", "wrong cluster", "wrong context", "wrong message", "missing reconcile", "wrong level", "malformed reconcile", "truncated", "mixed malformed"} {
		t.Run(failure, func(t *testing.T) {
			r := pauseRecord()
			switch failure {
			case "stale":
				r["ts"] = testNow.Add(-time.Minute).Format(time.RFC3339)
			case "future":
				r["ts"] = testNow.Add(time.Minute).Format(time.RFC3339)
			case "wrong namespace":
				r["namespace"] = "other"
			case "wrong cluster":
				r["name"] = "other"
			case "wrong context":
				at(r, "Cluster")["name"] = "other"
			case "wrong message":
				r["msg"] = "pause requested"
			case "missing reconcile":
				delete(r, "reconcileID")
			case "wrong level":
				r["level"] = "info"
			case "malformed reconcile":
				r["reconcileID"] = "unknown"
			}
			b, _ := json.Marshal(r)
			if failure == "truncated" {
				b = b[:len(b)-1]
			}
			if failure == "mixed malformed" {
				b = append(b, []byte("\nnot JSON")...)
			}
			got, _ := pauseAcknowledged(b, testNow.Add(-time.Second), testNow, nil)
			if got != (failure == "") {
				t.Fatalf("incorrect acknowledgment for %q", failure)
			}
		})
	}
}

// TestOperatorLeaderBindingIsComplete rejects incomplete or expired leader identity.
func TestOperatorLeaderBindingIsComplete(t *testing.T) {
	for _, failure := range []string{"", "wrong lease", "missing transitions", "expired", "wrong pod", "wrong image", "unready"} {
		t.Run(failure, func(t *testing.T) {
			lease, pod := leaderFixture()
			switch failure {
			case "wrong lease":
				at(lease, "metadata")["name"] = "other"
			case "missing transitions":
				delete(at(lease, "spec"), "leaseTransitions")
			case "expired":
				at(lease, "spec")["renewTime"] = testNow.Add(-time.Minute).Format(time.RFC3339)
			case "wrong pod":
				at(pod, "metadata")["name"] = "other"
			case "wrong image":
				list(pod, "spec", "containers")[0]["image"] = "unknown"
			case "unready":
				list(pod, "status", "conditions")[0]["status"] = "False"
			}
			c := client{now: func() time.Time { return testNow }, command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[1] == "lease" {
					return json.Marshal(lease)
				}
				return json.Marshal(pod)
			}}
			_, err := c.leader(context.Background())
			if (err == nil) != (failure == "") {
				t.Fatalf("failure=%q returned %v", failure, err)
			}
		})
	}
}

// TestOperatorLeaderAllowsOnlyBoundedClockSkew keeps expired leases invalid.
func TestOperatorLeaderAllowsOnlyBoundedClockSkew(t *testing.T) {
	for _, offset := range []time.Duration{time.Millisecond, 2 * time.Second, 2*time.Second + time.Nanosecond, -15*time.Second - time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			lease, pod := leaderFixture()
			at(lease, "spec")["renewTime"] = testNow.Add(offset).Format(time.RFC3339Nano)
			c := client{now: func() time.Time { return testNow }, command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[1] == "lease" {
					return json.Marshal(lease)
				}
				return json.Marshal(pod)
			}}
			_, err := c.leader(context.Background())
			want := offset > 0 && offset <= 2*time.Second
			if (err == nil) != want {
				t.Fatalf("renewal offset %s accepted=%v, want=%v: %v", offset, err == nil, want, err)
			}
		})
	}
}

// TestPauseAcknowledgmentAllowsOnlyBoundedClockSkew checks both clock directions.
func TestPauseAcknowledgmentAllowsOnlyBoundedClockSkew(t *testing.T) {
	for _, offset := range []time.Duration{-2*time.Second - time.Nanosecond, -2 * time.Second, -time.Millisecond, time.Millisecond, 2 * time.Second, 2*time.Second + time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			r := pauseRecord()
			r["ts"] = testNow.Add(offset).Format(time.RFC3339Nano)
			b, _ := json.Marshal(r)
			want := offset >= -2*time.Second && offset <= 2*time.Second
			got, err := pauseAcknowledged(b, testNow, testNow, nil)
			if err != nil || got != want {
				t.Fatalf("acknowledgment offset %s accepted=%v, want=%v", offset, got, want)
			}
		})
	}
}

// TestCompletedJoinBlocksEveryActiveOrUnknownBackup rejects nonterminal phases, even
// when another older backup can supply the recent-success evidence.
func TestCompletedJoinBlocksEveryActiveOrUnknownBackup(t *testing.T) {
	for _, phase := range []string{"pending", "started", "running", "finalizing", "unknown", ""} {
		t.Run(phase, func(t *testing.T) {
			s, g := completedFixture()
			other := fixture().backups[0]
			at(other, "metadata")["name"] = "another-backup"
			at(other, "status")["phase"] = phase
			s.backups = append(s.backups, other)
			if _, err := completedJoinPlan(s, testOptions(), g); err == nil {
				t.Fatalf("backup phase %q was accepted", phase)
			}
		})
	}
}

// TestCompletedJoinSnapshotIncludesUnlabelledBackups checks the actual cluster join
// rather than assuming that every backup has an optional cluster label.
func TestCompletedJoinSnapshotIncludesUnlabelledBackups(t *testing.T) {
	s, _ := completedFixture()
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[1] == "backups.postgresql.cnpg.io" && strings.Contains(strings.Join(args, " "), " -l ") {
			t.Fatal("namespace backup census still depends on an optional label")
		}
		return fakeRead(s, args[1:])
	}}
	if _, err := c.snapshotPending(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

// TestCompletedJoinBackupCensusDoesNotDropUnknownEntries rejects malformed coverage
// while admitting positively identified backups belonging to another cluster.
func TestCompletedJoinBackupCensusDoesNotDropUnknownEntries(t *testing.T) {
	for _, failure := range []string{"foreign cluster", "missing cluster", "missing metadata", "malformed item"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := completedFixture()
			other := fixture().backups[0]
			at(other, "metadata")["name"] = "other-backup"
			switch failure {
			case "foreign cluster":
				at(other, "spec", "cluster")["name"] = "other-db"
			case "missing cluster":
				delete(at(other, "spec"), "cluster")
			case "missing metadata":
				delete(other, "metadata")
			}
			c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[1] == "backups.postgresql.cnpg.io" {
					var second any = other
					if failure == "malformed item" {
						second = "truncated"
					}
					return json.Marshal(object{"items": []any{s.backups[0], second}})
				}
				return fakeRead(s, args[1:])
			}}
			got, err := c.snapshotPending(context.Background(), true)
			if failure == "foreign cluster" {
				if err != nil || len(got.backups) != 1 {
					t.Fatalf("unrelated well-formed cluster was not excluded: %v", err)
				}
			} else if err == nil {
				t.Fatalf("incomplete backup coverage %q was accepted", failure)
			}
		})
	}
}

// TestCompletedJoinTransaction exercises real reads and mutations with a stateful
// API stand-in, including the operator's ordinal reuse and pending fresh PVC.
func TestCompletedJoinTransaction(t *testing.T) {
	for _, failure := range []string{"", "read only", "runner ahead", "runner behind", "baseline read", "malformed baseline", "malformed acknowledgment", "replayed acknowledgment", "source", "namespace read", "malformed volumes", "unlabelled consumer", "pause CAS", "no acknowledgment", "leader changed", "job CAS", "job removal read", "PVC CAS", "PV loss", "pause owner", "old CSI reused", "unselected old CSI", "pending claim", "backing read", "backing UID drift", "backing attached", "backing terminating"} {
		t.Run(failure, func(t *testing.T) {
			s, g := completedFixture()
			lease, leaderPod := leaderFixture()
			now := testNow
			clockOffset := time.Duration(0)
			switch failure {
			case "runner ahead":
				clockOffset = -time.Millisecond
			case "runner behind":
				clockOffset = time.Millisecond
			}
			if clockOffset != 0 {
				at(lease, "spec")["renewTime"] = now.Add(clockOffset).Format(time.RFC3339Nano)
			}
			writes, waits := []string{}, 0
			paused, acknowledged := false, false
			pending := false
			fresh := func() {
				claim := fixture().claims[0]
				at(claim, "metadata")["uid"] = "fresh-claim"
				at(claim, "spec")["volumeName"] = "fresh-pv"
				pv := fixture().volumes["old-pv"]
				at(pv, "metadata")["name"], at(pv, "metadata")["uid"] = "fresh-pv", "fresh-volume"
				at(pv, "spec", "claimRef")["uid"] = "fresh-claim"
				at(pv, "spec")["csi"] = object{"driver": "driver.longhorn.io", "volumeHandle": "fresh-volume"}
				if failure == "old CSI reused" || failure == "unselected old CSI" {
					at(pv, "spec", "csi")["volumeHandle"] = "original-volume"
				}
				s.claims, s.volumes["fresh-pv"] = []object{claim}, pv
				pod := fixture().pods[0]
				at(pod, "metadata")["uid"] = "fresh-pod"
				at(pod, "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
				s.pods = append(s.pods, pod)
				at(s.cluster, "status")["readyInstances"] = float64(3)
				appendCondition(s.cluster, object{"type": "Ready", "status": "True"})
			}
			c := client{now: func() time.Time { return now }, source: func(context.Context) error {
				if failure == "source" {
					return errors.New("source changed")
				}
				return nil
			}, wait: func(context.Context) error {
				waits++
				now = now.Add(20 * time.Second)
				at(lease, "spec")["renewTime"] = now.Add(-time.Second).Format(time.RFC3339Nano)
				if pending {
					pending = false
					fresh()
				}
				if failure == "no acknowledgment" || waits > 5 {
					return errors.New("deadline")
				}
				return nil
			}}
			c.command = func(_ context.Context, args []string, body []byte) ([]byte, error) {
				if args[0] == "get" {
					if args[1] == "volumes.longhorn.io" {
						if failure == "backing read" {
							return nil, errors.New("unreadable backing volume")
						}
						v := object{"metadata": object{"name": "original-volume", "namespace": "longhorn-system", "uid": "backing-uid", "resourceVersion": "10"}, "status": object{"state": "detached"}}
						if len(writes) == 2 {
							switch failure {
							case "backing UID drift":
								at(v, "metadata")["uid"] = "different-backing"
							case "backing attached":
								at(v, "status")["state"] = "attached"
							case "backing terminating":
								at(v, "metadata")["deletionTimestamp"] = now.Format(time.RFC3339)
							}
						}
						return json.Marshal(v)
					}
					if args[1] == "lease" {
						return json.Marshal(lease)
					}
					if args[1] == "pod" && args[2] == id(leaderPod).name {
						return json.Marshal(leaderPod)
					}
					if args[1] == "pvc" && args[2] == targetName && len(s.claims) == 1 {
						return json.Marshal(s.claims[0])
					}
					if args[1] == "pvc" && args[2] == "-n" && failure == "unselected old CSI" && len(writes) == 4 {
						return json.Marshal(object{"items": []object{}})
					}
					if args[1] == "pods" && !strings.Contains(strings.Join(args, " "), " -l ") {
						if failure == "namespace read" || (failure == "job removal read" && len(writes) == 2) {
							return nil, errors.New("unreadable consumers")
						}
						pods := append([]object{}, s.pods...)
						if failure == "unlabelled consumer" {
							pods = append(pods, object{"metadata": object{"name": "unknown", "namespace": namespace, "uid": "other", "resourceVersion": "10"}, "spec": object{"volumes": []any{object{"persistentVolumeClaim": object{"claimName": targetName}}}}})
						}
						if failure == "malformed volumes" {
							pods = append(pods, object{"metadata": object{"name": "unknown", "namespace": namespace, "uid": "other", "resourceVersion": "10"}, "spec": object{"volumes": []any{"truncated"}}})
						}
						return json.Marshal(object{"items": pods})
					}
					return fakeRead(s, args[1:])
				}
				if args[0] == "logs" {
					if args[1] != id(leaderPod).name || !strings.Contains(strings.Join(args, " "), "--since-time="+testNow.Add(-2*time.Second).Format(time.RFC3339Nano)) || !strings.Contains(strings.Join(args, " "), "--limit-bytes=1048577") {
						t.Fatal("acknowledgment escaped the bound leader or bounded skew window")
					}
					r := pauseRecord()
					r["ts"] = now.Add(clockOffset).Format(time.RFC3339Nano)
					if !paused {
						if failure == "baseline read" {
							return nil, errors.New("unreadable pre-pause logs")
						}
						if failure == "malformed baseline" {
							return []byte("truncated"), nil
						}
						if failure != "replayed acknowledgment" {
							r["msg"] = "ordinary reconciliation"
							r["reconcileID"] = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
						}
						return json.Marshal(r)
					}
					if failure == "no acknowledgment" {
						r["msg"] = "other"
					}
					if failure == "malformed acknowledgment" {
						return []byte("truncated"), nil
					}
					if failure == "leader changed" {
						at(lease, "spec")["leaseTransitions"] = float64(2)
					}
					acknowledged = failure != "no acknowledgment"
					return json.Marshal(r)
				}
				if args[0] == "patch" {
					if args[1] != "cluster.postgresql.cnpg.io" || args[2] != clusterName {
						t.Fatal("mutation escaped exact cluster")
					}
					var ops []object
					if err := json.Unmarshal(body, &ops); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(ops[:2], tests(s.cluster)) {
						t.Fatal("missing observed UID/version preconditions")
					}
					last := ops[len(ops)-1]
					if str(last, "op") == "add" {
						writes = append(writes, "pause")
						if failure == "pause CAS" {
							return nil, errors.New("conflict")
						}
						paused = true
						at(s.cluster, "metadata", "annotations")[pauseKey] = "disabled"
						manager := fieldManager
						if failure == "pause owner" {
							manager = "other"
						}
						at(s.cluster, "metadata")["managedFields"] = []any{object{"manager": manager, "fieldsV1": object{"f:metadata": object{"f:annotations": object{"f:" + pauseKey: object{}}}}}}
					} else if str(last, "op") == "remove" && str(last, "path") == "/metadata/annotations/cnpg.io~1reconciliationLoop" {
						if !paused || !released(s.volumes["old-pv"], identity{targetName, "claim-uid"}, identity{"old-pv", "pv-uid"}) || len(s.claims) != 0 || len(s.jobs) != 0 {
							t.Fatal("resume preceded old-volume quarantine")
						}
						writes = append(writes, "resume")
						paused = false
						delete(at(s.cluster, "metadata", "annotations"), pauseKey)
						if failure == "pending claim" {
							pending = true
							s.claims = []object{object{"metadata": object{"name": targetName, "namespace": namespace, "uid": "fresh-claim", "resourceVersion": "11"}, "spec": object{}, "status": object{"phase": "Pending"}}}
						} else {
							fresh()
						}
					} else {
						t.Fatalf("unexpected patch: %v", ops)
					}
					return []byte(`{}`), nil
				}
				if args[0] != "delete" || !paused || !acknowledged {
					t.Fatalf("unacknowledged or unexpected mutation: %v", args)
				}
				var deletion object
				if err := json.Unmarshal(body, &deletion); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(args[2], "/jobs/") {
					writes = append(writes, "job")
					if str(deletion, "preconditions", "uid") != g.jobUID || str(deletion, "preconditions", "resourceVersion") != "10" || str(deletion, "propagationPolicy") != "Foreground" {
						t.Fatal("job deletion not identity-bound")
					}
					if failure == "job CAS" {
						return nil, errors.New("conflict")
					}
					s.jobs, s.pods = []object{}, s.pods[1:]
				} else if args[2] == "/api/v1/namespaces/"+namespace+"/persistentvolumeclaims/"+targetName {
					writes = append(writes, "claim")
					if str(deletion, "preconditions", "uid") != g.claimUID || str(deletion, "preconditions", "resourceVersion") != "10" || len(s.jobs) != 0 || len(s.pods) != 2 {
						t.Fatal("claim delete preceded completed-job collection")
					}
					if failure == "PVC CAS" {
						return nil, errors.New("conflict")
					}
					s.claims = []object{}
					at(s.volumes["old-pv"], "status")["phase"] = "Released"
					if failure == "PV loss" {
						at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Delete"
					}
				} else {
					t.Fatalf("unexpected deletion: %v", args)
				}
				return []byte(`{}`), nil
			}
			ok := failure == "" || failure == "read only" || failure == "pending claim" || failure == "runner ahead" || failure == "runner behind"
			err := quarantineCompletedJoin(context.Background(), c, testOptions(), g, failure != "read only")
			if (err == nil) != ok {
				t.Fatalf("failure=%q writes=%v waits=%d returned %v", failure, writes, waits, err)
			}
			if failure == "read only" && len(writes) != 0 {
				t.Fatal("planning performed mutations")
			}
			if (failure == "job removal read" || failure == "malformed acknowledgment") && waits != 0 {
				t.Fatal("unknown observation was retried rather than stopping immediately")
			}
			if failure == "" || failure == "pending claim" || failure == "runner ahead" || failure == "runner behind" {
				if !reflect.DeepEqual(writes, []string{"pause", "job", "claim", "resume"}) || waits < 1 {
					t.Fatalf("unexpected transaction %v / separated waits=%d", writes, waits)
				}
			}
			maximum := map[string]int{"baseline read": 0, "malformed baseline": 0, "malformed acknowledgment": 1, "replayed acknowledgment": 1, "source": 0, "namespace read": 0, "malformed volumes": 0, "unlabelled consumer": 0, "pause CAS": 1, "no acknowledgment": 1, "leader changed": 1, "pause owner": 1, "job CAS": 2, "job removal read": 2, "PVC CAS": 3, "PV loss": 3, "old CSI reused": 4, "unselected old CSI": 4, "backing read": 0, "backing UID drift": 2, "backing attached": 2, "backing terminating": 2}
			if n, exists := maximum[failure]; exists && len(writes) != n {
				t.Fatalf("failure %q escaped stop boundary: %v", failure, writes)
			}
		})
	}
}

// TestPauseLogCoverageIsBounded refuses partial baselines before pausing.
func TestPauseLogCoverageIsBounded(t *testing.T) {
	for _, malformed := range []string{"null", `{"ts":`, strings.Repeat(" ", (1<<20)+1), "{}\nnot JSON"} {
		if _, err := pauseLogRecords([]byte(malformed)); err == nil {
			t.Fatal("incomplete baseline log coverage was accepted")
		}
	}
	for _, complete := range []string{"", " \n", `{"msg":"unrelated startup log"}`} {
		if _, err := pauseLogRecords([]byte(complete)); err != nil {
			t.Fatalf("complete baseline was refused: %v", err)
		}
	}
}

// TestCompletedJoinCLIRequiresSeparateApproval binds execution to the new grant,
// not the consumed legacy grant.
func TestCompletedJoinCLIRequiresSeparateApproval(t *testing.T) {
	good := map[string]string{"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_SHA": strings.Repeat("a", 40), "WEDDING_REPAIR_CONFIRM": "retain-volume-rebuild-completed-standby"}
	for _, failure := range []string{"workflow", "confirm", "branch", "attempt", "SHA", "missing UID", "resume", "proof"} {
		t.Run(failure, func(t *testing.T) {
			for k, v := range good {
				t.Setenv(k, v)
			}
			t.Setenv("WEDDING_REPAIR_RESUME_FENCED", "false")
			args := []string{"repair", "--execute", "--quarantine-completed-join", "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "--job-uid", "cccccccc-cccc-cccc-cccc-cccccccccccc", "--claim-uid", "dddddddd-dddd-dddd-dddd-dddddddddddd", "--volume-uid", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}
			switch failure {
			case "workflow":
				t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/replace-wedding-standby.yaml@refs/heads/main")
			case "confirm":
				t.Setenv("WEDDING_REPAIR_CONFIRM", "retain-volumes-replace-failed-standby")
			case "branch":
				t.Setenv("GITHUB_REF", "refs/heads/other")
			case "attempt":
				t.Setenv("GITHUB_RUN_ATTEMPT", "2")
			case "SHA":
				t.Setenv("GITHUB_SHA", strings.Repeat("z", 40))
			case "missing UID":
				args = args[:len(args)-2]
			case "resume":
				args = append(args, "--resume-fenced")
			case "proof":
				args = append(args, "--prove-fenced")
			}
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = args
			if err := run(); err == nil || (!strings.Contains(err.Error(), "separately confirmed") && !strings.Contains(err.Error(), "UIDs are required") && !strings.Contains(err.Error(), "cannot share")) {
				t.Fatalf("separate recovery boundary returned %v", err)
			}
		})
	}
}
