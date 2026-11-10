package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkOwner = "storage.platform.devantler.tech/benchmark-owner"

type benchmarkMachine struct{ ProviderID, SystemUUID, OSImage, Architecture string }
type benchmarkResource struct{ Name, Namespace, UID string }
type benchmarkRecord struct {
	Recovery                                                                  *benchmarkRecoveryOwner          `json:",omitempty"`
	Fences                                                                    map[string]benchmarkFenceReceipt `json:",omitempty"`
	FenceCheckpoint                                                           string                           `json:",omitempty"`
	FencePending                                                              string                           `json:",omitempty"`
	PendingCreate                                                             string                           `json:",omitempty"`
	TagNodes                                                                  map[string]string                `json:",omitempty"`
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
	raw := text(cm, "data", "record")
	fields := map[string][]string{
		"":         {"Recovery", "Fences", "FenceCheckpoint", "FencePending", "PendingCreate", "TagNodes", "Resources", "Version", "Phase", "Source", "Run", "Image", "Workload", "Namespace", "NamespaceUID", "CompletedAt", "Machines", "Measurements", "Cleaned", "CPU"},
		"Recovery": {"Source", "Run", "JournalUID", "Deletes"},
	}
	for _, kind := range []string{"Namespace", "NetworkPolicy", "Pod", "PersistentVolumeClaim", "StorageClass", "PersistentVolume", "LonghornVolume"} {
		fields["Resources."+kind] = []string{"Name", "Namespace", "UID"}
		fields["Recovery.Deletes."+kind] = []string{"Name", "Namespace", "UID"}
	}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		fields["Machines."+name] = []string{"ProviderID", "SystemUUID", "OSImage", "Architecture"}
	}
	if len(raw) > 1024*1024 || strictjson.Validate([]byte(raw), fields) != nil {
		return nil, record, errors.New("benchmark journal is incomplete or ambiguous")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&record) != nil {
		return nil, record, errors.New("benchmark journal is malformed")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !benchmarkNamespaceMatches(record) || record.Phase != phase || record.Image != fioImage || record.Workload != benchmarkWorkload() ||
		!validBenchmarkOwner(record.Run, record.Source) || len(record.Machines) != 3 {
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
	if err = validateBenchmarkFenceReceipts(record); err != nil {
		return nil, record, err
	}
	if len(record.Fences) != 0 {
		for _, name := range benchmarkFenceNames(phase, record.Run) {
			fields["Fences."+name] = []string{"PolicyUID", "BindingUID"}
		}
		if strictjson.Validate([]byte(raw), fields) != nil {
			return nil, record, errors.New("benchmark fence receipts are ambiguous")
		}
	}
	if record.Recovery != nil && (!validBenchmarkOwner(record.Recovery.Run, record.Recovery.Source) || record.Recovery.Run == record.Run || record.Recovery.JournalUID != text(cm, "metadata", "uid") || record.Cleaned) {
		return nil, record, errors.New("benchmark recovery ownership is invalid")
	}
	if record.Recovery != nil {
		for kind, intent := range record.Recovery.Deletes {
			if (kind != "Pod" && kind != "NetworkPolicy") || intent.UID == "" || record.Resources[kind] != intent {
				return nil, record, errors.New("benchmark deletion intent lacks its original UID receipt")
			}
		}
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

func benchmarkTagNodes(nodes []object) (map[string]string, error) {
	indexedNodes, err := indexed(nodes)
	if err != nil {
		return nil, err
	}
	result, seen := map[string]string{}, map[string]bool{}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		uid := text(indexedNodes[name], "metadata", "uid")
		if uid == "" || seen[uid] {
			return nil, errors.New("benchmark tag nodes need three distinct Longhorn UIDs")
		}
		result[name], seen[uid] = uid, true
	}
	return result, nil
}

func benchmarkTags(ctx context.Context, c client, source, tag string, nodes map[string]string, adding bool) error {
	if len(nodes) != 3 {
		return errors.New("benchmark tag node receipts are incomplete")
	}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		if err := c.setBenchmarkTag(ctx, source, tag, name, nodes[name], adding); err != nil {
			return err
		}
	}
	return nil
}

// setBenchmarkTag judges every attempt from a fresh read. A node's resource version moves
// often, and an unretried patch would strand a finished run with no way to recover it.
func (c client) setBenchmarkTag(ctx context.Context, source, tag, name, expectedUID string, adding bool) error {
	if expectedUID == "" {
		return errors.New("benchmark tag requires its recorded Longhorn node UID")
	}
	var err error
	for attempt := range refreshAttempts {
		if attempt > 0 {
			if pause := refreshPause(ctx); pause != nil {
				return errors.Join(err, pause)
			}
		}
		var n object
		if n, err = c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false); err != nil {
			continue
		}
		if text(n, "metadata", "uid") != expectedUID {
			return errors.New("benchmark Longhorn node was replaced")
		}
		owner := text(n, "metadata", "annotations", benchmarkOwner)
		old, _ := value(n, "spec", "tags").([]any)
		tags, found := []any{}, false
		for _, existing := range old {
			if existing == tag {
				found = true
			} else {
				tags = append(tags, existing)
			}
		}
		// Only this run's retry can find its own write: a lost response hid its success.
		if attempt > 0 && (adding && owner == tag && found || !adding && owner == "" && !found) {
			return nil
		}
		if adding && owner != "" || !adding && owner != tag {
			return errors.New("benchmark node tag is owned by another operation")
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
		if err = c.patch(ctx, "nodes.longhorn.io", "longhorn-system", n, changes...); err == nil {
			return nil
		}
	}
	return err
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
	var v1Runtime benchmarkV1Runtime
	if phase == "v1" {
		v1Runtime, err = captureBenchmarkV1Runtime(ctx, c, slices.Sorted(maps.Keys(machines)))
		if err != nil {
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
	tag := benchmarkTag(phase, runID)
	objects, err := benchmarkResources(phase, runID, benchmarkNamespace)
	if err != nil {
		return err
	}
	namespace, err := observeBenchmarkWorkspace(ctx, c, "")
	if err != nil {
		return err
	}
	for _, asset := range objects {
		existing, err := c.get(ctx, strings.ToLower(text(asset, "kind")), text(asset, "metadata", "namespace"), text(asset, "metadata", "name"), true)
		if err != nil || existing != nil {
			return errors.New("a prior or foreign benchmark asset already occupies the reviewed scope")
		}
	}
	tagNodes, err := benchmarkTagNodes(s.storageNodes)
	if err != nil {
		return err
	}
	namespaceIdentity := benchmarkIdentity(namespace)
	record := benchmarkRecord{Version: 3, Phase: phase, Source: source, Run: runID, Image: fioImage, Workload: benchmarkWorkload(), Namespace: benchmarkNamespace, NamespaceUID: namespaceIdentity.UID,
		Resources: map[string]benchmarkResource{"Namespace": namespaceIdentity}, Machines: machines, TagNodes: tagNodes}
	body, _ := json.Marshal(record)
	cm, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": "longhorn-v2-benchmark-" + phase, "namespace": "longhorn-system"}, "data": object{"record": string(body)}})
	if err != nil {
		return err
	}
	if err = benchmarkTags(ctx, c, source, tag, record.TagNodes, true); err != nil {
		return err
	}
	created := map[string]object{}
	for _, o := range objects {
		actual, err := createBenchmarkScratch(ctx, c, &record, o)
		if err != nil {
			return fmt.Errorf("benchmark scratch %s could not be created; preserve the journal: %w", text(o, "kind"), err)
		}
		created[text(o, "kind")] = actual
	}
	pod := created["Pod"]
	var claim, pv, volume object
	if err = waitReady(ctx, func(ctx context.Context) error {
		current, err := c.get(ctx, "pod", record.Namespace, "fio", false)
		if err != nil || text(current, "status", "phase") != "Running" {
			return errors.New("benchmark consumer is not running")
		}
		if err = verifyBenchmarkPod(current, text(pod, "metadata", "uid")); err != nil {
			return err
		}
		claim, err = c.get(ctx, "persistentvolumeclaim", record.Namespace, "fio-data", false)
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
	pod, err = c.get(ctx, "pod", record.Namespace, "fio", false)
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
	start, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	scratchName := text(volume, "metadata", "name")
	replicas := scratchReplicas(start, scratchName)
	if len(replicas) != 3 {
		return errors.New("the scratch volume does not have exactly three replicas to observe")
	}
	if err = verifyTrialComparable(start, phase, machines, scratchName, replicas); err != nil {
		return err
	}
	current, err := c.get(ctx, "pod", record.Namespace, "fio", false)
	if err != nil {
		return err
	}
	if err = verifyBenchmarkPod(current, text(pod, "metadata", "uid")); err != nil {
		return err
	}
	if _, err = c.kubectl(ctx, nil, "exec", "--namespace="+record.Namespace, "fio", "--container=fio", "--", "/bin/sh", "-ec", "touch /tmp/benchmark-start"); err != nil {
		return err
	}
	// The Pod's own deadline ends the trials first, and it reports Failed when it does.
	sampled, unproven := time.Now(), 0
	if err = waitReadyWithin(ctx, benchmarkCompletionLimit, func(ctx context.Context) error {
		p, err := c.get(ctx, "pod", record.Namespace, "fio", false)
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
		}
		if time.Since(sampled) >= benchmarkSampleInterval {
			sampled = time.Now()
			s, readErr := c.snapshot(ctx)
			if err = judgeTrialSample(&unproven, readErr, func() error {
				return verifyTrialComparable(s, phase, machines, scratchName, replicas)
			}); err != nil {
				return fmt.Errorf("%w: %w", errReadinessTerminal, err)
			}
		}
		return errors.New("benchmark has not completed successfully")
	}); err != nil {
		return err
	}
	logs, err := c.kubectl(ctx, nil, "logs", "--namespace="+record.Namespace, "fio", "--container=fio")
	if err != nil {
		return err
	}
	record.Measurements, err = parseFIO(logs)
	if err != nil {
		return err
	}
	// The measurements exist only in this Pod's logs until they are kept, and a later hold
	// must not lose them.
	public, _ := json.Marshal(record.Measurements)
	fmt.Printf("Longhorn %s measurements: %s\n", phase, public)
	if err = saveBenchmarkProgress(ctx, c, source, runID, record); err != nil {
		return err
	}
	// Kubernetes now detaches the finished Pod's volume, so give that transition time.
	if err = waitReady(ctx, func(ctx context.Context) error {
		fresh, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		return verifyTrialComparable(fresh, phase, machines, scratchName, replicas)
	}); err != nil {
		return err
	}
	current, err = c.get(ctx, "pod", record.Namespace, "fio", false)
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
	if err = cleanupBenchmark(ctx, c, &record, v1Runtime, created, claim, pv, volume); err != nil {
		return err
	}
	if err = benchmarkTags(ctx, c, source, tag, record.TagNodes, false); err != nil {
		return err
	}
	currentJournal, original, err := readBenchmark(ctx, c, phase)
	if err != nil || text(currentJournal, "metadata", "uid") != text(cm, "metadata", "uid") || !reflect.DeepEqual(original, record) {
		return errors.New("benchmark journal ownership changed")
	}
	record.Cleaned, record.CompletedAt = true, time.Now().UTC().Format(time.RFC3339)
	if err = writeBenchmarkProgress(ctx, c, currentJournal, source, record); err != nil {
		return err
	}
	checkedJournal, checked, err := readBenchmark(ctx, c, phase)
	if err != nil || text(checkedJournal, "metadata", "uid") != text(cm, "metadata", "uid") || !reflect.DeepEqual(checked, record) {
		return errors.New("completed benchmark receipt could not be read back")
	}
	fmt.Printf("Longhorn %s: completed eighteen controlled fio trials; scratch resources removed.\n", phase)
	fmt.Println("Observed CPU configuration:", publicBenchmarkCPU(record.CPU))
	return nil
}

// scratchReplicas names each of the volume's replicas with the time it became healthy. A
// rebuild changes one or the other, even when it finished before anything looked.
func scratchReplicas(s inventory, volume string) map[string]string {
	replicas := map[string]string{}
	for _, r := range s.replicas {
		if text(r, "spec", "volumeName") == volume {
			replicas[text(r, "metadata", "name")] = text(r, "spec", "healthyAt")
		}
	}
	return replicas
}

var errScratchRebuilt = errors.New("a scratch replica was rebuilt during the benchmark")

// judgeTrialSample holds the run once health stays unproven for benchmarkUnprovenSamples reads
// in a row, whether reads fail or a volume stays unhealthy. A shorter gap can be an ordinary
// attach elsewhere on the cluster. A rebuilt scratch replica holds at once: its data path changed.
func judgeTrialSample(unproven *int, readErr error, verify func() error) error {
	err := readErr
	if err == nil {
		err = verify()
	}
	if err == nil {
		*unproven = 0
		return nil
	}
	if *unproven++; *unproven >= benchmarkUnprovenSamples || errors.Is(err, errScratchRebuilt) {
		return errors.New("storage health stayed unproven during the benchmark")
	}
	return nil
}

// verifyTrialComparable holds a measurement taken while storage was not as it started.
func verifyTrialComparable(s inventory, phase string, machines map[string]benchmarkMachine, volume string, replicas map[string]string) error {
	current, err := benchmarkMachines(s, phase)
	if err != nil || !reflect.DeepEqual(current, machines) {
		return errors.New("worker configuration or volume health changed during the benchmark")
	}
	if !reflect.DeepEqual(scratchReplicas(s, volume), replicas) {
		return errScratchRebuilt
	}
	return nil
}

func benchmarkIdentity(o object) benchmarkResource {
	return benchmarkResource{text(o, "metadata", "name"), text(o, "metadata", "namespace"), text(o, "metadata", "uid")}
}

func validateBenchmarkResources(record benchmarkRecord) error {
	if record.TagNodes != nil {
		if len(record.TagNodes) != 3 {
			return errors.New("benchmark tag node identity is incomplete")
		}
		seen := map[string]bool{}
		for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
			uid := record.TagNodes[name]
			if uid == "" || seen[uid] {
				return errors.New("benchmark tag node identity is invalid")
			}
			seen[uid] = true
		}
	}
	names := map[string]string{"Namespace": record.Namespace, "NetworkPolicy": "fio-default-deny", "Pod": "fio", "PersistentVolumeClaim": "fio-data", "StorageClass": "lh-benchmark-" + record.Phase + "-" + record.Run, "PersistentVolume": "", "LonghornVolume": ""}
	if record.PendingCreate != "" {
		_, known := names[record.PendingCreate]
		_, recorded := record.Resources[record.PendingCreate]
		if !known || record.PendingCreate == "PersistentVolume" || record.PendingCreate == "LonghornVolume" || recorded || record.Cleaned {
			return errors.New("benchmark pending creation is invalid")
		}
	}
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
	if record.Version == 3 && (record.Resources["Namespace"].UID == "" || record.PendingCreate == "Namespace") {
		return errors.New("the GitOps benchmark namespace must have a prior UID observation")
	}
	return nil
}

func saveBenchmarkProgress(ctx context.Context, c client, source, runID string, record benchmarkRecord) error {
	cm, previous, err := readBenchmark(ctx, c, record.Phase)
	if err != nil {
		return err
	}
	if cm == nil || previous.Source != source || previous.Run != runID || previous.Cleaned || previous.Recovery != nil || record.Recovery != nil {
		return errors.New("benchmark journal ownership changed")
	}
	if !reflect.DeepEqual(previous.TagNodes, record.TagNodes) {
		return errors.New("benchmark tag node identities changed")
	}
	if !reflect.DeepEqual(previous.Fences, record.Fences) || previous.FenceCheckpoint != record.FenceCheckpoint || previous.FencePending != record.FencePending {
		return errors.New("benchmark fence receipts changed outside their journal transition")
	}
	for kind, old := range previous.Resources {
		if record.Resources[kind] != old {
			return errors.New("benchmark scratch identity changed")
		}
	}
	return writeBenchmarkProgress(ctx, c, cm, source, record)
}

func writeBenchmarkProgress(ctx context.Context, c client, cm object, source string, record benchmarkRecord) error {
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
