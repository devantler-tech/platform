package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type canaryRecord struct {
	Version                                                                   int
	Source, Run, Phase, Image, Workload, PayloadSHA256, Snapshot, CompletedAt string
	Resources                                                                 map[string]benchmarkResource
	Transport                                                                 string
	CSIScratch                                                                map[string]csiScratchEvidence
	RestoreVerified, ReattachVerified, Cleaned                                bool
}

// The canary is qualified for the instance-manager image shipped by the reviewed
// Longhorn 1.13.0 chart. A chart upgrade requires a new qualification, rather
// than accepting matching but unknown live declarations as image provenance.
const canaryManagerImage = "longhornio/longhorn-instance-manager:v1.13.0"

type canaryRun struct {
	c                                        client
	source, run                              string
	record                                   canaryRecord
	journalUID                               string
	csi                                      *csiCapture
	sourceClaim, sourceVolume, snapshotClass object
}

// canaryWorkload binds recovery evidence to both reviewed payload commands.
func canaryWorkload() string {
	digest := sha256.Sum256([]byte(canaryScript + canaryVerify))
	return hex.EncodeToString(digest[:])
}

// save conditionally updates the owned journal without changing previously
// recorded scratch identities, then requires an exact native readback.
func (p *canaryRun) save(ctx context.Context) error {
	name, err := canaryJournalName(p.record.Transport)
	if err != nil {
		return err
	}
	cm, err := p.c.get(ctx, "configmap", "longhorn-system", name, false)
	if err != nil {
		return err
	}
	var old canaryRecord
	if p.journalUID == "" || text(cm, "metadata", "uid") != p.journalUID || json.Unmarshal([]byte(text(cm, "data", "record")), &old) != nil || old.Source != p.source || old.Run != p.run || old.Transport != p.record.Transport || old.Cleaned {
		return errors.New("canary journal ownership changed")
	}
	for key, identity := range old.Resources {
		if p.record.Resources[key] != identity {
			return errors.New("canary scratch identity changed")
		}
	}
	body, err := json.Marshal(p.record)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return err
	}
	if err = p.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(body)}); err != nil {
		return err
	}
	cm, err = p.c.get(ctx, "configmap", "longhorn-system", name, false)
	if err != nil || text(cm, "metadata", "uid") != p.journalUID || text(cm, "data", "record") != string(body) {
		return errors.New("canary journal update could not be read back")
	}
	return nil
}

// remember persists a newly created resource identity before later operations.
func (p *canaryRun) remember(ctx context.Context, key string, o object) error {
	if text(o, "metadata", "name") == "" || text(o, "metadata", "uid") == "" {
		return errors.New("canary resource identity is incomplete")
	}
	p.record.Resources[key] = benchmarkIdentity(o)
	return p.save(ctx)
}

// create records the native identity returned by an exclusive scratch creation.
func (p *canaryRun) create(ctx context.Context, key string, desired object) (object, error) {
	o, err := p.c.create(ctx, p.source, desired)
	if err != nil {
		return nil, err
	}
	if err = p.remember(ctx, key, o); err != nil {
		return nil, err
	}
	return o, nil
}

// verifyCanaryPod requires the original restricted consumer and executable on
// the selected client before any command reads or writes the payload.
func verifyCanaryPod(p object, uid, node string) error {
	status, _ := value(p, "status").(object)
	if text(p, "metadata", "uid") != uid || value(p, "metadata", "deletionTimestamp") != nil || text(p, "spec", "nodeName") != node ||
		value(p, "spec", "automountServiceAccountToken") != false || value(p, "spec", "hostUsers") != false || !ready(status) {
		return errors.New("canary consumer identity or placement changed")
	}
	containers, _ := value(p, "spec", "containers").([]any)
	statuses, _ := value(p, "status", "containerStatuses").([]any)
	if len(containers) != 1 || len(statuses) != 1 {
		return errors.New("canary consumer executable is ambiguous")
	}
	c, _ := containers[0].(object)
	containerStatus, _ := statuses[0].(object)
	if text(c, "name") != "fio" || text(c, "image") != fioImage || !reflect.DeepEqual(value(c, "command"), []any{"/bin/sh", "-ec"}) || !reflect.DeepEqual(value(c, "args"), []any{canaryScript}) ||
		text(containerStatus, "name") != "fio" || !strings.HasSuffix(text(containerStatus, "imageID"), strings.Split(fioImage, "@")[1]) {
		return errors.New("canary executable differs from the reviewed probe")
	}
	return nil
}

// verifyCanarySettings binds both engine enablement and the effective SPDK
// configuration to the reviewed migration profile before disk registration or IO.
func verifyCanarySettings(ctx context.Context, c client) error {
	for name, expected := range map[string]string{
		"v1-data-engine": "true", "v2-data-engine": "true", "guaranteed-instance-manager-cpu": `{"v1":"6","v2":"25"}`,
		"data-engine-cpu-mask": `{"v2":"0x3"}`, "data-engine-number-of-cpu-cores": `{"v2":"0"}`, "data-engine-cpu-isolation-enabled": `{"v2":"true"}`,
		"data-engine-interrupt-mode-enabled": `{"v2":"false"}`, "data-engine-hugepage-enabled": `{"v2":"true"}`, "data-engine-memory-size": `{"v2":"2048"}`,
	} {
		o, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", name, false)
		if err != nil {
			return err
		}
		actual := text(o, "value")
		if strings.HasPrefix(expected, "{") {
			var want, got map[string]string
			if json.Unmarshal([]byte(expected), &want) != nil || json.Unmarshal([]byte(actual), &got) != nil || !reflect.DeepEqual(want, got) {
				return errors.New("live V2 settings have not adopted the reviewed configuration")
			}
		} else if actual != expected {
			return errors.New("both engines must remain enabled during the recovery canary")
		}
	}
	return nil
}

// canaryEnvironment proves both the actual worker clients and the three ready
// raw disks; enabling V2 alone cannot authorize a recovery probe.
func canaryEnvironment(ctx context.Context, c client, pools map[string]migrationPool) (inventory, error) {
	if err := verifyCanarySettings(ctx, c); err != nil {
		return inventory{}, err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return inventory{}, err
	}
	managers, err := c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return inventory{}, err
	}
	if err = verifyV2Runtime(s, managers, pools, canaryManagerImage); err != nil {
		return inventory{}, err
	}
	return s, nil
}

// verifyCanaryBindingIdentity rejects replacement source objects and target
// claims outside this run's exact native restore.
func verifyCanaryBindingIdentity(phase string, record canaryRecord, class, claim object) error {
	group := "source"
	if phase == "v2" {
		group = "target"
	} else if phase != "v1" {
		return errors.New("canary engine is outside this procedure")
	}
	expected := record.Resources[group+"/StorageClass"]
	if expected.UID == "" || expected.UID != text(class, "metadata", "uid") || expected.Name != text(class, "metadata", "name") {
		return errors.New("canary storage class was replaced")
	}
	if phase == "v1" {
		expected = record.Resources["source/PersistentVolumeClaim"]
		if expected.UID == "" || expected.UID != text(claim, "metadata", "uid") || expected.Name != text(claim, "metadata", "name") {
			return errors.New("canary source claim was replaced")
		}
	} else if text(claim, "metadata", "labels", "velero.io/restore-name") != "longhorn-v2-canary-"+record.Run || text(claim, "metadata", "labels", "velero.io/backup-name") != "longhorn-v2-canary-"+record.Run {
		return errors.New("V2 claim was not produced by the exact canary restore")
	}
	return nil
}

// binding waits for the owned consumer's complete claim/PV/backend graph and
// healthy replicas, preserving each identity for comparison and cleanup.
func (p *canaryRun) binding(ctx context.Context, namespace, class, phase, node, podUID string) (map[string]object, object, object, error) {
	var result map[string]object
	var pv, volume object
	err := waitReady(ctx, func(ctx context.Context) error {
		pod, err := p.c.get(ctx, "pod", namespace, "fio", false)
		if err != nil || text(pod, "status", "phase") != "Running" {
			return errors.New("canary consumer is not running")
		}
		if err = verifyCanaryPod(pod, podUID, node); err != nil {
			return err
		}
		claim, err := p.c.get(ctx, "persistentvolumeclaim", namespace, "fio-data", false)
		if err != nil || text(claim, "status", "phase") != "Bound" || text(claim, "spec", "storageClassName") != class {
			return errors.New("canary claim has not bound to its reviewed class")
		}
		pv, err = p.c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
		if err != nil {
			return err
		}
		volume, err = p.c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(pv, "spec", "csi", "volumeHandle"), false)
		if err != nil || text(volume, "spec", "dataEngine") != phase || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" {
			return errors.New("canary backend has not become healthy on the selected engine")
		}
		classObject, err := p.c.get(ctx, "storageclass", "", class, false)
		if err != nil {
			return err
		}
		if err = verifyCanaryBindingIdentity(phase, p.record, classObject, claim); err != nil {
			return err
		}
		if err = verifyScratchBinding(pv, volume, claim, classObject); err != nil {
			return err
		}
		s, err := p.c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = verifyRedundancy(s); err != nil {
			return err
		}
		result = map[string]object{"Pod": pod, "PersistentVolumeClaim": claim, "StorageClass": classObject}
		return nil
	})
	return result, pv, volume, err
}

// verifyBytes rebinds the live executable before checking the sentinel, length,
// stored checksum and optional original source digest.
func (p *canaryRun) verifyBytes(ctx context.Context, pod object, node, expected string) (string, error) {
	namespace := text(pod, "metadata", "namespace")
	fresh, err := p.c.get(ctx, "pod", namespace, "fio", false)
	if err != nil {
		return "", err
	}
	if err = verifyCanaryPod(fresh, text(pod, "metadata", "uid"), node); err != nil {
		return "", err
	}
	if _, err = p.c.kubectl(ctx, nil, "exec", "--namespace="+namespace, "fio", "--container=fio", "--", "/bin/sh", "-ec", canaryVerify); err != nil {
		return "", errors.New("canary restored bytes failed their checksum or sentinel check")
	}
	data, err := p.c.kubectl(ctx, nil, "exec", "--namespace="+namespace, "fio", "--container=fio", "--", "sha256sum", "/data/payload.bin")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[1] != "/data/payload.bin" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[0]) || expected != "" && expected != fields[0] {
		return "", errors.New("canary payload differs from the original source")
	}
	return fields[0], nil
}

// transfer requires a completed owned native transfer; CSI uses its separate
// data-mover proof rather than filesystem-backup fields.
func (p *canaryRun) transfer(ctx context.Context, desired, pod object, expectedSnapshot, sourceNS string) (object, string, error) {
	if p.csi != nil {
		return p.transferCSI(ctx, desired, expectedSnapshot)
	}
	kind, query := text(desired, "kind"), "podvolumebackups.velero.io"
	if kind == "Restore" {
		query = "podvolumerestores.velero.io"
	}
	created, err := p.create(ctx, kind, desired)
	if err != nil {
		return nil, "", err
	}
	var parent object
	var snapshot string
	err = waitReady(ctx, func(ctx context.Context) error {
		parent, err = p.c.get(ctx, strings.ToLower(kind)+"s.velero.io", "velero", text(created, "metadata", "name"), false)
		if err != nil || text(parent, "metadata", "uid") != text(created, "metadata", "uid") {
			return errors.New("canary backup or restore identity changed")
		}
		if err = cleanVeleroResult(parent); err != nil {
			return err
		}
		transfers, err := p.c.list(ctx, query, "velero")
		if err != nil {
			return err
		}
		if kind == "Restore" {
			pod, err = p.c.get(ctx, "pod", "longhorn-canary-v2-"+p.run, "fio", false)
			if err != nil {
				return err
			}
		}
		snapshot, err = verifyCanaryTransfer(transfers, parent, pod, expectedSnapshot, sourceNS)
		return err
	})
	return parent, snapshot, err
}

// runCanaryWithTransport owns a single disposable V1-to-V2 recovery trial and
// records acceptance only after byte comparison, remote reattachment and cleanup.
func runCanaryWithTransport(ctx context.Context, c client, source, run string, pools map[string]migrationPool, transport string) error {
	journalName, err := canaryJournalName(transport)
	if err != nil {
		return err
	}
	assets, err := canaryResources(run)
	if transport == "csi" {
		assets, err = csiCanaryResources(run)
	}
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err = (temporaryV2Activation{c: c, pools: pools}).accepted(ctx); err != nil {
		return err
	}
	s, err := canaryEnvironment(ctx, c, pools)
	if err != nil {
		return err
	}
	if err = requireBenchmarkBaseline(ctx, c, s.nodes); err != nil {
		return err
	}
	for _, volume := range s.volumes {
		if text(volume, "spec", "dataEngine") != "v1" {
			return errors.New("the recovery canary must precede application migration")
		}
	}
	bsl, err := c.get(ctx, "backupstoragelocations.velero.io", "velero", "default", false)
	if err != nil || text(bsl, "status", "phase") != "Available" || text(bsl, "spec", "accessMode") == "ReadOnly" {
		return errors.New("the existing backup destination is unavailable for the recovery canary")
	}
	old, err := c.get(ctx, "configmap", "longhorn-system", journalName, true)
	if err != nil {
		return err
	}
	if old != nil {
		return errors.New("a prior canary journal exists; preserve its resources and diagnose before explicit recovery")
	}
	p := canaryRun{c: c, source: source, run: run, record: canaryRecord{Version: 2, Source: source, Run: run, Transport: transport, Phase: "created", Image: fioImage, Workload: canaryWorkload(), Resources: map[string]benchmarkResource{}, CSIScratch: map[string]csiScratchEvidence{}}}
	body, _ := json.Marshal(p.record)
	journal, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": journalName, "namespace": "longhorn-system"}, "data": object{"record": string(body)}})
	if err != nil {
		return err
	}
	p.journalUID = text(journal, "metadata", "uid")
	created := map[string]map[string]object{"source": {}, "target": {}}
	for i, desired := range assets[:8] {
		group := "source"
		if i >= 5 {
			group = "target"
		}
		kind := text(desired, "kind")
		o, err := p.create(ctx, group+"/"+kind, desired)
		if err != nil {
			return err
		}
		created[group][kind] = o
	}
	sourceNS, targetNS := "longhorn-canary-v1-"+run, "longhorn-canary-v2-"+run
	sourceBound, sourcePV, sourceVolume, err := p.binding(ctx, sourceNS, "lh-canary-v1-"+run, "v1", "prod-worker-3", text(created["source"]["Pod"], "metadata", "uid"))
	if err != nil {
		return err
	}
	for kind, o := range map[string]object{"PersistentVolume": sourcePV, "LonghornVolume": sourceVolume} {
		if err = p.remember(ctx, "source/"+kind, o); err != nil {
			return err
		}
	}
	p.record.Phase = "source-write-requested"
	if err = p.save(ctx); err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	freshSource, err := c.get(ctx, "pod", sourceNS, "fio", false)
	if err != nil {
		return err
	}
	if err = verifyCanaryPod(freshSource, text(sourceBound["Pod"], "metadata", "uid"), "prod-worker-3"); err != nil {
		return err
	}
	fmt.Println("Canary storage provisioned with three healthy copies; writing the disposable source payload.")
	if _, err = c.kubectl(ctx, nil, "exec", "--namespace="+sourceNS, "fio", "--container=fio", "--", "/bin/sh", "-ec", "touch /tmp/canary-start"); err != nil {
		return err
	}
	if err = waitReady(ctx, func(ctx context.Context) error {
		logs, err := c.kubectl(ctx, nil, "logs", "--namespace="+sourceNS, "fio", "--container=fio")
		if err != nil || strings.TrimSpace(string(logs)) != "CANARY_READY" {
			return errors.New("source canary has not completed its verified write")
		}
		return nil
	}); err != nil {
		return err
	}
	p.record.PayloadSHA256, err = p.verifyBytes(ctx, sourceBound["Pod"], "prod-worker-3", "")
	if err != nil {
		return err
	}
	if _, err = p.create(ctx, "Modifier", assets[8]); err != nil {
		return err
	}
	if transport == "csi" {
		p.sourceClaim, p.sourceVolume = sourceBound["PersistentVolumeClaim"], sourceVolume
		p.snapshotClass, err = p.create(ctx, "SnapshotClass", assets[11])
		if err != nil {
			return err
		}
		p.csi = newCSICapture(run)
		if err = p.csi.start(ctx, c); err != nil {
			return err
		}
		defer p.csi.stop()
	}
	backup, snapshot, err := p.transfer(ctx, assets[9], sourceBound["Pod"], "", sourceNS)
	if err != nil {
		return err
	}
	p.record.Snapshot, p.record.Phase = snapshot, "backup-verified"
	if err = p.save(ctx); err != nil {
		return err
	}
	if p.csi != nil {
		if err = p.waitCSIUploadGone(ctx); err != nil {
			return err
		}
	}
	fmt.Println("Canary backup data transfer verified; restoring onto an independent V2 volume.")
	restore, _, err := p.transfer(ctx, assets[10], nil, snapshot, sourceNS)
	if err != nil {
		return err
	}
	restoredPod, err := c.get(ctx, "pod", targetNS, "fio", false)
	if err != nil {
		return err
	}
	targetBound, targetPV, targetVolume, err := p.binding(ctx, targetNS, "lh-canary-v2-"+run, "v2", "prod-worker-3", text(restoredPod, "metadata", "uid"))
	if err != nil {
		return err
	}
	if text(targetPV, "metadata", "uid") == text(sourcePV, "metadata", "uid") || text(targetVolume, "metadata", "uid") == text(sourceVolume, "metadata", "uid") {
		return errors.New("restore reused source storage instead of creating an independent V2 backend")
	}
	if p.csi != nil {
		if err = verifyCSIRestoredBinding(p.record.CSIScratch["DataDownload"], targetBound["PersistentVolumeClaim"], targetPV, targetVolume); err != nil {
			return err
		}
	}
	for kind, o := range targetBound {
		created["target"][kind] = o
		if err = p.remember(ctx, "restored/"+kind, o); err != nil {
			return err
		}
	}
	for kind, o := range map[string]object{"PersistentVolume": targetPV, "LonghornVolume": targetVolume} {
		if err = p.remember(ctx, "target/"+kind, o); err != nil {
			return err
		}
	}
	if _, err = p.verifyBytes(ctx, targetBound["Pod"], "prod-worker-3", p.record.PayloadSHA256); err != nil {
		return err
	}
	p.record.RestoreVerified, p.record.Phase = true, "restore-verified"
	if err = p.save(ctx); err != nil {
		return err
	}
	fmt.Println("Restored bytes match the source; checking persistence on another worker.")
	if err = p.reattach(ctx, created["target"], targetPV, targetVolume); err != nil {
		return err
	}
	p.record.ReattachVerified, p.record.Phase = true, "reattach-verified"
	if err = p.save(ctx); err != nil {
		return err
	}
	if p.csi != nil {
		if err = p.removeBackup(ctx, backup, restore, assets[8]); err != nil {
			return err
		}
		if err = p.waitCSIArtifactsGone(ctx); err != nil {
			return err
		}
	}
	if err = cleanupBenchmark(ctx, c, source, targetNS, created["target"], targetPV, targetVolume); err != nil {
		return err
	}
	if err = cleanupBenchmark(ctx, c, source, sourceNS, created["source"], sourcePV, sourceVolume); err != nil {
		return err
	}
	if p.csi == nil {
		if err = p.removeBackup(ctx, backup, restore, assets[8]); err != nil {
			return err
		}
	} else {
		if err = p.removeCSISnapshotClass(ctx); err != nil {
			return err
		}
		p.csi.stop()
		if err = p.csi.health(); err != nil {
			return err
		}
	}
	p.record.Cleaned, p.record.Phase, p.record.CompletedAt = true, "completed", time.Now().UTC().Format(time.RFC3339)
	if err = p.save(ctx); err != nil {
		return err
	}
	fmt.Println("V2 recovery canary passed: independent three-replica restore, original-byte comparison, remote-client reattachment and scratch cleanup verified.")
	return nil
}
