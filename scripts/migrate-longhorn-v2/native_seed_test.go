package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nativeSeedDispatch() map[string]string {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = nativeSeedOperation, nativeSeedConfirmation
	env["LONGHORN_V2_NODE"], env["LONGHORN_V2_DATABASE"] = "", ""
	return env
}

func nativeSeedRetirementFixture(t *testing.T) benchmarkRecord {
	t.Helper()
	r := retirementFenceFixture(t)
	r.Version, r.Namespace, r.Workload = 3, benchmarkNamespace, nativeSeedWorkload()
	for kind, identity := range r.Resources {
		if kind == "Namespace" {
			identity.Name = benchmarkNamespace
		}
		if kind == "Pod" || kind == "PersistentVolumeClaim" || kind == "NetworkPolicy" {
			identity.Namespace = benchmarkNamespace
		}
		r.Resources[kind] = identity
	}
	r.TagNodes = map[string]string{}
	r.V1Runtime = &benchmarkV1Runtime{Image: benchmarkRetirementImage, SettingUID: "setting-uid", SettingVersion: "17"}
	for _, n := range []string{"1", "2", "3"} {
		r.TagNodes["prod-worker-"+n] = "worker-uid-" + n
		r.V1Runtime.Epochs = append(r.V1Runtime.Epochs, benchmarkManagerEpoch{
			Pod: "instance-manager-" + n, PodUID: "pod-" + n, ManagerUID: "manager-" + n, Node: "prod-worker-" + n,
			ContainerID: "containerd://" + strings.Repeat(n, 64), StartedAt: time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC),
		})
	}
	return r
}

func TestNativeSeedRequiresItsOwnFixedProtectedOperation(t *testing.T) {
	for _, fault := range []string{"", "operation", "confirmation", "branch", "attempt", "node", "database", "workflow"} {
		t.Run(fault, func(t *testing.T) {
			env := nativeSeedDispatch()
			switch fault {
			case "operation":
				env["LONGHORN_V2_OPERATION"] = "benchmark-v1"
			case "confirmation":
				env["LONGHORN_V2_CONFIRM"] = "benchmark-longhorn-storage"
			case "branch":
				env["GITHUB_REF"] = "refs/heads/unsafe"
			case "attempt":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "node":
				env["LONGHORN_V2_NODE"] = "prod-worker-1"
			case "database":
				env["LONGHORN_V2_DATABASE"] = "umami-db"
			case "workflow":
				env["GITHUB_WORKFLOW_REF"] = "other"
			}
			if err := authorizeNativeSeed(func(k string) string { return env[k] }); (err == nil) != (fault == "") {
				t.Fatal("native seed admission did not preserve its explicit scope", fault, err)
			}
		})
	}
}

func TestNativeSeedCannotBorrowTheBenchmarkOwnerOrJournal(t *testing.T) {
	for _, seed := range []bool{false, true} {
		for _, nativeOwner := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "benchmark-client", true: "seed-client"}[seed], map[bool]string{false: "benchmark-owner", true: "seed-owner"}[nativeOwner]}, "/"), func(t *testing.T) {
				env := nativeSeedDispatch()
				if !nativeOwner {
					env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = "benchmark-v1", "benchmark-longhorn-storage"
				}
				for k, v := range env {
					t.Setenv(k, v)
				}
				r := nativeSeedRetirementFixture(t)
				r.Source = env["GITHUB_SHA"]
				if !seed {
					r.Workload = benchmarkWorkload()
				}
				if actual := activeBenchmarkV1FenceOwner(client{context: "admin@prod", nativeSeed: seed}, r); actual != (seed == nativeOwner) {
					t.Fatal("a scratch owner escaped its own protected operation")
				}
			})
		}
	}
	for _, seed := range []bool{false, true} {
		t.Run(map[bool]string{false: "benchmark-reader", true: "seed-reader"}[seed], func(t *testing.T) {
			r := nativeSeedRetirementFixture(t)
			if !seed {
				r.Workload = benchmarkWorkload()
			}
			body, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			want := "longhorn-v2-benchmark-v1"
			if seed {
				want = nativeSeedJournal
			}
			c := client{context: "admin@prod", nativeSeed: seed, run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" || args[3] != "configmap" || args[4] != want {
					t.Fatal("journal reader chose a different operation's object", program, args)
				}
				cm := resource(want)
				cm["data"] = object{"record": string(body)}
				return json.Marshal(cm)
			}}
			if _, _, err = readBenchmark(t.Context(), c, "v1"); err != nil {
				t.Fatal("the original owned journal was refused", err)
			}
		})
	}
}

func TestNativeSeedJournalRefusesForeignResultsAndIncompleteRuntimeReceipts(t *testing.T) {
	for _, fault := range []string{"benchmark workload", "legacy version", "different namespace", "claimed baseline", "recovery owner", "missing runtime", "missing tag owner"} {
		t.Run(fault, func(t *testing.T) {
			r := nativeSeedRetirementFixture(t)
			switch fault {
			case "benchmark workload":
				r.Workload = benchmarkWorkload()
			case "legacy version":
				r.Version = 2
			case "different namespace":
				r.Namespace = "another"
			case "claimed baseline":
				r.Cleaned = true
			case "recovery owner":
				r.Recovery = &benchmarkRecoveryOwner{Source: strings.Repeat("2", 40), Run: "5678", JournalUID: "other"}
			case "missing runtime":
				r.V1Runtime = nil
			case "missing tag owner":
				delete(r.TagNodes, "prod-worker-1")
			}
			body, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			c := client{context: "admin@prod", nativeSeed: true, run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "kubectl" || args[2] != "get" || args[4] != nativeSeedJournal {
					t.Fatal("a refused journal caused a write or selected another scope")
				}
				cm := resource(nativeSeedJournal)
				cm["data"] = object{"record": string(body)}
				return json.Marshal(cm)
			}}
			if _, _, err = readBenchmark(t.Context(), c, "v1"); err == nil {
				t.Fatal("a seed journal accepted foreign or incomplete evidence")
			}
		})
	}
	c := client{nativeSeed: true, run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("a V2 journal request escaped the fixed V1 scope")
		return nil, nil
	}}
	if _, _, err := readBenchmark(t.Context(), c, "v2"); err == nil {
		t.Fatal("the seed client selected V2")
	}
}

func TestNativeSeedChangesOnlyTheFixedDisposableProbe(t *testing.T) {
	ordinary, err := benchmarkScratchAssets(client{}, "v1", "1234", benchmarkNamespace, benchmarkClientNode)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := benchmarkScratchAssets(client{nativeSeed: true}, "v1", "1234", benchmarkNamespace, benchmarkClientNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 4 || len(seed) != 4 {
		t.Fatal("the existing protected workspace resources changed")
	}
	for i := range ordinary {
		kind := text(seed[i], "kind")
		if kind == "Pod" || kind == "PersistentVolumeClaim" {
			if value(seed[i], "metadata", "labels", "velero.io/exclude-from-backup") != nil {
				t.Fatal("the explicit canary backup still excludes its source")
			}
			value(seed[i], "metadata", "labels").(object)["velero.io/exclude-from-backup"] = "true"
		}
		if text(seed[i], "kind") == "Pod" {
			if text(seed[i], "metadata", "annotations", "backup.velero.io/backup-volumes") != "data" {
				t.Fatal("the source does not opt in its exact filesystem volume")
			}
			delete(value(seed[i], "metadata").(object), "annotations")
			if value(seed[i], "spec", "activeDeadlineSeconds") != 2700 || text(value(seed[i], "spec", "containers").([]any)[0].(object), "name") != "fio" {
				t.Fatal("the source probe escaped its bounded container profile")
			}
			value(seed[i], "spec", "containers").([]any)[0].(object)["args"] = []any{fioScript}
			value(seed[i], "spec").(object)["activeDeadlineSeconds"] = benchmarkDeadlineSeconds
		}
		a, e := json.Marshal(ordinary[i])
		if e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(seed[i])
		if e != nil || string(a) != string(b) {
			t.Fatal("the seed widened the reviewed class, retention, namespace, placement or security profile")
		}
	}
	for _, scope := range []struct{ phase, namespace string }{{"v2", benchmarkNamespace}, {"v1", "another"}} {
		if _, err = benchmarkScratchAssets(client{nativeSeed: true}, scope.phase, "1234", scope.namespace, benchmarkClientNode); err == nil {
			t.Fatal("the native source assets escaped their fixed engine or namespace")
		}
	}
}
