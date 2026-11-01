package main

import (
	"errors"
	"regexp"
	"time"
)

// benchmarkDeadlineSeconds bounds the fio Pod from its creation: eighteen 40-second trials
// and the preparation pass, with room for slow storage.
const benchmarkDeadlineSeconds = 1500

// benchmarkSampleInterval spaces storage health reads during the trials. Each read lists
// every Longhorn volume, replica and engine.
var benchmarkSampleInterval = time.Minute

const benchmarkUnprovenSamples = 3

// benchmarkCompletionLimit lets the Pod report Failed at its deadline rather than cutting it short.
const benchmarkCompletionLimit = (benchmarkDeadlineSeconds + 120) * time.Second

// The linux/amd64 manifest of the fio 3.41 image used by longhorn-tests.
const fioImage = "docker.io/openeuler/fio:3.41-oe2403sp2@sha256:335e14c142c65c8da4f83172d53387af7c9cd8336910107a4504ba3a5ddbba7f"

const fioScript = `set -eu
while [ ! -f /tmp/benchmark-start ]; do sleep 2; done
fio --name=prepare --filename=/data/io.bin --size=2G --rw=write --bs=1M --ioengine=libaio --direct=1 --end_fsync=1 --output=/tmp/prepare.json --output-format=json
throttled() {
  while read -r key amount; do
    if [ "$key" = throttled_usec ]; then printf '%s' "$amount"; return; fi
  done < /sys/fs/cgroup/cpu.stat
  return 1
}
for profile in randread randwrite randrw; do
  for depth in 1 32; do
    for trial in 1 2 3; do
      before=$(throttled)
      fio --name="$profile-$depth-$trial" --filename=/data/io.bin --size=2G --rw="$profile" --rwmixread=70 --bs=4k --ioengine=libaio --direct=1 --iodepth="$depth" --numjobs=1 --thread=1 --time_based=1 --runtime=30 --ramp_time=10 --randrepeat=1 --randseed=5582 --group_reporting=1 --eta=never --output-format=json --output=/tmp/fio.json
      after=$(throttled)
      printf '{"throttled_usec":%s,"fio":' "$((after-before))"
      cat /tmp/fio.json
      printf '}\n'
    done
  done
done
`

func benchmarkResources(phase, runID, tag string) ([]object, error) {
	if (phase != "v1" && phase != "v2") || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(runID) || tag != "longhorn-benchmark-"+phase+"-"+runID {
		return nil, errors.New("unreviewed benchmark identity")
	}
	namespace, class := tag, "lh-benchmark-"+phase+"-"+runID
	meta := func(name string, namespaced bool) object {
		m := object{"name": name, "labels": object{"storage.platform.devantler.tech/benchmark-run": runID}}
		if namespaced {
			m["namespace"] = namespace
		}
		return m
	}
	containerSecurity := object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"capabilities": object{"drop": []any{"ALL"}}, "seccompProfile": object{"type": "RuntimeDefault"}}
	nsMeta := meta(namespace, false)
	nsMeta["labels"].(object)["pod-security.kubernetes.io/enforce"] = "restricted"
	nsMeta["labels"].(object)["pod-security.kubernetes.io/enforce-version"] = "latest"
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": nsMeta},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta("default-deny", true),
			"spec": object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}},
		{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": meta(class, false),
			"provisioner": "driver.longhorn.io", "reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer",
			"allowVolumeExpansion": false, "parameters": object{"numberOfReplicas": "3", "staleReplicaTimeout": "30", "fsType": "ext4",
				"dataLocality": "disabled", "unmapMarkSnapChainRemoved": "ignored", "disableRevisionCounter": "true", "dataEngine": phase,
				"nodeSelector": tag, "backupTargetName": "default"}},
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("fio-data", true),
			"spec": object{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": class,
				"resources": object{"requests": object{"storage": "4Gi"}}}},
		{"apiVersion": "v1", "kind": "Pod", "metadata": meta("fio", true), "spec": object{
			"nodeSelector": object{"kubernetes.io/hostname": "prod-worker-3"}, "restartPolicy": "Never", "hostUsers": false,
			"automountServiceAccountToken": false, "enableServiceLinks": false, "activeDeadlineSeconds": benchmarkDeadlineSeconds,
			"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
				"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": object{"type": "RuntimeDefault"}},
			"containers": []any{object{"name": "fio", "image": fioImage, "imagePullPolicy": "IfNotPresent",
				"command": []any{"/bin/sh", "-ec"}, "args": []any{fioScript}, "securityContext": containerSecurity,
				"resources":    object{"requests": object{"cpu": "1", "memory": "512Mi"}, "limits": object{"cpu": "2", "memory": "1Gi"}},
				"volumeMounts": []any{object{"name": "data", "mountPath": "/data"}, object{"name": "scratch", "mountPath": "/tmp"}}}},
			"volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "fio-data"}},
				object{"name": "scratch", "emptyDir": object{"sizeLimit": "32Mi"}}}}},
	}, nil
}
