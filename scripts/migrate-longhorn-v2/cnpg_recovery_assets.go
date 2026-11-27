package main

import (
	"errors"
	"regexp"
	"strings"
)

var recoveryRun = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var recoveryDigest = regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql@sha256:[0-9a-f]{64}$`)

// source comes only from bindCNPGRecoverySource; image comes from the exact
// observed source PostgreSQL process, not its mutable requested tag. This
// renders resources only. Creation, isolation, physical/byte verification and
// exact Retain cleanup must all succeed before any production claim changes.
// hostname must uniquely select the fresh Node UID/boot qualified by the caller
// before Cluster creation; this selector alone does not establish that binding.
func cnpgRecoveryResources(run string, source cnpgRecoverySource, image string, barrier postgresState, hostname string) ([]object, error) {
	if !recoveryRun.MatchString(run) || !recoveryDigest.MatchString(image) || !recoveryImage.MatchString(source.Image) || validPostgresState(barrier) != nil || barrier.InRecovery || source.ClusterUID == "" || source.StoreUID == "" || source.BackupUID == "" || !recoveryBackupID.MatchString(source.BackupID) ||
		!knownCNPGArchiveSource(source.Namespace, source.Cluster) || !recoveryEndpoint.MatchString(source.Endpoint) || !recoverySize.MatchString(source.Size) || len(hostname) > 63 || !kubeName.MatchString(hostname) {
		return nil, errors.New("isolated recovery lacks a bound source, immutable process image or primary WAL barrier")
	}
	// CNPG 1.30 requires a version tag even with an immutable manifest digest.
	// Keep the exact source tag for admission/version detection and its observed
	// repository-qualified digest for executable identity.
	_, digest, _ := strings.Cut(image, "@")
	pinnedImage := source.Image + "@" + digest
	ns := "longhorn-db-proof-" + source.Cluster + "-" + run
	metadata := func(name, namespace string) object {
		m := object{"name": name, "labels": object{"storage.platform.devantler.tech/recovery-run": run, "app.kubernetes.io/managed-by": "github-actions"}}
		if namespace != "" {
			m["namespace"] = namespace
		}
		return m
	}
	namespace := metadata(ns, "")
	namespace["labels"].(object)["pod-security.kubernetes.io/enforce"] = "restricted"
	deny := []any{}
	for _, production := range cnpgSources() {
		deny = append(deny, object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": production.Namespace}}}})
	}
	credentials := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		credentials[field] = object{"name": "archive-read", "key": key}
	}
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": namespace},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": metadata("restore-isolation", ns), "spec": object{
			"endpointSelector": object{}, "enableDefaultDeny": object{"ingress": true, "egress": true}, "egressDeny": deny,
			// Same exact control-plane source and ports as the platform's
			// generated allow-cnpg-operator rule. This platform-owned temporary
			// namespace does not rely on asynchronous policy generation.
			"ingress": []any{object{
				"fromEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "cnpg-system", "k8s:app.kubernetes.io/name": "cloudnative-pg", "k8s:app.kubernetes.io/instance": "cloudnative-pg"}}},
				"toPorts":       []any{object{"ports": []any{object{"port": "8000", "protocol": "TCP"}, object{"port": "5432", "protocol": "TCP"}}}},
			}},
			"egress": []any{
				object{"toEntities": []any{"kube-apiserver"}},
				object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "UDP"}, object{"port": "53", "protocol": "TCP"}}, "rules": object{"dns": []any{object{"matchPattern": "*"}}}}}},
				object{"toFQDNs": []any{object{"matchName": strings.TrimPrefix(source.Endpoint, "https://")}}, "toPorts": []any{object{"ports": []any{object{"port": "443", "protocol": "TCP"}}}}},
			},
		}},
		{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": metadata("restore-source", ns), "spec": object{"configuration": object{
			"destinationPath": source.Destination, "endpointURL": source.Endpoint, "s3Credentials": credentials, "wal": object{"compression": "gzip"}, "data": object{"compression": "gzip"},
		}}},
		{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": metadata("restore", ns), "spec": object{
			"instances": 1, "imageName": pinnedImage, "enableSuperuserAccess": false, "enablePDB": false,
			"affinity": object{"nodeSelector": object{"kubernetes.io/hostname": hostname}},
			// CNPG 1.30.2 INCLUDE_PLUGINS defaulting skips names already
			// present, including explicitly disabled entries. Keep this entry
			// parameterless. The Barman configuration reads the source for
			// external recovery independently of the WAL archive settings.
			// The caller must still check the server-admitted projection for
			// unexpected enabled plugins before creating the Cluster.
			"plugins":          []any{object{"name": "barman-cloud.cloudnative-pg.io", "enabled": false, "isWALArchiver": false}},
			"storage":          object{"size": source.Size, "storageClass": "longhorn-v2"},
			"resources":        object{"requests": object{"cpu": "50m", "memory": "256Mi"}, "limits": object{"cpu": "1", "memory": "1Gi"}},
			"bootstrap":        object{"recovery": object{"source": "archive", "recoveryTarget": object{"backupID": source.BackupID, "targetTLI": barrier.Timeline, "targetLSN": barrier.FlushLSN}}},
			"externalClusters": []any{object{"name": "archive", "plugin": object{"name": "barman-cloud.cloudnative-pg.io", "enabled": true, "parameters": object{"barmanObjectName": "restore-source", "serverName": source.Server}}}},
		}},
	}, nil
}
