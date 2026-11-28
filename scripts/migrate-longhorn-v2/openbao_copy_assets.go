package main

import (
	"context"
	_ "embed"
)

const (
	openBaoCopyImage      = "ghcr.io/coroot/clickhouse@sha256:7a7bced745cb7975ce8b249da1e08192faf8adbcc24aae45060ccc6d7c9187a3"
	openBaoCopyLabel      = "storage.platform.devantler.tech/openbao-copy-run"
	openBaoCopyGatePrefix = "lh-openbao-copy-gate-"
)

var openBaoCopyIsolation = storageCopyIsolationScope{
	Namespace: openBaoNamespace, Label: openBaoCopyLabel, Prefix: "lh-openbao-copy-deny-",
}

//go:embed openbao_copy_payload.sh
var openBaoCopyPayload string

func openBaoStagingClaims(run, source string) [2]object {
	var claims [2]object
	for i, kind := range [2]string{"data", "audit"} {
		size := [2]string{"10Gi", "40Gi"}[i]
		claims[i] = object{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": object{
				"name": "lh-openbao-staging-" + kind + "-" + run, "namespace": openBaoNamespace,
				"annotations": object{openBaoCopyFenceOwner: run + ":" + source},
			},
			"spec": object{
				"storageClassName": "longhorn-v2", "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
				"resources": object{"requests": object{"storage": size}},
			},
		}
	}
	return claims
}

func openBaoStagePod(run, source, hostname string) object {
	const inspect = `set -euo pipefail
for path in /destination-data /destination-audit; do
    test -w "$path"
    test "$(stat -c %g "$path")" = 1000
    entry=$(find "$path" -mindepth 1 -maxdepth 1 ! -name lost+found -print -quit)
    test -z "$entry"
done
test "$(stat -c %d /destination-data)" != "$(stat -c %d /destination-audit)"
free_data=$(df -PB1 -- /destination-data | awk 'NR==2 {print $4}')
free_audit=$(df -PB1 -- /destination-audit | awk 'NR==2 {print $4}')
[[ "$free_data" =~ ^[0-9]+$ && "$free_audit" =~ ^[0-9]+$ ]]
printf 'OPENBAO_DESTINATIONS=READY data=%s audit=%s\n' "$free_data" "$free_audit"
sleep 300
`
	pod, mounts := openBaoVolumePod(run, source, "lh-openbao-stage-"+run, hostname)
	spec := valueObject(pod, "spec")
	security := valueObject(spec, "securityContext")
	security["fsGroup"], security["fsGroupChangePolicy"] = float64(1000), "OnRootMismatch"
	spec["containers"] = []any{openBaoVolumeContainer("verify", []any{"bash", "-ec", inspect}, mounts)}
	return pod
}

func openBaoCopyPod(run, source, member, hostname string) object {
	const wait = `set -euo pipefail
printf '%s\n' 'OPENBAO_OFFLINE_COPY=WAITING_FOR_ISOLATION'
for ((n=0; n<420; n++)); do
    if [[ $(< /gate/state) == "$1" ]]; then break; fi
    sleep 1
done
[[ $(< /gate/state) == "$1" ]]
`
	pod, mounts := openBaoVolumePod(run, source, "lh-openbao-copy-"+run, hostname)
	spec := valueObject(pod, "spec")
	spec["activeDeadlineSeconds"] = float64(1200)
	volumes := value(spec, "volumes").([]any)
	for _, kind := range [2]string{"data", "audit"} {
		name := "source-" + kind
		volumes = append(volumes, object{"name": name,
			"persistentVolumeClaim": object{"claimName": kind + "-" + member, "readOnly": true}})
		mounts = append(mounts, object{"name": name, "mountPath": "/" + name, "readOnly": true})
	}
	spec["volumes"] = append(volumes, object{"name": "gate", "configMap": object{
		"name": openBaoCopyGatePrefix + run, "defaultMode": float64(292),
	}})
	mounts = append(mounts, object{"name": "gate", "mountPath": "/gate", "readOnly": true})
	spec["initContainers"] = []any{openBaoVolumeContainer("copy",
		[]any{"bash", "-c", wait + openBaoCopyPayload, "openbao-copy", run + ":" + source}, mounts)}
	spec["containers"] = []any{openBaoVolumeContainer("verify", []any{"sleep", "300"}, nil)}
	return pod
}

func openBaoVolumePod(run, source, name, hostname string) (object, []any) {
	claims := openBaoStagingClaims(run, source)
	volumes, mounts := make([]any, len(claims)), make([]any, len(claims))
	for i, kind := range [2]string{"data", "audit"} {
		name := "destination-" + kind
		volumes[i] = object{"name": name, "persistentVolumeClaim": object{"claimName": text(claims[i], "metadata", "name")}}
		mounts[i] = object{"name": name, "mountPath": "/" + name}
	}
	pod := object{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": object{
			"name": name, "namespace": openBaoNamespace,
			"labels":      object{openBaoCopyLabel: run},
			"annotations": object{openBaoCopyFenceOwner: run + ":" + source},
		},
		"spec": object{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"enableServiceLinks": false, "hostUsers": false, "activeDeadlineSeconds": float64(720),
			"securityContext": object{
				"runAsNonRoot": true, "runAsUser": float64(100), "runAsGroup": float64(1000),
				"seccompProfile": object{"type": "RuntimeDefault"},
			},
			"affinity": object{"nodeAffinity": object{"requiredDuringSchedulingIgnoredDuringExecution": object{
				"nodeSelectorTerms": []any{object{"matchExpressions": []any{object{
					"key": "kubernetes.io/hostname", "operator": "In", "values": []any{hostname},
				}}}},
			}}},
			"volumes": volumes,
		},
	}
	return pod, mounts
}

func openBaoCopyGate(run, source string) object {
	return object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{
			"name": openBaoCopyGatePrefix + run, "namespace": openBaoNamespace,
			"annotations": object{openBaoCopyFenceOwner: run + ":" + source},
		},
		"data": object{"state": "hold"},
	}
}

func openBaoVolumeContainer(name string, command, mounts []any) object {
	container := object{
		"name": name, "image": openBaoCopyImage, "imagePullPolicy": "IfNotPresent", "command": command,
		"securityContext": object{
			"runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"seLinuxOptions": object{}, "capabilities": object{"drop": []any{"ALL"}},
			"seccompProfile": object{"type": "RuntimeDefault"},
		},
		"resources": object{
			"requests": object{"cpu": "100m", "memory": "64Mi"},
			"limits":   object{"cpu": "1", "memory": "256Mi"},
		},
	}
	if len(mounts) > 0 {
		container["volumeMounts"] = mounts
	}
	return container
}

func (c client) matchBoundOpenBaoVolumePod(ctx context.Context, pod, created, desired object, hostname string) bool {
	expected, err := c.expectedCopyPodTopology(ctx, pod, desired, hostname)
	return err == nil && matchStorageCopyVolumePod(pod, created, expected, openBaoNamespace, openBaoCopyFenceOwner)
}
