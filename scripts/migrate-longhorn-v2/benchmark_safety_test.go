package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func benchmarkInventoryFixture() inventory {
	s := evacuatedFixture()
	s.nodes = s.nodes[1:]
	for i := range s.nodes {
		old, name := text(s.nodes[i], "metadata", "name"), fmt.Sprintf("prod-worker-%d", i+1)
		n := nodeFixture()
		n["metadata"].(object)["name"], n["metadata"].(object)["uid"] = name, "uid-"+name
		n["metadata"].(object)["labels"].(object)["node.kubernetes.io/instance-type"] = "cx43"
		n["spec"].(object)["providerID"] = fmt.Sprintf("hcloud://%d", i+100)
		n["status"].(object)["nodeInfo"].(object)["systemUUID"] = "hardware-" + name
		// Comparability uses the observed opaque OS identity, not a version pin.
		n["status"].(object)["nodeInfo"].(object)["osImage"] = "Talos (fixture-before-upgrade)"
		n["status"].(object)["nodeInfo"].(object)["architecture"] = "amd64"
		s.nodes[i] = n
		s.storageNodes[i]["metadata"].(object)["name"] = name
		s.storageNodes[i]["spec"].(object)["allowScheduling"] = true
		s.storageNodes[i]["spec"].(object)["evictionRequested"] = false
		for _, r := range s.replicas {
			if text(r, "spec", "nodeID") == old {
				r["spec"].(object)["nodeID"] = name
			}
		}
		for _, e := range s.engines {
			if text(e, "spec", "nodeID") == old {
				e["spec"].(object)["nodeID"] = name
			}
		}
	}
	return s
}

func TestBaselineRefusesTemporaryOrChangedReplicaPlacement(t *testing.T) {
	if _, err := benchmarkMachines(benchmarkInventoryFixture(), "v1"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*inventory){
		"temporary copy":       func(s *inventory) { s.replicas[0]["spec"].(object)["nodeID"] = "autoscale-longhorn-v1-other" },
		"huge pages missing":   func(s *inventory) { s.nodes[0]["status"].(object)["capacity"] = object{} },
		"same physical server": func(s *inventory) { s.nodes[0]["spec"].(object)["providerID"] = text(s.nodes[1], "spec", "providerID") },
		"mixed engines":        func(s *inventory) { s.volumes[0]["spec"].(object)["dataEngine"] = "v2" },
	} {
		t.Run(name, func(t *testing.T) {
			s := benchmarkInventoryFixture()
			change(&s)
			if _, err := benchmarkMachines(s, "v1"); err == nil {
				t.Fatal("incomparable baseline accepted")
			}
		})
	}
}

func TestComparisonCannotRunWithAnIncompleteOrForeignBaseline(t *testing.T) {
	s := benchmarkInventoryFixture()
	machines, err := benchmarkMachines(s, "v1")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := parseFIO(fioFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	record := benchmarkRecord{Version: 2, Phase: "v1", Source: strings.Repeat("1", 40), Run: "1234", Image: fioImage, Workload: benchmarkWorkload(), Namespace: "longhorn-benchmark-v1-1234", NamespaceUID: "scratch-uid", Machines: machines, Measurements: metrics, Cleaned: true, CompletedAt: "2026-10-29T18:00:00Z"}
	compute, managers, probe := benchmarkComputeFixture()
	record.CPU, err = benchmarkCompute(compute, managers, probe, "v1")
	if err != nil {
		t.Fatal(err)
	}
	record.Resources = map[string]benchmarkResource{
		"Namespace": {Name: record.Namespace, UID: "scratch-uid"}, "Pod": {Name: "fio", Namespace: record.Namespace, UID: "pod"},
		"NetworkPolicy":         {Name: "default-deny", Namespace: record.Namespace, UID: "policy"},
		"PersistentVolumeClaim": {Name: "fio-data", Namespace: record.Namespace, UID: "claim"}, "StorageClass": {Name: "lh-benchmark-v1-1234", UID: "class"},
		"PersistentVolume": {Name: "scratch-pv", UID: "pv"}, "LonghornVolume": {Name: "scratch-volume", Namespace: "longhorn-system", UID: "volume"},
	}
	test := func(record benchmarkRecord, current []object, terminal bool) error {
		b, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		cm := resource("longhorn-v2-benchmark-v1")
		cm["data"] = object{"record": string(b)}
		c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			if program == "kubectl" {
				if args[2] != "get" {
					t.Fatal("unvalidated baseline caused a write")
				}
				return json.Marshal(cm)
			}
			if program != "gh" {
				t.Fatalf("unexpected command %s", program)
			}
			status := "in_progress"
			if terminal {
				status = "completed"
			}
			return json.Marshal(object{"status": status, "head_sha": record.Source, "head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
		}}
		return requireBenchmarkBaseline(context.Background(), c, current)
	}
	if err = test(record, s.nodes, true); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*benchmarkRecord){
		"incomplete cleanup":         func(r *benchmarkRecord) { r.Cleaned = false },
		"missing trial":              func(r *benchmarkRecord) { r.Measurements = r.Measurements[1:] },
		"wrong image":                func(r *benchmarkRecord) { r.Image = "unpinned" },
		"changed workload":           func(r *benchmarkRecord) { r.Workload = "changed" },
		"missing namespace identity": func(r *benchmarkRecord) { r.NamespaceUID = "" },
		"lost resource identities":   func(r *benchmarkRecord) { r.Resources = nil },
		"lost storage cpu context":   func(r *benchmarkRecord) { r.CPU = benchmarkCPU{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := record
			mutate(&r)
			if test(r, s.nodes, true) == nil {
				t.Fatal("incomplete baseline accepted")
			}
		})
	}
	if test(record, s.nodes, false) == nil || test(record, s.nodes[1:], true) == nil {
		t.Fatal("live workflow or missing permanent hardware accepted")
	}
	changed := benchmarkInventoryFixture()
	changed.nodes[0]["spec"].(object)["providerID"] = "hcloud://replacement"
	if test(record, changed.nodes, true) == nil {
		t.Fatal("baseline reused after a hardware change")
	}
	changedOS := benchmarkInventoryFixture()
	changedOS.nodes[0]["status"].(object)["nodeInfo"].(object)["osImage"] = "Talos (fixture-after-upgrade)"
	if test(record, changedOS.nodes, true) == nil {
		t.Fatal("baseline reused after an operating-system change")
	}
}

func TestBenchmarkJournalAcceptsEveryCreatedAssetAndRejectsForeignPolicy(t *testing.T) {
	record := benchmarkRecord{Phase: "v1", Run: "1234", Namespace: "longhorn-benchmark-v1-1234", Resources: map[string]benchmarkResource{}}
	assets, err := benchmarkResources(record.Phase, record.Run, record.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		kind := text(asset, "kind")
		asset["metadata"].(object)["uid"] = "owned-" + kind
		record.Resources[kind] = benchmarkIdentity(asset)
		if kind == "Namespace" {
			record.NamespaceUID = "owned-" + kind
		}
		if err = validateBenchmarkResources(record); err != nil {
			t.Fatalf("valid scratch journal rejected after creating %s: %v", kind, err)
		}
	}
	for name, policy := range map[string]benchmarkResource{
		"different policy": {Name: "another-policy", Namespace: record.Namespace, UID: "policy"},
		"other namespace":  {Name: "default-deny", Namespace: "production", UID: "policy"},
		"missing UID":      {Name: "default-deny", Namespace: record.Namespace},
	} {
		t.Run(name, func(t *testing.T) {
			record.Resources["NetworkPolicy"] = policy
			if validateBenchmarkResources(record) == nil {
				t.Fatal("foreign or unbound policy accepted in scratch journal")
			}
		})
	}
}

func TestScratchDeletionUsesAtomicUIDAndVersionPreconditions(t *testing.T) {
	writes := 0
	c := client{context: "admin@prod", run: func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(strings.Repeat("1", 40) + "\trefs/heads/main\n"), nil
		}
		writes++
		var options object
		if json.Unmarshal(body, &options) != nil || text(options, "kind") != "DeleteOptions" || text(options, "preconditions", "uid") != "uid-fio" || text(options, "preconditions", "resourceVersion") != "1" ||
			strings.Join(args[2:], " ") != "delete --raw=/api/v1/namespaces/scratch/pods/fio --filename=/dev/stdin" {
			t.Fatal("scratch delete lost its exact object preconditions")
		}
		return nil, errors.New("resource changed; refused by API")
	}}
	if c.deleteObserved(context.Background(), strings.Repeat("1", 40), "/api/v1/namespaces/scratch/pods/fio", resource("fio")) == nil || writes != 1 {
		t.Fatal("API conflict treated as successful deletion")
	}
	if c.deleteObserved(context.Background(), strings.Repeat("1", 40), "/api/v1/namespaces/scratch/pods/fio", object{}) == nil || writes != 1 {
		t.Fatal("deletion without identity was attempted")
	}
}

func TestScratchBindingCannotRetireAnApplicationVolume(t *testing.T) {
	claim := resource("fio-data")
	claim["metadata"].(object)["namespace"] = "scratch"
	class := resource("scratch-class")
	pv := resource("scratch-pv")
	pv["spec"] = object{"persistentVolumeReclaimPolicy": "Retain", "claimRef": object{"uid": text(claim, "metadata", "uid"), "name": "fio-data", "namespace": "scratch"}, "storageClassName": "scratch-class", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "scratch-volume"}}
	volume := resource("scratch-volume")
	volume["status"] = object{"kubernetesStatus": object{"pvName": "scratch-pv"}}
	if err := verifyScratchBinding(pv, volume, claim, class); err != nil {
		t.Fatal(err)
	}
	pv["spec"].(object)["claimRef"].(object)["uid"] = "application-claim"
	if verifyScratchBinding(pv, volume, claim, class) == nil {
		t.Fatal("application data could be retired as scratch")
	}
}

func scratchDeleteClient(deleteResult func(attempt int) error, versions *[]string, gets *int) client {
	deletes := 0
	return client{context: "admin@prod", run: func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(strings.Repeat("1", 40) + "\trefs/heads/main\n"), nil
		}
		if args[2] == "delete" {
			var options object
			if json.Unmarshal(body, &options) != nil {
				return nil, errors.New("delete options are not JSON")
			}
			*versions = append(*versions, text(options, "preconditions", "resourceVersion"))
			deletes++
			if err := deleteResult(deletes); err != nil {
				return nil, err
			}
			return nil, nil
		}
		*gets++
		o := resource("fio")
		o["metadata"].(object)["resourceVersion"] = fmt.Sprint(*gets)
		return json.Marshal(o)
	}}
}

func TestScratchDeletionRetriesAConflictWithFreshPreconditions(t *testing.T) {
	var versions []string
	gets := 0
	// The deletion wait fails here on purpose: the object still exists, and the
	// cancelled context ends that wait without its polling delay.
	ctx, cancel := context.WithCancel(context.Background())
	c := scratchDeleteClient(func(attempt int) error {
		if attempt == 1 {
			return fmt.Errorf("delete refused: %w", errConflict)
		}
		cancel()
		return nil
	}, &versions, &gets)
	if removeScratch(ctx, c, strings.Repeat("1", 40), "pod", "scratch", "/api/v1/namespaces/scratch/pods/fio", resource("fio")) == nil {
		t.Fatal("deletion wait was expected to stop on the cancelled context")
	}
	if strings.Join(versions, ",") != "1,2" {
		t.Fatalf("retry must use the freshly read version, sent %v", versions)
	}
}

func TestScratchDeletionNeverRetriesOtherFailuresAndStopsAtItsBound(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{"command failure": {errors.New("external command failed"), 1}, "persistent conflict": {errConflict, scratchDeleteAttempts}} {
		t.Run(name, func(t *testing.T) {
			var versions []string
			gets := 0
			c := scratchDeleteClient(func(int) error { return tc.err }, &versions, &gets)
			err := removeScratch(context.Background(), c, strings.Repeat("1", 40), "pod", "scratch", "/api/v1/namespaces/scratch/pods/fio", resource("fio"))
			if !errors.Is(err, tc.err) || len(versions) != tc.want {
				t.Fatalf("err=%v, delete attempts=%d, want %d", err, len(versions), tc.want)
			}
		})
	}
}

func TestScratchDeletionNeverRetriesKindsClearedByEarlierChecks(t *testing.T) {
	for _, kind := range []string{"persistentvolume", "volumes.longhorn.io", "storageclass", "namespace"} {
		t.Run(kind, func(t *testing.T) {
			var versions []string
			gets := 0
			c := scratchDeleteClient(func(int) error { return errConflict }, &versions, &gets)
			err := removeScratch(context.Background(), c, strings.Repeat("1", 40), kind, "", "/api/v1/fio", resource("fio"))
			if !errors.Is(err, errConflict) || len(versions) != 1 {
				t.Fatalf("err=%v, delete attempts=%d, want one", err, len(versions))
			}
		})
	}
}

func TestExecuteClassifiesOnlyAConflictAndKeepsStderrPrivate(t *testing.T) {
	for script, conflict := range map[string]bool{
		`echo 'Error from server (Conflict): private detail' >&2; exit 1`:  true,
		`echo 'Error from server (Forbidden): private detail' >&2; exit 1`: false,
	} {
		_, err := execute(context.Background(), nil, "sh", "-c", script)
		if err == nil || errors.Is(err, errConflict) != conflict || strings.Contains(err.Error(), "private detail") {
			t.Fatalf("%s: got %v", script, err)
		}
	}
}

func TestAMeasurementTakenDuringAStorageFaultIsNotComparable(t *testing.T) {
	baseline := benchmarkInventoryFixture()
	machines, err := benchmarkMachines(baseline, "v1")
	if err != nil {
		t.Fatal(err)
	}
	volume := text(baseline.volumes[0], "metadata", "name")
	replicas := scratchReplicas(baseline, volume)
	if len(replicas) != 3 {
		t.Fatalf("fixture has %d replicas", len(replicas))
	}
	if err := verifyTrialComparable(benchmarkInventoryFixture(), "v1", machines, volume, replicas); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*inventory){
		"a volume lost health": func(s *inventory) { s.volumes[0]["status"].(object)["robustness"] = "degraded" },
		"a replica rebuilt in place": func(s *inventory) {
			for _, r := range s.replicas {
				if text(r, "spec", "volumeName") == volume {
					r["spec"].(object)["healthyAt"] = "2030-01-01T00:00:00Z"
					return
				}
			}
		},
		"a replica replaced": func(s *inventory) {
			for _, r := range s.replicas {
				if text(r, "spec", "volumeName") == volume {
					r["metadata"].(object)["name"] = "replacement"
					return
				}
			}
		},
		"a worker changed": func(s *inventory) {
			for _, n := range s.nodes {
				if text(n, "metadata", "name") == "prod-worker-1" {
					n["status"].(object)["nodeInfo"].(object)["osImage"] = "changed"
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := benchmarkInventoryFixture()
			change(&s)
			if err := verifyTrialComparable(s, "v1", machines, volume, replicas); err == nil {
				t.Fatal("the measurement was accepted")
			}
		})
	}
}

func TestTrialHealthCannotStayUnproven(t *testing.T) {
	failedRead, unhealthy := errors.New("read failed"), func() error { return errors.New("unhealthy") }
	healthy := func() error { return nil }
	for name, samples := range map[string][]func(*int) error{
		"reads never succeed": {
			func(u *int) error { return judgeTrialSample(u, failedRead, healthy) },
			func(u *int) error { return judgeTrialSample(u, failedRead, healthy) },
			func(u *int) error { return judgeTrialSample(u, failedRead, healthy) },
		},
		"a volume stays unhealthy": {
			func(u *int) error { return judgeTrialSample(u, nil, unhealthy) },
			func(u *int) error { return judgeTrialSample(u, nil, unhealthy) },
			func(u *int) error { return judgeTrialSample(u, nil, unhealthy) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			unproven := 0
			var err error
			for _, sample := range samples {
				err = sample(&unproven)
			}
			if err == nil {
				t.Fatal("the trial continued with health unproven")
			}
		})
	}
	unproven := 0
	for range 2 {
		if err := judgeTrialSample(&unproven, nil, unhealthy); err != nil {
			t.Fatal("a short gap held the run")
		}
	}
	if err := judgeTrialSample(&unproven, nil, healthy); err != nil || unproven != 0 {
		t.Fatal("a healthy read did not clear a short gap")
	}
	if err := judgeTrialSample(&unproven, nil, func() error { return errScratchRebuilt }); err == nil {
		t.Fatal("a rebuilt scratch replica did not hold at once")
	}
}
