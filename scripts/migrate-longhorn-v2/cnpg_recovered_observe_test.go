package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// This is a composed synthetic API fixture, not captured deployment evidence.
func recoveredStorageObservationFixture(t *testing.T, scenario string) (*cnpgProofRun, map[string]int) {
	t.Helper()
	f := newRecoveredStorageFixture(t)
	p := f.p
	ns := p.record.Resources["Namespace"].Name
	namespace := object{"apiVersion": "v1", "kind": "Namespace", "metadata": object{"name": ns, "uid": p.record.Resources["Namespace"].UID, "resourceVersion": "10", "labels": object{"pod-security.kubernetes.io/enforce": "restricted", "storage.platform.devantler.tech/recovery-run": p.record.Run, "app.kubernetes.io/managed-by": "github-actions"}}}
	objects := map[string][]object{
		"pods": {f.pod}, "persistentvolumeclaims": {f.claim}, "clusters.postgresql.cnpg.io": {f.cluster},
		"persistentvolumes": {f.pv}, "volumes.longhorn.io": {f.volume}, "engines.longhorn.io": {f.engine}, "replicas.longhorn.io": f.replicas,
		"jobs": {}, "deployments": {}, "statefulsets": {}, "daemonsets": {}, "cronjobs": {}, "replicasets": {},
	}
	delegate := p.c.run
	reads := map[string]int{}
	p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			reads["source"]++
			if scenario == "source changed" && reads["source"] > 1 {
				return []byte(strings.Repeat("b", 40) + "\trefs/heads/main\n"), nil
			}
			return delegate(ctx, input, program, args...)
		}
		if program != "kubectl" || len(input) != 0 || len(args) < 5 || args[2] != "get" || strings.Contains(strings.Join(args, " "), "selector") {
			t.Fatal("storage observer attempted a write, filtered inventory or process command")
		}
		kind := args[3]
		reads[kind]++
		if kind == "configmap" {
			body, err := delegate(ctx, input, program, args...)
			if scenario == "journal changed" && reads[kind] > 1 {
				var cm object
				if json.Unmarshal(body, &cm) != nil {
					t.Fatal("invalid synthetic journal")
				}
				value(cm, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
				return json.Marshal(cm)
			}
			return body, err
		}
		if kind == "namespace" {
			if args[4] != ns {
				t.Fatal("observer selected a namespace outside its journal")
			}
			if scenario == "namespace replaced" && reads[kind] > 1 {
				value(namespace, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			}
			if scenario == "namespace status advanced" && reads[kind] > 1 {
				value(namespace, "metadata").(object)["resourceVersion"] = "21"
				namespace["status"] = object{"phase": "Active"}
			}
			return json.Marshal(namespace)
		}
		items, known := objects[kind]
		if !known {
			t.Fatalf("unexpected storage inventory %q", kind)
		}
		expectedNS := ns
		if strings.HasSuffix(kind, ".longhorn.io") {
			expectedNS = "longhorn-system"
		}
		if kind != "persistentvolumes" && !strings.Contains(strings.Join(args, " "), "--namespace="+expectedNS) {
			t.Fatal("observer changed inventory scope")
		}
		if kind == "replicas.longhorn.io" && scenario == "read failed" {
			return nil, errors.New("private-storage-read-canary")
		}
		if kind == "pods" && reads[kind] > 1 && scenario == "process changed" {
			value(f.pod, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
		}
		if reads[kind] > 1 && scenario == "status advanced" && len(items) > 0 {
			value(items[0], "metadata").(object)["resourceVersion"] = "21"
			if status, ok := value(items[0], "status").(object); ok {
				status["incidentalObservation"] = "advanced"
			}
		}
		if kind == "replicas.longhorn.io" && reads[kind] > 1 && scenario == "copy changed" {
			value(f.replicas[0], "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
		}
		if kind == "pods" && scenario == "extra pod" || kind == "jobs" && scenario == "bootstrap job remains" || kind == "deployments" && scenario == "foreign controller" {
			items = append(items, resource("foreign"))
		}
		if kind == "persistentvolumes" && scenario == "contradictory PV" {
			extra, err := cloneRecoverySpec(f.pv)
			if err != nil {
				t.Fatal(err)
			}
			value(extra, "metadata").(object)["name"] = "foreign-pv"
			value(extra, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			items = append(items, extra)
		}
		if kind == "persistentvolumes" && (scenario == "backend alias first" || scenario == "backend alias last") {
			extra, err := cloneRecoverySpec(f.pv)
			if err != nil {
				t.Fatal(err)
			}
			value(extra, "metadata").(object)["name"] = "foreign-pv"
			value(extra, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			value(extra, "spec").(object)["claimRef"] = object{"namespace": "foreign", "name": "foreign", "uid": "88888888-8888-4888-8888-888888888888"}
			if scenario == "backend alias first" {
				items = append([]object{extra}, items...)
			} else {
				items = append(items, extra)
			}
		}
		if kind == "engines.longhorn.io" && scenario == "contradictory engine" {
			extra, err := cloneRecoverySpec(f.engine)
			if err != nil {
				t.Fatal(err)
			}
			value(extra, "metadata").(object)["name"] = "foreign-engine"
			value(extra, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			value(extra, "spec").(object)["volumeName"] = "other-volume"
			items = append(items, extra)
		}
		if kind == "engines.longhorn.io" && scenario == "unrelated engine" {
			extra, err := cloneRecoverySpec(f.engine)
			if err != nil {
				t.Fatal(err)
			}
			value(extra, "metadata").(object)["name"] = "unrelated-engine"
			value(extra, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			value(extra, "spec").(object)["volumeName"] = "unrelated-volume"
			ref := value(extra, "metadata", "ownerReferences").([]any)[0].(object)
			ref["name"], ref["uid"] = "unrelated-volume", "88888888-8888-4888-8888-888888888888"
			items = append(items, extra)
		}
		if len(items) > 0 {
			switch scenario {
			case "wrong API":
				items[0]["apiVersion"] = "foreign/v1"
			case "wrong kind":
				items[0]["kind"] = "Foreign"
			case "wrong namespace":
				value(items[0], "metadata").(object)["namespace"] = "foreign"
			case "missing resource version":
				delete(value(items[0], "metadata").(object), "resourceVersion")
			}
		}
		entries := []any{}
		for _, item := range items {
			entries = append(entries, item)
		}
		body := object{"metadata": object{"resourceVersion": "20"}, "items": entries}
		if scenario == "partial list" {
			body["metadata"].(object)["continue"] = "unread-page"
		}
		if kind == "replicas.longhorn.io" && scenario == "duplicate UID" {
			value(f.replicas[1], "metadata").(object)["uid"] = text(f.replicas[0], "metadata", "uid")
		}
		return json.Marshal(body)
	}
	return p, reads
}

func TestRecoveredStorageObservationReadsCompleteInventoriesBetweenOwnerFences(t *testing.T) {
	p, reads := recoveredStorageObservationFixture(t, "")
	before, err := encodeCNPGProofRecord(p.record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.observeRecoveredStorage(t.Context())
	if err != nil || got.binding.Pod.Name != "restore-1" || len(got.binding.Replicas) != 3 || reads["pods"] != 2 || reads["replicas.longhorn.io"] != 2 || reads["configmap"] != 2 || reads["source"] != 2 {
		t.Fatalf("complete native-shaped storage graph refused: %v reads=%v", err, reads)
	}
	after, err := encodeCNPGProofRecord(p.record)
	if err != nil || before != after {
		t.Fatal("read-only storage observation advanced the recovery journal")
	}
}

func TestRecoveredStorageObservationRefusesPartialChangedAndContradictoryGraphs(t *testing.T) {
	for _, scenario := range []string{"source changed", "journal changed", "namespace replaced", "process changed", "copy changed", "extra pod", "bootstrap job remains", "foreign controller", "contradictory PV", "backend alias first", "backend alias last", "contradictory engine", "wrong API", "wrong kind", "wrong namespace", "missing resource version", "partial list", "duplicate UID", "read failed"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := recoveredStorageObservationFixture(t, scenario)
			got, err := p.observeRecoveredStorage(t.Context())
			if err == nil || got.binding.Pod.UID != "" || strings.Contains(err.Error(), "private-storage-read-canary") {
				t.Fatal("unqualified native storage graph returned a witness or private read error")
			}
		})
	}
}

func TestRecoveredStorageObservationKeepsUnrelatedEnginesVisibleWithoutJoiningThem(t *testing.T) {
	for _, scenario := range []string{"unrelated engine", "status advanced", "namespace status advanced"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := recoveredStorageObservationFixture(t, scenario)
			got, err := p.observeRecoveredStorage(t.Context())
			if err != nil || got.binding.Engine.UID == "" {
				t.Fatalf("a coherent complete storage inventory was refused: %v", err)
			}
		})
	}
}
