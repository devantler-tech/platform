package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestDatabaseRecoveryIsIsolatedV2AndPinsItsOriginalArchiveAndWAL(t *testing.T) {
	for _, reviewed := range cnpgArchiveSources() {
		c, store, backup := recoverySourceFixture(reviewed.Name)
		source, err := bindCNPGRecoverySource(c, store, backup, text(backup, "metadata", "uid"), archiveBindingFixture(c, store))
		if err != nil {
			t.Fatal(err)
		}
		image := "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64)
		barrier := postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/100"}
		assets, err := cnpgRecoveryResources("1234", source, image, barrier, "qualified-worker")
		if err != nil || len(assets) != 4 {
			t.Fatalf("recovery assets: %v", err)
		}
		ns, policy, archive, cluster := assets[0], assets[1], assets[2], assets[3]
		if !reflect.DeepEqual(value(cluster, "spec", "affinity"), object{"nodeSelector": object{"kubernetes.io/hostname": "qualified-worker"}}) {
			t.Fatal("recovery could schedule outside the Node qualified before Cluster creation")
		}
		if text(ns, "metadata", "name") == source.Namespace || text(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" ||
			text(cluster, "metadata", "namespace") != text(ns, "metadata", "name") || value(cluster, "spec", "instances") != 1 || text(cluster, "spec", "storage", "storageClass") != "longhorn-v2" || text(cluster, "spec", "imageName") != source.Image+"@"+strings.Split(image, "@")[1] {
			t.Fatal("recovery was not independent, restricted and bound to V2 and the exact source image")
		}
		external := value(cluster, "spec", "externalClusters").([]any)[0].(object)
		if text(external, "plugin", "parameters", "serverName") != source.Server || text(external, "plugin", "parameters", "barmanObjectName") != text(archive, "metadata", "name") ||
			text(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID") != source.BackupID || text(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "targetTLI") != "7" || text(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "targetLSN") != "2/100" {
			t.Fatal("restore defaulted to a new archive server, an arbitrary backup or latest timeline")
		}
		// CNPG's INCLUDE_PLUGINS defaulting skips a name only when an entry
		// already exists. Omitting this entry can enable WAL archiving against
		// the source archive even though recovery itself is read-only.
		if !reflect.DeepEqual(value(cluster, "spec", "plugins"), []any{object{"name": "barman-cloud.cloudnative-pg.io", "enabled": false, "isWALArchiver": false}}) || value(external, "plugin", "enabled") != true {
			t.Fatal("restore does not explicitly disable cluster archiving while enabling its external recovery source")
		}
		if value(archive, "spec", "retentionPolicy") != nil || text(archive, "spec", "configuration", "destinationPath") != source.Destination || text(archive, "spec", "configuration", "endpointURL") != source.Endpoint {
			t.Fatal("isolated recovery could archive or apply retention to the source")
		}
		ingress := value(policy, "spec", "ingress").([]any)
		denied := value(policy, "spec", "egressDeny").([]any)
		if value(policy, "spec", "enableDefaultDeny", "ingress") != true || value(policy, "spec", "enableDefaultDeny", "egress") != true || len(ingress) != 1 || len(denied) != len(cnpgSources()) {
			t.Fatal("recovery omitted explicit default deny or a production database namespace fence")
		}
		for i, production := range cnpgSources() {
			peers := value(denied[i].(object), "toEndpoints").([]any)
			if len(peers) != 1 || text(peers[0].(object), "matchLabels", "k8s:io.kubernetes.pod.namespace") != production.Namespace {
				t.Fatal("recovery omitted a production database namespace")
			}
		}
		from := value(ingress[0].(object), "fromEndpoints").([]any)
		ports := value(ingress[0].(object), "toPorts").([]any)
		if len(from) != 1 || len(ports) != 1 || len(value(ports[0].(object), "ports").([]any)) != 2 || text(from[0].(object), "matchLabels", "k8s:io.kubernetes.pod.namespace") != "cnpg-system" || text(from[0].(object), "matchLabels", "k8s:app.kubernetes.io/name") != "cloudnative-pg" || text(from[0].(object), "matchLabels", "k8s:app.kubernetes.io/instance") != "cloudnative-pg" {
			t.Fatal("recovery ingress is not restricted to the native CNPG operator path")
		}
		if !reflect.DeepEqual(value(ports[0].(object), "ports"), []any{object{"port": "8000", "protocol": "TCP"}, object{"port": "5432", "protocol": "TCP"}}) {
			t.Fatal("recovery changed the operator's exact TCP ports")
		}
	}
}

func TestRecoveryRenderingCannotInventAnImageOrHistory(t *testing.T) {
	c, store, backup := recoverySourceFixture("umami-db")
	source, err := bindCNPGRecoverySource(c, store, backup, text(backup, "metadata", "uid"), archiveBindingFixture(c, store))
	if err != nil {
		t.Fatal(err)
	}
	barrier := postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/100"}
	image := "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64)
	for _, hostname := range []string{"", "bad/hostname", "-hostname", strings.Repeat("a", 64)} {
		if resources, err := cnpgRecoveryResources("1234", source, image, barrier, hostname); err == nil || resources != nil {
			t.Fatal("missing or invalid selected hostname cleared recovery rendering")
		}
	}
	for _, hostname := range []string{"qualified-worker", "another-worker"} {
		resources, err := cnpgRecoveryResources("1234", source, image, barrier, hostname)
		if err != nil || text(resources[3], "spec", "affinity", "nodeSelector", "kubernetes.io/hostname") != hostname {
			t.Fatal("renderer did not preserve its exact independently selected hostname")
		}
	}
	for _, image := range []string{source.Image, "sha256:" + strings.Repeat("a", 64), "example.invalid/postgres@sha256:" + strings.Repeat("a", 64)} {
		if _, err := cnpgRecoveryResources("1234", source, image, barrier, "qualified-worker"); err == nil {
			t.Fatal("a mutable, unqualified or foreign source image was accepted")
		}
	}
	barrier.InRecovery = true
	if _, err := cnpgRecoveryResources("1234", source, "ghcr.io/cloudnative-pg/postgresql@sha256:"+strings.Repeat("a", 64), barrier, "qualified-worker"); err == nil {
		t.Fatal("a standby observation was treated as the source primary WAL barrier")
	}
}
