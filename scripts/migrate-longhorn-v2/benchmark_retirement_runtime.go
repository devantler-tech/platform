package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var benchmarkRetirementImage = regexp.MustCompile(`^ghcr\.io/devantler-tech/platform-longhorn-instance-manager@sha256:[a-f0-9]{64}$`)
var benchmarkContainerID = regexp.MustCompile(`^containerd://[a-f0-9]{64}$`)
var benchmarkDynamicVolume = regexp.MustCompile(`^pvc-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type benchmarkManagerEpoch struct {
	Pod, PodUID, ManagerUID, Node, ContainerID string
	StartedAt                                  time.Time
}

type benchmarkV1Runtime struct {
	Image, SettingUID, SettingVersion string
	Epochs                            []benchmarkManagerEpoch
}

func captureBenchmarkV1Runtime(ctx context.Context, c client, requiredNodes []string) (benchmarkV1Runtime, error) {
	setting, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", "default-instance-manager-image", false)
	if err != nil {
		return benchmarkV1Runtime{}, errors.New("the approved V1 retirement runtime must be deployed before creating scratch")
	}
	return observeBenchmarkV1Runtime(ctx, c, text(setting, "value"), requiredNodes)
}

func checkpointBenchmarkV1Backend(ctx context.Context, c client, record *benchmarkRecord, baseline benchmarkV1Runtime) error {
	requiredNodes := slices.Sorted(maps.Keys(record.Machines))
	verifyRuntime := func() error {
		current, err := observeBenchmarkV1Runtime(ctx, c, baseline.Image, requiredNodes)
		if err != nil || !reflect.DeepEqual(baseline, current) {
			return errors.New("the approved V1 runtime epochs changed since the benchmark started")
		}
		return nil
	}
	if !activeBenchmarkV1FenceOwner(c, *record) {
		return errors.New("only the active benchmark may checkpoint this V1 backend")
	}
	if err := verifyRuntime(); err != nil {
		return err
	}
	policies, err := benchmarkRetirementFences(*record)
	if err != nil {
		return err
	}
	var backend object
	identity := record.Resources["LonghornVolume"]
	if err := waitReady(ctx, func(ctx context.Context) error {
		current, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", identity.Name, false)
		if err != nil || benchmarkIdentity(current) != identity || text(current, "spec", "dataEngine") != "v1" ||
			text(current, "spec", "accessMode") != "rwo" || value(current, "metadata", "deletionTimestamp") != nil {
			return errors.New("original benchmark V1 backend identity changed")
		}
		if err := verifyBenchmarkDetachedBackendState(current); err != nil {
			return err
		}
		if err := verifyRecoveredBackendCascadeCleared(current); err != nil {
			return err
		}
		backend = current
		return nil
	}); err != nil {
		return err
	}
	if err := installBenchmarkReferenceFence(ctx, c, record, policies[1]); err != nil {
		return err
	}
	if err := proveBenchmarkReferenceFenceOnServingAPIs(ctx, c, *record, backend); err != nil {
		return err
	}
	if err := proveAndCheckpointBenchmarkInstances(ctx, c, record); err != nil {
		return err
	}
	if err := verifyRuntime(); err != nil {
		return err
	}
	for _, epoch := range baseline.Epochs {
		if err := sourceCurrent(ctx, c.run, record.Source); err != nil {
			return err
		}
		if err := requestBenchmarkV1Seal(ctx, c, *record, epoch); err != nil {
			return err
		}
	}
	return verifyRuntime()
}

func observeBenchmarkV1Runtime(ctx context.Context, c client, image string, requiredNodes []string) (benchmarkV1Runtime, error) {
	if !benchmarkRetirementImage.MatchString(image) {
		return benchmarkV1Runtime{}, errors.New("an immutable V1 retirement image is required")
	}
	beforeUID, beforeVersion, err := readBenchmarkDefaultManagerImage(ctx, c, image)
	if err != nil {
		return benchmarkV1Runtime{}, err
	}
	managers, err := c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return benchmarkV1Runtime{}, err
	}
	pods, err := c.list(ctx, "pods", "longhorn-system")
	if err != nil {
		return benchmarkV1Runtime{}, err
	}
	epochs, err := qualifyV1RetirementManagerEpochs(image, requiredNodes, managers, pods)
	if err != nil {
		return benchmarkV1Runtime{}, err
	}
	afterUID, afterVersion, err := readBenchmarkDefaultManagerImage(ctx, c, image)
	if err != nil || afterUID != beforeUID || afterVersion != beforeVersion {
		return benchmarkV1Runtime{}, errors.New("the V1 default instance manager changed during observation")
	}
	return benchmarkV1Runtime{Image: image, SettingUID: beforeUID, SettingVersion: beforeVersion, Epochs: epochs}, nil
}

func readBenchmarkDefaultManagerImage(ctx context.Context, c client, image string) (uid, version string, err error) {
	const name = "default-instance-manager-image"
	const namespace = "longhorn-system"
	setting, err := c.get(ctx, "settings.longhorn.io", namespace, name, false)
	if err != nil || text(setting, "metadata", "namespace") != namespace ||
		text(setting, "metadata", "name") != name || value(setting, "metadata", "deletionTimestamp") != nil ||
		text(setting, "value") != image {
		return "", "", errors.New("the selected V1 default instance manager is unverified")
	}
	return text(setting, "metadata", "uid"), text(setting, "metadata", "resourceVersion"), nil
}

// The caller must recheck current main, ownership, active admission and the
// observed epoch on both sides of this request. Its reply is runtime evidence
// for this Pod alone, and does not authorize deletion.
func requestBenchmarkV1Seal(ctx context.Context, c client, record benchmarkRecord, epoch benchmarkManagerEpoch) error {
	pv := record.Resources["PersistentVolume"]
	backend := record.Resources["LonghornVolume"]
	claim := record.Resources["PersistentVolumeClaim"]
	if c.context != "admin@prod" || record.Phase != "v1" || verifyRetirementReceipts(record) != nil ||
		!benchmarkDynamicVolume.MatchString(backend.Name) || pv.Name != backend.Name ||
		backend.Name != "pvc-"+claim.UID || !strings.HasPrefix(epoch.Pod, "instance-manager-") ||
		safeKubeRequest("pod", "longhorn-system", epoch.Pod, false) != nil {
		return errors.New("the original dynamic V1 benchmark volume is unverified")
	}
	reply, err := c.kubectl(ctx, nil, "exec", "--namespace=longhorn-system", epoch.Pod, "--container=instance-manager",
		"--", "/usr/local/bin/longhorn-instance-manager", "process", "seal-volume-for-retirement", "--volume", backend.Name)
	if err != nil || string(reply) != "SEALED\n" {
		return errors.New("V1 benchmark volume has no complete native retirement seal")
	}
	return nil
}

// Use with the separately verified default V1 image and manager deployment.
// Longhorn v1.13.0 places new V1 instances only in its selected default image.
// Other image generations can continue to host previously attached volumes.
func qualifyV1RetirementManagerEpochs(image string, requiredNodes []string, managers, pods []object) ([]benchmarkManagerEpoch, error) {
	if !benchmarkRetirementImage.MatchString(image) || len(requiredNodes) == 0 || managers == nil || pods == nil {
		return nil, errors.New("complete V1 retirement runtime inputs are required")
	}
	managerByName, err := indexed(managers)
	if err != nil {
		return nil, err
	}
	podByName, err := indexed(pods)
	if err != nil {
		return nil, err
	}

	epochs := make([]benchmarkManagerEpoch, 0, len(managers))
	selected := map[string]bool{}
	for name, manager := range managerByName {
		if text(manager, "spec", "image") != image || text(manager, "spec", "dataEngine") == "v2" {
			continue
		}
		node := text(manager, "spec", "nodeID")
		if !kubeNamespace.MatchString(node) || selected[node] ||
			safeKubeRequest("pod", "longhorn-system", name, false) != nil ||
			text(manager, "metadata", "namespace") != "longhorn-system" ||
			text(manager, "spec", "dataEngine") != "v1" || text(manager, "spec", "type") != "aio" ||
			text(manager, "status", "currentState") != "running" {
			return nil, errors.New("default V1 instance manager selection is ambiguous")
		}
		epoch, err := verifyBenchmarkManagerPod(podByName[name], manager, image)
		if err != nil {
			return nil, err
		}
		selected[node] = true
		delete(podByName, name)
		epochs = append(epochs, epoch)
	}
	for _, pod := range podByName {
		containers, _ := value(pod, "spec", "containers").([]any)
		for _, entry := range containers {
			container, _ := entry.(object)
			if text(container, "image") == image && text(managerByName[text(pod, "metadata", "name")], "spec", "dataEngine") != "v2" {
				return nil, errors.New("unpaired V1 retirement runtime Pod")
			}
		}
	}
	for _, node := range requiredNodes {
		if !selected[node] {
			return nil, errors.New("benchmark node lacks the approved V1 retirement runtime")
		}
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i].Pod < epochs[j].Pod })
	return epochs, nil
}

func verifyBenchmarkManagerPod(pod, manager object, image string) (benchmarkManagerEpoch, error) {
	name := text(manager, "metadata", "name")
	epoch := benchmarkManagerEpoch{
		Pod: name, PodUID: text(pod, "metadata", "uid"),
		ManagerUID: text(manager, "metadata", "uid"), Node: text(manager, "spec", "nodeID"),
	}
	refs, _ := value(pod, "metadata", "ownerReferences").([]any)
	containers, _ := value(pod, "spec", "containers").([]any)
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if text(pod, "metadata", "namespace") != "longhorn-system" ||
		text(pod, "spec", "nodeName") != epoch.Node || text(pod, "spec", "restartPolicy") != "Never" ||
		value(pod, "spec", "hostPID") == true || value(pod, "spec", "shareProcessNamespace") == true ||
		text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
		len(refs) != 1 || len(containers) != 1 || len(statuses) != 1 {
		return benchmarkManagerEpoch{}, errors.New("retirement manager Pod identity or readiness is incomplete")
	}
	owner, _ := refs[0].(object)
	container, _ := containers[0].(object)
	running, _ := statuses[0].(object)
	if text(owner, "kind") != "InstanceManager" || text(owner, "apiVersion") != "longhorn.io/v1beta2" ||
		text(owner, "name") != name || text(owner, "uid") != epoch.ManagerUID || value(owner, "controller") != true ||
		text(container, "name") != "instance-manager" || text(container, "image") != image ||
		value(container, "securityContext", "privileged") != true ||
		value(container, "securityContext", "runAsUser") != float64(0) ||
		value(container, "securityContext", "runAsGroup") != float64(0) ||
		value(container, "securityContext", "runAsNonRoot") == true ||
		text(running, "name") != "instance-manager" ||
		strings.TrimPrefix(text(running, "imageID"), "docker-pullable://") != image ||
		value(running, "ready") != true || value(running, "started") != true ||
		value(running, "restartCount") != float64(0) || !benchmarkManagerHasDurableLog(pod, container) {
		return benchmarkManagerEpoch{}, errors.New("retirement manager container, image or durable marker mount is unverified")
	}
	epoch.ContainerID = text(running, "containerID")
	var err error
	epoch.StartedAt, err = time.Parse(time.RFC3339Nano, text(running, "state", "running", "startedAt"))
	if err != nil || epoch.StartedAt.IsZero() || !benchmarkContainerID.MatchString(epoch.ContainerID) {
		return benchmarkManagerEpoch{}, errors.New("retirement manager container epoch is unverified")
	}
	return epoch, nil
}

func benchmarkManagerHasDurableLog(pod, container object) bool {
	mounts, _ := value(container, "volumeMounts").([]any)
	volumes, _ := value(pod, "spec", "volumes").([]any)
	matches := 0
	for _, entry := range mounts {
		mount, _ := entry.(object)
		if text(mount, "mountPath") != "/log" {
			continue
		}
		if text(mount, "name") != "log" || value(mount, "readOnly") == true ||
			!absentOrEmptyString(value(mount, "subPath")) || !absentOrEmptyString(value(mount, "subPathExpr")) {
			return false
		}
		matches++
	}
	if matches != 1 {
		return false
	}
	matches = 0
	for _, entry := range volumes {
		volume, _ := entry.(object)
		if text(volume, "name") != "log" {
			continue
		}
		if text(volume, "hostPath", "path") != "/var/lib/longhorn/logs" ||
			text(volume, "hostPath", "type") != "DirectoryOrCreate" {
			return false
		}
		matches++
	}
	return matches == 1
}
