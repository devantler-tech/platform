package main

import _ "embed"

const (
	corootKeeperCopyLabel      = "storage.platform.devantler.tech/coroot-keeper-copy-run"
	corootKeeperCopyPrefix     = "coroot-keeper-copy-"
	corootKeeperStagePodPrefix = "coroot-keeper-stage-"
	corootKeeperCopyGatePrefix = "coroot-keeper-copy-gate-"
	corootKeeperCopyVerified   = "COROOT_KEEPER_OFFLINE_COPY=VERIFIED"
)

//go:embed coroot_keeper_copy_payload.sh
var corootKeeperCopyPayload string

func corootKeeperStagingClaim(run, source string) object {
	return object{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{
			"name": corootKeeperStagingPrefix + run, "namespace": corootKeeperNamespace,
			"annotations": object{corootKeeperFenceOwner: run + ":" + source},
		},
		"spec": object{
			"storageClassName": "longhorn-v2", "accessModes": []any{"ReadWriteOnce"},
			"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": corootKeeperDataSize}},
		},
	}
}

func corootKeeperCopyPod(run, source, original, hostname string) object {
	const wait = `set -eu
printf '%s\n' 'COROOT_KEEPER_OFFLINE_COPY=WAITING_FOR_ISOLATION'
# Allow for the controller's release window and kubelet projection delay.
for ((n=0; n<420; n++)); do
    if [[ $(< /gate/state) == "$1" ]]; then break; fi
    sleep 1
done
[[ $(< /gate/state) == "$1" ]]
`
	pod := corootKeeperVolumePod(run, source, corootKeeperCopyPrefix+run, hostname)
	spec := valueObject(pod, "spec")
	spec["activeDeadlineSeconds"] = float64(900)
	security := valueObject(spec, "securityContext")
	// Keep the namespace's policy default without requesting a group ownership change.
	delete(security, "fsGroup")
	mounts := []any{
		object{"name": "original", "mountPath": "/original", "readOnly": true},
		object{"name": "destination", "mountPath": "/destination"},
		object{"name": "gate", "mountPath": "/gate", "readOnly": true},
	}
	spec["volumes"] = append(value(spec, "volumes").([]any),
		object{"name": "original", "persistentVolumeClaim": object{"claimName": original, "readOnly": true}},
		object{"name": "gate", "configMap": object{"name": corootKeeperCopyGatePrefix + run, "defaultMode": float64(292)}})
	spec["initContainers"] = []any{corootKeeperVolumeContainer("copy", []any{"bash", "-c", wait + corootKeeperCopyPayload, "coroot-keeper-copy", run + ":" + source}, mounts)}
	return pod
}

func corootKeeperStagePod(run, source, hostname string) object {
	return corootKeeperVolumePod(run, source, corootKeeperStagePodPrefix+run, hostname)
}

func corootKeeperVolumePod(run, source, name, hostname string) object {
	const inspect = `set -euo pipefail
test -w /destination
test -d /destination
free=$(df -PB1 -- /destination | awk 'NR==2 {print $4}')
[[ "$free" =~ ^[0-9]+$ ]]
printf 'COROOT_KEEPER_DESTINATION=READY bytes=%s\n' "$free"
sleep 300
`
	return object{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": object{
			"name": name, "namespace": corootKeeperNamespace,
			"labels":      object{corootKeeperCopyLabel: run},
			"annotations": object{corootKeeperFenceOwner: run + ":" + source},
		},
		"spec": object{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"enableServiceLinks": false, "hostUsers": false, "activeDeadlineSeconds": float64(720),
			"securityContext": object{
				"runAsNonRoot": true, "runAsUser": float64(65534), "runAsGroup": float64(65534),
				"fsGroup": float64(65534), "fsGroupChangePolicy": "OnRootMismatch",
				"seccompProfile": object{"type": "RuntimeDefault"},
			},
			"affinity": object{"nodeAffinity": object{"requiredDuringSchedulingIgnoredDuringExecution": object{
				"nodeSelectorTerms": []any{object{"matchExpressions": []any{object{
					"key": "kubernetes.io/hostname", "operator": "In", "values": []any{hostname},
				}}}},
			}}},
			"volumes": []any{object{"name": "destination", "persistentVolumeClaim": object{"claimName": corootKeeperStagingPrefix + run}}},
			"containers": []any{corootKeeperVolumeContainer("verify", []any{"bash", "-ec", inspect}, []any{
				object{"name": "destination", "mountPath": "/destination"},
			})},
		},
	}
}

func corootKeeperVolumeContainer(name string, command, mounts []any) object {
	return object{
		"name": name, "image": corootKeeperImageID, "imagePullPolicy": "IfNotPresent", "command": command,
		"securityContext": object{
			"runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"seLinuxOptions": object{},
			"capabilities":   object{"drop": []any{"ALL"}}, "seccompProfile": object{"type": "RuntimeDefault"},
		},
		"volumeMounts": mounts,
		"resources": object{
			"requests": object{"cpu": "50m", "memory": "32Mi"},
			"limits":   object{"cpu": "1", "memory": "128Mi"},
		},
	}
}

var corootKeeperCopyIsolation = storageCopyIsolationScope{
	Namespace: corootKeeperNamespace, Label: corootKeeperCopyLabel, Prefix: "coroot-keeper-copy-deny-",
}

func corootKeeperCopyNetworkPolicy(run, source string) object {
	return corootKeeperCopyIsolation.denyPolicy(run, source, corootKeeperFenceOwner)
}
