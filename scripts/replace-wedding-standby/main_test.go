package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFullyQualifiedDatabaseResources(t *testing.T) {
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[1] == "cluster" || args[1] == "backups" {
			t.Fatal("ambiguous CR kind could select a different API group")
		}
		return fakeRead(fixture(), args[1:])
	}}
	if err := repair(context.Background(), c, testOptions(), false); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateRecoveryObservationIsNotClean(t *testing.T) {
	s := fixture()
	p := plan{primary: id(s.pods[1]), healthy: id(s.pods[2]), target: id(s.pods[0])}
	s.pods = []object{s.pods[1], s.pods[2], s.pods[2]}
	at(s.cluster, "status")["readyInstances"] = float64(3)
	at(s.cluster, "status")["conditions"] = append(at(s.cluster, "status")["conditions"].([]any), object{"type": "Ready", "status": "True"})
	if ok, err := (client{}).recovered(context.Background(), s, testOptions(), p, id(s.claims[0])); ok || err == nil {
		t.Fatal("duplicate observations cleared recovery without a replacement")
	}
}

func TestDispatchBoundary(t *testing.T) {
	good := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "WEDDING_REPAIR_CONFIRM": "retain-volumes-replace-failed-standby", "GITHUB_SHA": strings.Repeat("a", 40)}
	for key := range good {
		t.Run(key, func(t *testing.T) {
			for k, v := range good {
				t.Setenv(k, v)
			}
			t.Setenv(key, "wrong")
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = []string{"repair", "--execute", "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}
			err := run()
			if err == nil || !strings.Contains(err.Error(), "execution requires") {
				t.Fatalf("protected boundary returned %v", err)
			}
		})
	}
}

func TestFenceMetricMustBeCompleteAndTrue(t *testing.T) {
	for _, tc := range []struct {
		body       string
		failed, ok bool
	}{{"cnpg_collector_fencing_on 1\n", false, true}, {"cnpg_collector_fencing_on 1\n", true, false}, {"cnpg_collector_fencing_on 0\n", false, false}, {"cnpg_collector_fencing_on 1\ncnpg_collector_fencing_on 1\n", false, false}, {"", false, false}} {
		c := client{command: func(context.Context, []string, []byte) ([]byte, error) {
			if tc.failed {
				return []byte(tc.body), errors.New("partial response")
			}
			return []byte(tc.body), nil
		}}
		if got := c.fenced(context.Background()); got != tc.ok {
			t.Fatalf("fencing %q=%v", tc.body, got)
		}
	}
}

func fakeRead(s inventory, args []string) ([]byte, error) {
	kind, name := strings.TrimSuffix(args[0], ".postgresql.cnpg.io"), ""
	if len(args) > 1 {
		name = args[1]
	}
	var result any
	switch kind {
	case "cluster":
		result = s.cluster
	case "deployment":
		result = s.operator
	case "pods":
		result = object{"items": s.pods}
	case "pvc":
		if name == "wedding-db-4" {
			claim := fixture().claims[0]
			at(claim, "metadata")["name"] = "wedding-db-4"
			at(claim, "metadata")["uid"] = "new-claim-uid"
			at(claim, "spec")["volumeName"] = "new-pv"
			result = claim
		} else {
			result = object{"items": s.claims}
		}
	case "pv":
		if name == "new-pv" {
			pv := fixture().volumes["old-pv"]
			at(pv, "metadata")["name"] = "new-pv"
			at(pv, "metadata")["uid"] = "new-pv-uid"
			at(pv, "spec", "claimRef")["name"] = "wedding-db-4"
			at(pv, "spec", "claimRef")["uid"] = "new-claim-uid"
			result = pv
		} else {
			result = s.volumes[name]
		}
	case "backups":
		result = object{"items": s.backups}
	case "jobs":
		jobs := s.jobs
		if jobs == nil {
			jobs = []object{}
		}
		result = object{"items": jobs}
	default:
		return nil, errors.New("unexpected read")
	}
	return json.Marshal(result)
}

func TestReadOnlyPlanAndPartialReadFailure(t *testing.T) {
	for _, fail := range []string{"", "cluster", "deployment", "pods", "pvc", "pv", "backups", "jobs"} {
		t.Run("partial "+fail, func(t *testing.T) {
			c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] != "get" {
					t.Fatal("plan issued a write")
				}
				data, err := fakeRead(fixture(), args[1:])
				if len(args) > 1 && strings.TrimSuffix(args[1], ".postgresql.cnpg.io") == fail {
					return data, errors.New("plausible partial response followed by failure")
				}
				return data, err
			}}
			err := repair(context.Background(), c, testOptions(), false)
			if (err == nil) != (fail == "") {
				t.Fatalf("error=%v on failed read %q", err, fail)
			}
		})
	}
}

func TestExecutionRefusesFailedSourceBeforeWrites(t *testing.T) {
	writes := 0
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] != "get" {
			writes++
			return nil, nil
		}
		return fakeRead(fixture(), args[1:])
	}, source: func(context.Context) error { return errors.New("source stale") }}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("stale source accepted")
	}
	if writes != 0 {
		t.Fatal("write before source proof")
	}
}

func TestRepairStopsOnIntermediateDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		after  int
		change func(inventory)
	}{
		{"primary changed before fence", 1, func(s inventory) { at(s.cluster, "status")["currentPrimary"] = "wedding-db-3" }},
		{"healthy peer replaced before detach", 2, func(s inventory) { at(s.pods[2], "metadata")["uid"] = "changed" }},
		{"volume binding changed before detach", 2, func(s inventory) { at(s.volumes["old-pv"], "spec", "claimRef")["uid"] = "changed" }},
		{"target ready before detach", 2, func(s inventory) {
			at(s.pods[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
		}},
		{"pod replaced before delete", 3, func(s inventory) { at(s.pods[0], "metadata")["uid"] = "changed" }},
		{"primary changing before delete", 3, func(s inventory) { at(s.cluster, "status")["targetPrimary"] = "wedding-db-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture()
			writes := 0
			c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return errors.New("unexpected wait") }}
			c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" {
					if args[1] == "--raw" {
						return []byte("cnpg_collector_fencing_on 1\n"), nil
					}
					return fakeRead(s, args[1:])
				}
				writes++
				if args[0] != "patch" {
					t.Fatal("unsafe pod deletion after drift")
				}
				switch args[1] {
				case "pv":
					at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
				case "cluster.postgresql.cnpg.io":
					at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
				case "pvc":
					at(s.claims[0], "metadata")["ownerReferences"] = []any{}
					at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
				default:
					t.Fatal("unexpected mutation")
				}
				if writes == tc.after {
					tc.change(s)
				}
				return []byte(`{}`), nil
			}
			if err := repair(context.Background(), c, testOptions(), true); err == nil {
				t.Fatal("drift cleared repair")
			}
			if writes != tc.after {
				t.Fatalf("made %d writes, expected stop at %d", writes, tc.after)
			}
		})
	}
}

func TestFailedConditionalWriteDoesNotRetry(t *testing.T) {
	writes := 0
	c := client{source: func(context.Context) error { return nil }}
	c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "get" {
			return fakeRead(fixture(), args[1:])
		}
		writes++
		return []byte(`{"status":"Success"}`), errors.New("CAS rejected after output")
	}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("failed write treated as clean")
	}
	if writes != 1 {
		t.Fatal("write retried or cleanup attempted")
	}
}

func TestUnacknowledgedFenceCannotDetachStorage(t *testing.T) {
	s := fixture()
	writes := 0
	c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return context.DeadlineExceeded }}
	c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "get" {
			if args[1] == "--raw" {
				return []byte("cnpg_collector_fencing_on 0\n"), nil
			}
			return fakeRead(s, args[1:])
		}
		writes++
		switch args[1] {
		case "pv":
			at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
		case "cluster.postgresql.cnpg.io":
			at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
		default:
			t.Fatal("mutated storage without acknowledged fencing")
		}
		return []byte(`{}`), nil
	}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("unfenced instance cleared repair")
	}
	if writes != 2 || !owned(s.claims[0], "cluster-uid") {
		t.Fatal("claim was changed without fencing")
	}
}

func TestWriteSequencePreservesDataAndPinsDeletes(t *testing.T) {
	s := fixture()
	var writes []string
	c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return nil }}
	c.command = func(_ context.Context, args []string, data []byte) ([]byte, error) {
		if args[0] == "get" {
			if args[1] == "--raw" {
				return []byte("# gauge\ncnpg_collector_fencing_on 1\n"), nil
			}
			return fakeRead(s, args[1:])
		}
		writes = append(writes, strings.Join(args[:2], " "))
		if args[0] == "delete" {
			if args[1] != "--raw" || !strings.Contains(strings.Join(args, " "), "/pods/wedding-db-1") {
				t.Fatal("deletion escaped the single pod")
			}
			var body object
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if str(body, "preconditions", "uid") != "target-uid" || str(body, "preconditions", "resourceVersion") != "10" {
				t.Fatal("delete not bound to exact pod")
			}
			s.pods = s.pods[1:]
			newPod := fixture().pods[2]
			at(newPod, "metadata")["name"] = "wedding-db-4"
			at(newPod, "metadata")["uid"] = "new-uid"
			at(newPod, "metadata", "labels")["cnpg.io/instanceName"] = "wedding-db-4"
			at(newPod, "spec")["volumes"] = []any{object{"persistentVolumeClaim": object{"claimName": "wedding-db-4"}}}
			s.pods = append(s.pods, newPod)
			at(s.cluster, "status")["readyInstances"] = float64(3)
			at(s.cluster, "status")["conditions"] = append(at(s.cluster, "status")["conditions"].([]any), object{"type": "Ready", "status": "True"})
			return []byte(`{}`), nil
		}
		if args[0] != "patch" {
			t.Fatalf("unexpected write %v", args)
		}
		var ops []object
		if err := json.Unmarshal(data, &ops); err != nil {
			t.Fatal(err)
		}
		if len(ops) < 3 || str(ops[0], "op") != "test" || str(ops[0], "path") != "/metadata/uid" || str(ops[1], "op") != "test" || str(ops[1], "path") != "/metadata/resourceVersion" {
			t.Fatal("patch omitted identity preconditions")
		}
		switch strings.TrimSuffix(args[1], ".postgresql.cnpg.io") {
		case "pv":
			at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
		case "cluster":
			last := ops[len(ops)-1]
			if str(last, "op") == "remove" {
				delete(at(s.cluster, "metadata", "annotations"), fenceKey)
			} else {
				at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
			}
		case "pvc":
			at(s.claims[0], "metadata")["ownerReferences"] = []any{}
			at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
		default:
			t.Fatal("write outside authorized resources")
		}
		return []byte(`{}`), nil
	}
	if err := repair(context.Background(), c, testOptions(), true); err != nil {
		t.Fatal(err)
	}
	want := "patch pv,patch cluster.postgresql.cnpg.io,patch pvc,delete --raw,patch cluster.postgresql.cnpg.io"
	if strings.Join(writes, ",") != want {
		t.Fatalf("write ordering %v; want %s", writes, want)
	}
	if id(s.claims[0]).uid != "claim-uid" || str(s.volumes["old-pv"], "spec", "claimRef", "uid") != "claim-uid" {
		t.Fatal("old data binding lost")
	}
}

var testNow = time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)

func fixture() inventory {
	owner := []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "wedding-db", "uid": "cluster-uid", "controller": true}}
	meta := func(name, uid string) object {
		return object{"name": name, "namespace": "wedding-app", "uid": uid, "resourceVersion": "10", "ownerReferences": owner}
	}
	pod := func(name, uid, role, ready string) object {
		m := meta(name, uid)
		m["labels"] = object{"cnpg.io/cluster": "wedding-db", "cnpg.io/instanceName": name, "cnpg.io/instanceRole": role, "cnpg.io/podRole": "instance"}
		status := object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": ready}}, "containerStatuses": []any{object{"name": "postgres", "ready": ready == "True", "state": object{"waiting": object{"reason": "CrashLoopBackOff"}}}}}
		return object{"metadata": m, "status": status, "spec": object{"volumes": []any{object{"persistentVolumeClaim": object{"claimName": name}}}}}
	}
	c := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": object{"name": "wedding-db", "namespace": "wedding-app", "uid": "cluster-uid", "resourceVersion": "10", "annotations": object{}}, "spec": object{"instances": float64(3)}, "status": object{"readyInstances": float64(2), "currentPrimary": "wedding-db-2", "targetPrimary": "wedding-db-2", "conditions": []any{object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}}}
	claim := object{"metadata": meta("wedding-db-1", "claim-uid"), "spec": object{"volumeName": "old-pv"}, "status": object{"phase": "Bound"}}
	claim["metadata"].(object)["labels"] = object{"cnpg.io/cluster": "wedding-db", "cnpg.io/instanceName": "wedding-db-1"}
	claim["metadata"].(object)["annotations"] = object{"cnpg.io/pvcStatus": "ready"}
	volume := object{"metadata": object{"name": "old-pv", "uid": "pv-uid", "resourceVersion": "10"}, "spec": object{"claimRef": object{"name": "wedding-db-1", "namespace": "wedding-app", "uid": "claim-uid"}, "persistentVolumeReclaimPolicy": "Delete"}, "status": object{"phase": "Bound"}}
	backup := object{"metadata": meta("daily-backup", "backup-uid"), "spec": object{"cluster": object{"name": "wedding-db"}, "method": "plugin", "pluginConfiguration": object{"name": "barman-cloud.cloudnative-pg.io"}}, "status": object{"phase": "completed", "stoppedAt": testNow.Add(-time.Hour).Format(time.RFC3339), "pluginMetadata": object{"clusterUID": "cluster-uid"}}}
	operator := object{"metadata": object{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "operator-uid", "resourceVersion": "10", "generation": float64(1)}, "spec": object{"replicas": float64(2), "template": object{"spec": object{"containers": []any{object{"image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"}}}}}, "status": object{"observedGeneration": float64(1), "availableReplicas": float64(2), "updatedReplicas": float64(2)}}
	return inventory{cluster: c, operator: operator, pods: []object{pod("wedding-db-1", "target-uid", "replica", "False"), pod("wedding-db-2", "primary-uid", "primary", "True"), pod("wedding-db-3", "healthy-uid", "replica", "True")}, claims: []object{claim}, volumes: map[string]object{"old-pv": volume}, backups: []object{backup}}
}

func at(o object, path ...string) object {
	for _, k := range path {
		o = o[k].(object)
	}
	return o
}
func testOptions() options {
	return options{clusterUID: "cluster-uid", podUID: "target-uid", now: testNow}
}

func TestValidateSafeRepair(t *testing.T) {
	p, err := validate(fixture(), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if p.primary.uid != "primary-uid" || p.healthy.uid != "healthy-uid" || p.target.uid != "target-uid" || len(p.claims) != 1 {
		t.Fatalf("unbound plan: %+v", p)
	}
}

func TestRefuseUnsafeRepair(t *testing.T) {
	cases := []struct {
		name   string
		change func(*inventory, *options)
	}{
		{"unaudited operator", func(s *inventory, _ *options) {
			at(s.operator, "spec", "template", "spec")["containers"] = []any{object{"image": "other:latest"}}
		}},
		{"operator rolling", func(s *inventory, _ *options) { at(s.operator, "status")["updatedReplicas"] = float64(1) }},
		{"stale cluster", func(_ *inventory, o *options) { o.clusterUID = "previous" }},
		{"stale pod", func(_ *inventory, o *options) { o.podUID = "previous" }},
		{"target primary", func(s *inventory, _ *options) { at(s.cluster, "status")["currentPrimary"] = "wedding-db-1" }},
		{"pending switchover", func(s *inventory, _ *options) { at(s.cluster, "status")["targetPrimary"] = "wedding-db-3" }},
		{"target primary label", func(s *inventory, _ *options) {
			at(s.pods[0], "metadata", "labels")["cnpg.io/instanceRole"] = "primary"
		}},
		{"target ready", func(s *inventory, _ *options) {
			at(s.pods[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
		}},
		{"unknown failure", func(s *inventory, _ *options) { at(s.pods[0], "status")["containerStatuses"] = []any{} }},
		{"unhealthy peer", func(s *inventory, _ *options) {
			at(s.pods[2], "status")["conditions"] = []any{object{"type": "Ready", "status": "False"}}
		}},
		{"missing primary", func(s *inventory, _ *options) { s.pods = s.pods[:1] }},
		{"foreign pod owner", func(s *inventory, _ *options) { at(s.pods[0], "metadata")["ownerReferences"] = []any{} }},
		{"deleting pod", func(s *inventory, _ *options) { at(s.pods[0], "metadata")["deletionTimestamp"] = "now" }},
		{"missing pod version", func(s *inventory, _ *options) { delete(at(s.pods[0], "metadata"), "resourceVersion") }},
		{"missing claim", func(s *inventory, _ *options) { s.claims = nil }},
		{"extra claim", func(s *inventory, _ *options) { s.claims = append(s.claims, s.claims[0]) }},
		{"foreign claim owner", func(s *inventory, _ *options) { at(s.claims[0], "metadata")["ownerReferences"] = []any{} }},
		{"deleting claim", func(s *inventory, _ *options) { at(s.claims[0], "metadata")["deletionTimestamp"] = "now" }},
		{"unbound claim", func(s *inventory, _ *options) { at(s.claims[0], "status")["phase"] = "Pending" }},
		{"missing volume", func(s *inventory, _ *options) { s.volumes = nil }},
		{"foreign volume binding", func(s *inventory, _ *options) { at(s.volumes["old-pv"], "spec", "claimRef")["uid"] = "foreign" }},
		{"deleting volume", func(s *inventory, _ *options) { at(s.volumes["old-pv"], "metadata")["deletionTimestamp"] = "now" }},
		{"existing fence", func(s *inventory, _ *options) {
			at(s.cluster, "metadata", "annotations")["cnpg.io/fencedInstances"] = `["other"]`
		}},
		{"active job", func(s *inventory, _ *options) { s.jobs = []object{{"status": object{"active": float64(1)}}} }},
		{"no backup", func(s *inventory, _ *options) { s.backups = nil }},
		{"stale backup", func(s *inventory, _ *options) {
			at(s.backups[0], "status")["stoppedAt"] = testNow.Add(-25 * time.Hour).Format(time.RFC3339)
		}},
		{"future backup", func(s *inventory, _ *options) {
			at(s.backups[0], "status")["stoppedAt"] = testNow.Add(time.Hour).Format(time.RFC3339)
		}},
		{"foreign backup", func(s *inventory, _ *options) { at(s.backups[0], "status", "pluginMetadata")["clusterUID"] = "foreign" }},
		{"failed backup", func(s *inventory, _ *options) { at(s.backups[0], "status")["phase"] = "failed" }},
		{"archiving unhealthy", func(s *inventory, _ *options) { at(s.cluster, "status")["conditions"] = []any{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, o := fixture(), testOptions()
			tc.change(&s, &o)
			if _, err := validate(s, o); err == nil {
				t.Fatal("unsafe repair accepted")
			}
		})
	}
}

// JSON round-tripping also exercises the numeric and list forms kubectl returns.
func TestFixtureDecodesLikeKubernetes(t *testing.T) {
	s := fixture()
	b, _ := json.Marshal(s.cluster)
	var decoded object
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	s.cluster = decoded
	if _, err := validate(s, testOptions()); err != nil {
		t.Fatal(err)
	}
}
