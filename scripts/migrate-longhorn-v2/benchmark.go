package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const benchmarkOwner = "storage.platform.devantler.tech/benchmark-owner"

type benchmarkMachine struct{ ProviderID, SystemUUID, OSImage, Architecture string }
type benchmarkResource struct{ Name, Namespace, UID string }
type benchmarkRecord struct {
	Resources                                                                 map[string]benchmarkResource
	Version                                                                   int
	Phase, Source, Run, Image, Workload, Namespace, NamespaceUID, CompletedAt string
	Machines                                                                  map[string]benchmarkMachine
	Measurements                                                              []fioMeasurement
	Cleaned                                                                   bool
	CPU                                                                       benchmarkCPU
}

func benchmarkWorkload() string {
	sum := sha256.Sum256([]byte(fioScript))
	return hex.EncodeToString(sum[:])
}

func benchmarkMachines(s inventory, phase string) (map[string]benchmarkMachine, error) {
	if err := verifyRedundancy(s); err != nil {
		return nil, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return nil, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return nil, err
	}
	result := map[string]benchmarkMachine{}
	providers, hardware := map[string]bool{}, map[string]bool{}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		n, lh := nodes[name], storage[name]
		m := benchmarkMachine{text(n, "spec", "providerID"), text(n, "status", "nodeInfo", "systemUUID"), text(n, "status", "nodeInfo", "osImage"), text(n, "status", "nodeInfo", "architecture")}
		if n == nil || lh == nil || !hugepagesReady(n) || value(n, "spec", "unschedulable") == true || m.ProviderID == "" || m.SystemUUID == "" || m.OSImage == "" || m.Architecture != "amd64" ||
			text(n, "metadata", "labels", "node.kubernetes.io/instance-type") != "cx43" || value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") == true {
			return nil, errors.New("all three permanent CX43 workers must be prepared and schedulable for a comparable benchmark")
		}
		if providers[m.ProviderID] || hardware[m.SystemUUID] {
			return nil, errors.New("benchmark replicas must occupy three distinct physical workers")
		}
		providers[m.ProviderID], hardware[m.SystemUUID] = true, true
		result[name] = m
	}
	for _, v := range s.volumes {
		if text(v, "spec", "dataEngine") != phase {
			return nil, errors.New("benchmark must precede migration or follow complete engine migration")
		}
	}
	for _, r := range s.replicas {
		if _, ok := result[text(r, "spec", "nodeID")]; !ok {
			return nil, errors.New("replicas must be back on the permanent workers for the benchmark")
		}
	}
	if phase == "v1" {
		for name, lh := range storage {
			if _, permanent := result[name]; !permanent && value(lh, "spec", "allowScheduling") == true {
				return nil, errors.New("capture V1 baseline before enabling temporary replica storage")
			}
		}
	}
	return result, nil
}

func readBenchmark(ctx context.Context, c client, phase string) (object, benchmarkRecord, error) {
	cm, err := c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-benchmark-"+phase, true)
	if err != nil || cm == nil {
		return cm, benchmarkRecord{}, err
	}
	var record benchmarkRecord
	d := json.NewDecoder(strings.NewReader(text(cm, "data", "record")))
	d.DisallowUnknownFields()
	if d.Decode(&record) != nil {
		return nil, record, errors.New("benchmark journal is malformed")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || record.Version != 2 || record.Phase != phase || record.Image != fioImage || record.Workload != benchmarkWorkload() ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(record.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(record.Run) || len(record.Machines) != 3 || record.Namespace != "longhorn-benchmark-"+phase+"-"+record.Run {
		return nil, record, errors.New("benchmark journal does not match this reviewed procedure")
	}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		m := record.Machines[name]
		if m.ProviderID == "" || m.SystemUUID == "" || m.OSImage == "" || m.Architecture != "amd64" {
			return nil, record, errors.New("benchmark machine inventory is incomplete")
		}
	}
	if record.Cleaned && (record.NamespaceUID == "" || validateMeasurements(record.Measurements) != nil || validateBenchmarkCPU(record.CPU) != nil) {
		return nil, record, errors.New("completed benchmark measurements or cleanup identity are incomplete")
	}
	if err = validateBenchmarkResources(record); err != nil {
		return nil, record, err
	}
	return cm, record, nil
}

func requireBenchmarkBaseline(ctx context.Context, c client, current []object) error {
	_, record, err := readBenchmark(ctx, c, "v1")
	if err != nil || !record.Cleaned || len(record.Measurements) != 18 || record.CompletedAt == "" {
		return errors.New("the completed, cleaned V1 baseline is required before moving any replicas")
	}
	if _, err = time.Parse(time.RFC3339, record.CompletedAt); err != nil {
		return errors.New("baseline completion time is invalid")
	}
	byName, err := indexed(current)
	if err != nil {
		return err
	}
	for name, old := range record.Machines {
		n := byName[name]
		if n == nil {
			return errors.New("baseline permanent worker is missing")
		}
		if old.ProviderID != text(n, "spec", "providerID") || old.SystemUUID != text(n, "status", "nodeInfo", "systemUUID") || old.OSImage != text(n, "status", "nodeInfo", "osImage") || text(n, "status", "nodeInfo", "architecture") != old.Architecture || !hugepagesReady(n) {
			return errors.New("permanent worker identity or configuration differs from the baseline")
		}
	}
	return previousRunCompleted(ctx, c, record.Run, record.Source)
}

func (c client) create(ctx context.Context, source string, o object) (object, error) {
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return nil, err
	}
	body, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	b, err := c.kubectl(ctx, body, "create", "--filename=/dev/stdin", "--output=json")
	if err != nil {
		return nil, err
	}
	var created object
	if json.Unmarshal(b, &created) != nil || text(created, "metadata", "uid") == "" || text(created, "metadata", "resourceVersion") == "" ||
		text(created, "metadata", "name") != text(o, "metadata", "name") || text(created, "kind") != text(o, "kind") {
		return nil, errors.New("created resource identity could not be read back")
	}
	return created, nil
}

func benchmarkTags(ctx context.Context, c client, source, tag string, adding bool) error {
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		n, err := c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false)
		if err != nil {
			return err
		}
		owner := text(n, "metadata", "annotations", benchmarkOwner)
		if adding && owner != "" || !adding && owner != tag {
			return errors.New("benchmark node tag is owned by another operation")
		}
		old, _ := value(n, "spec", "tags").([]any)
		tags, found := []any{}, false
		for _, existing := range old {
			if existing == tag {
				found = true
			} else {
				tags = append(tags, existing)
			}
		}
		if adding && found || !adding && !found {
			return errors.New("benchmark tag and ownership disagree")
		}
		changes := []object{{"op": "add", "path": "/spec/tags", "value": tags}}
		if adding {
			changes[0]["value"] = append(tags, tag)
			changes = append(changes, metadataChanges(n, "annotations", map[string]string{benchmarkOwner: tag})...)
		} else {
			changes = append(changes, object{"op": "remove", "path": "/metadata/annotations/" + strings.ReplaceAll(benchmarkOwner, "/", "~1")})
		}
		if err = sourceCurrent(ctx, c.run, source); err != nil {
			return err
		}
		if err = c.patch(ctx, "nodes.longhorn.io", "longhorn-system", n, changes...); err != nil {
			return err
		}
	}
	return nil
}

func verifyBenchmarkPod(p object, uid string) error {
	containers, _ := value(p, "spec", "containers").([]any)
	statuses, _ := value(p, "status", "containerStatuses").([]any)
	if text(p, "metadata", "uid") != uid || text(p, "spec", "nodeName") != "prod-worker-3" || len(containers) != 1 || len(statuses) != 1 {
		return errors.New("benchmark consumer identity or placement differs")
	}
	container, _ := containers[0].(object)
	status, _ := statuses[0].(object)
	digest := strings.Split(fioImage, "@")[1]
	if text(container, "image") != fioImage || !reflect.DeepEqual(value(container, "args"), []any{fioScript}) ||
		!reflect.DeepEqual(value(container, "command"), []any{"/bin/sh", "-ec"}) || value(p, "spec", "automountServiceAccountToken") != false ||
		value(p, "spec", "hostUsers") != false ||
		text(status, "name") != "fio" || !strings.HasSuffix(text(status, "imageID"), digest) {
		return errors.New("benchmark executable or workload differs")
	}
	return nil
}

func runBenchmark(ctx context.Context, c client, source, runID, phase string) error {
	s, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	machines, err := benchmarkMachines(s, phase)
	if err != nil {
		return err
	}
	if phase == "v2" {
		if err = requireBenchmarkBaseline(ctx, c, s.nodes); err != nil {
			return err
		}
	}
	prior, _, err := readBenchmark(ctx, c, phase)
	if err != nil {
		return err
	}
	if prior != nil {
		return errors.New("benchmark journal already exists; inspect its completion and scratch-resource state before any recovery")
	}
	tag := "longhorn-benchmark-" + phase + "-" + runID
	objects, err := benchmarkResources(phase, runID, tag)
	if err != nil {
		return err
	}
	record := benchmarkRecord{Version: 2, Phase: phase, Source: source, Run: runID, Image: fioImage, Workload: benchmarkWorkload(), Namespace: tag, Machines: machines}
	body, _ := json.Marshal(record)
	cm, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": "longhorn-v2-benchmark-" + phase, "namespace": "longhorn-system"}, "data": object{"record": string(body)}})
	if err != nil {
		return err
	}
	if err = benchmarkTags(ctx, c, source, tag, true); err != nil {
		return err
	}
	created := map[string]object{}
	for _, o := range objects {
		actual, err := c.create(ctx, source, o)
		if err != nil {
			return fmt.Errorf("benchmark scratch %s could not be created; preserve the journal: %w", text(o, "kind"), err)
		}
		created[text(o, "kind")] = actual
		if record.Resources == nil {
			record.Resources = map[string]benchmarkResource{}
		}
		record.Resources[text(o, "kind")] = benchmarkIdentity(actual)
		if text(o, "kind") == "Namespace" {
			record.NamespaceUID = text(actual, "metadata", "uid")
		}
		if err = saveBenchmarkProgress(ctx, c, source, runID, record); err != nil {
			return err
		}
	}
	pod := created["Pod"]
	var claim, pv, volume object
	if err = waitReady(ctx, func() error {
		current, err := c.get(ctx, "pod", tag, "fio", false)
		if err != nil || text(current, "status", "phase") != "Running" {
			return errors.New("benchmark consumer is not running")
		}
		if err = verifyBenchmarkPod(current, text(pod, "metadata", "uid")); err != nil {
			return err
		}
		claim, err = c.get(ctx, "persistentvolumeclaim", tag, "fio-data", false)
		if err != nil || text(claim, "metadata", "uid") != text(created["PersistentVolumeClaim"], "metadata", "uid") || text(claim, "status", "phase") != "Bound" {
			return errors.New("benchmark claim has not bound")
		}
		pv, err = c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
		if err != nil || text(pv, "spec", "claimRef", "uid") != text(claim, "metadata", "uid") || text(pv, "spec", "storageClassName") != text(created["StorageClass"], "metadata", "name") || text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
			return errors.New("benchmark binding is outside scratch storage")
		}
		volume, err = c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(pv, "spec", "csi", "volumeHandle"), false)
		if err != nil || text(volume, "spec", "dataEngine") != phase || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" {
			return errors.New("benchmark volume is not healthy")
		}
		fresh, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		_, err = benchmarkMachines(fresh, phase)
		return err
	}); err != nil {
		return err
	}
	record.Resources["PersistentVolume"] = benchmarkIdentity(pv)
	record.Resources["LonghornVolume"] = benchmarkIdentity(volume)
	pod, err = c.get(ctx, "pod", tag, "fio", false)
	if err != nil {
		return err
	}
	if err = verifyBenchmarkPod(pod, text(created["Pod"], "metadata", "uid")); err != nil {
		return err
	}
	record.CPU, err = captureBenchmarkCPU(ctx, c, phase, pod)
	if err != nil {
		return err
	}
	if err = saveBenchmarkProgress(ctx, c, source, runID, record); err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	current, err := c.get(ctx, "pod", tag, "fio", false)
	if err != nil {
		return err
	}
	if err = verifyBenchmarkPod(current, text(pod, "metadata", "uid")); err != nil {
		return err
	}
	if _, err = c.kubectl(ctx, nil, "exec", "--namespace="+tag, "fio", "--container=fio", "--", "/bin/sh", "-ec", "touch /tmp/benchmark-start"); err != nil {
		return err
	}
	// The Pod's own deadline ends the trials first, and it reports Failed when it does.
	if err = waitReadyWithin(ctx, benchmarkCompletionLimit, func() error {
		p, err := c.get(ctx, "pod", tag, "fio", false)
		if err != nil {
			return err
		}
		if err = verifyBenchmarkPod(p, text(pod, "metadata", "uid")); err != nil {
			return err
		}
		switch text(p, "status", "phase") {
		case "Succeeded":
			return nil
		case "Failed":
			return fmt.Errorf("benchmark consumer failed: %w", errReadinessTerminal)
		default:
			return errors.New("benchmark has not completed successfully")
		}
	}); err != nil {
		return err
	}
	logs, err := c.kubectl(ctx, nil, "logs", "--namespace="+tag, "fio", "--container=fio")
	if err != nil {
		return err
	}
	record.Measurements, err = parseFIO(logs)
	if err != nil {
		return err
	}
	fresh, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	endingMachines, err := benchmarkMachines(fresh, phase)
	if err != nil || !reflect.DeepEqual(endingMachines, machines) {
		return errors.New("worker configuration or volume health changed during the benchmark")
	}
	current, err = c.get(ctx, "pod", tag, "fio", false)
	if err != nil {
		return err
	}
	if err = verifyBenchmarkPod(current, text(pod, "metadata", "uid")); err != nil {
		return err
	}
	endingCPU, err := captureBenchmarkCPU(ctx, c, phase, current)
	if err != nil || !reflect.DeepEqual(endingCPU, record.CPU) {
		return errors.New("admitted client or storage CPU context changed during the benchmark")
	}
	if err = cleanupBenchmark(ctx, c, source, tag, created, pv, volume); err != nil {
		return err
	}
	if err = benchmarkTags(ctx, c, source, tag, false); err != nil {
		return err
	}
	record.NamespaceUID, record.Cleaned, record.CompletedAt = text(created["Namespace"], "metadata", "uid"), true, time.Now().UTC().Format(time.RFC3339)
	body, _ = json.Marshal(record)
	cm, err = c.get(ctx, "configmap", "longhorn-system", text(cm, "metadata", "name"), false)
	if err != nil {
		return err
	}
	var original benchmarkRecord
	if json.Unmarshal([]byte(text(cm, "data", "record")), &original) != nil || original.Run != runID || original.Source != source || original.Cleaned {
		return errors.New("benchmark journal ownership changed")
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err = c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(body)}); err != nil {
		return err
	}
	_, checked, err := readBenchmark(ctx, c, phase)
	if err != nil || !reflect.DeepEqual(checked, record) {
		return errors.New("completed benchmark receipt could not be read back")
	}
	public, _ := json.Marshal(record.Measurements)
	fmt.Printf("Longhorn %s: completed eighteen controlled fio trials; scratch resources removed. Measurements: %s\n", phase, public)
	fmt.Println("Observed CPU configuration:", publicBenchmarkCPU(record.CPU))
	return nil
}

func benchmarkIdentity(o object) benchmarkResource {
	return benchmarkResource{text(o, "metadata", "name"), text(o, "metadata", "namespace"), text(o, "metadata", "uid")}
}

func validateBenchmarkResources(record benchmarkRecord) error {
	names := map[string]string{"Namespace": record.Namespace, "NetworkPolicy": "default-deny", "Pod": "fio", "PersistentVolumeClaim": "fio-data", "StorageClass": "lh-benchmark-" + record.Phase + "-" + record.Run, "PersistentVolume": "", "LonghornVolume": ""}
	if record.Cleaned && len(record.Resources) != len(names) {
		return errors.New("completed benchmark scratch identities are incomplete")
	}
	for kind, resource := range record.Resources {
		name, known := names[kind]
		namespace := ""
		if kind == "Pod" || kind == "PersistentVolumeClaim" || kind == "NetworkPolicy" {
			namespace = record.Namespace
		}
		if kind == "LonghornVolume" {
			namespace = "longhorn-system"
		}
		if !known || resource.UID == "" || resource.Name == "" || name != "" && resource.Name != name || resource.Namespace != namespace {
			return errors.New("benchmark scratch resource identity is invalid")
		}
	}
	if ns, present := record.Resources["Namespace"]; present && ns.UID != record.NamespaceUID {
		return errors.New("benchmark namespace identity disagrees with its journal")
	}
	return nil
}

func saveBenchmarkProgress(ctx context.Context, c client, source, runID string, record benchmarkRecord) error {
	cm, previous, err := readBenchmark(ctx, c, record.Phase)
	if err != nil {
		return err
	}
	if cm == nil || previous.Source != source || previous.Run != runID || previous.Cleaned {
		return errors.New("benchmark journal ownership changed")
	}
	for kind, old := range previous.Resources {
		if record.Resources[kind] != old {
			return errors.New("benchmark scratch identity changed")
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	return c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(body)})
}

func metadataChanges(o object, field string, values map[string]string) []object {
	changes := []object{}
	if value(o, "metadata", field) == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/" + field, "value": object{}})
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		changes = append(changes, object{"op": "add", "path": "/metadata/" + field + "/" + path, "value": values[key]})
	}
	return changes
}
