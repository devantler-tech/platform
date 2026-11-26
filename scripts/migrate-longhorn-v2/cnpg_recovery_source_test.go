package main

import (
	"strings"
	"testing"
)

func recoverySourceFixture(name string) (object, object, object) {
	ns, database, storeName, server, credential := "umami", "umami", "umami-db", "", "umami-db-backup-r2"
	if name == "coroot-db" {
		ns, database, storeName, credential = "observability", "coroot", "coroot-db", "coroot-db-backup-r2"
	}
	if name == "wedding-db" {
		ns, database, storeName, server, credential = "wedding-app", "wedding", "wedding-db-dedicated", "wedding-db-20260909", "wedding-db-backup-r2-dedicated"
	}
	if name == "customer-identity-db" {
		ns, database, storeName, credential = "customer-identity", "zitadel", name, "customer-identity-db-backup-r2"
	}
	if name == "business-site-portal-db" {
		ns, database, storeName, credential = "business-site", "portal", name, "business-site-portal-db-backup-r2"
	}
	c, _, _, _ := cnpgFixture()
	c["metadata"].(object)["name"], c["metadata"].(object)["namespace"] = name, ns
	c["metadata"].(object)["generation"] = float64(1)
	c["spec"].(object)["bootstrap"] = object{"initdb": object{"database": database}}
	c["spec"].(object)["storage"].(object)["size"] = "5Gi"
	if ns == "customer-identity" || ns == "business-site" {
		c["spec"].(object)["instances"], c["status"].(object)["readyInstances"] = float64(2), float64(2)
		c["spec"].(object)["storage"] = object{"size": "10Gi", "storageClass": "hcloud"}
	}
	parameters := object{"barmanObjectName": storeName}
	if server != "" {
		parameters["serverName"] = server
	}
	c["spec"].(object)["plugins"] = []any{object{"name": "barman-cloud.cloudnative-pg.io", "enabled": true, "isWALArchiver": true, "parameters": parameters}}
	s, b := resource(storeName), resource("fresh-backup")
	s["metadata"].(object)["namespace"], s["metadata"].(object)["generation"] = ns, float64(1)
	credentials := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		credentials[field] = object{"name": credential, "key": key}
	}
	s["spec"] = object{"configuration": object{"destinationPath": "s3://fixture-backups/cnpg/" + name, "endpointURL": "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com", "s3Credentials": credentials, "wal": object{"compression": "gzip"}, "data": object{"compression": "gzip"}}}
	if name == "wedding-db" {
		value(s, "spec", "configuration").(object)["destinationPath"] = "s3://wedding-db-backups/cnpg/wedding-db"
	}
	b["metadata"].(object)["namespace"] = ns
	b["spec"] = object{"cluster": object{"name": name}, "method": "plugin", "pluginConfiguration": object{"name": "barman-cloud.cloudnative-pg.io"}}
	b["status"] = object{"phase": "completed", "backupId": "20261101T060000", "pluginMetadata": object{"name": "barman-cloud.cloudnative-pg.io", "clusterUID": text(c, "metadata", "uid")}}
	return c, s, b
}

func archiveBindingFixture(c, s object) cnpgArchiveBinding {
	return cnpgArchiveBinding{ClusterUID: text(c, "metadata", "uid"), StoreUID: text(s, "metadata", "uid"), ClusterGeneration: 1, StoreGeneration: 1,
		Endpoint: text(s, "spec", "configuration", "endpointURL"), Destination: text(s, "spec", "configuration", "destinationPath")}
}

func TestRecoveryBindsEachDatabaseToItsActualArchiveServer(t *testing.T) {
	for name, server := range map[string]string{"umami-db": "umami-db", "coroot-db": "coroot-db", "wedding-db": "wedding-db-20260909", "customer-identity-db": "customer-identity-db", "business-site-portal-db": "business-site-portal-db"} {
		c, s, b := recoverySourceFixture(name)
		binding, err := bindCNPGRecoverySource(c, s, b, text(b, "metadata", "uid"), archiveBindingFixture(c, s))
		if err != nil || binding.Server != server || binding.Database == "" || binding.BackupUID != text(b, "metadata", "uid") {
			t.Fatalf("%s archive binding: %+v %v", name, binding, err)
		}
	}
}

func TestProviderArchiveRequiresItsExactSourceProfile(t *testing.T) {
	for _, name := range []string{"customer-identity-db", "business-site-portal-db"} {
		t.Run(name, func(t *testing.T) {
			c, s, _ := recoverySourceFixture(name)
			expected := archiveBindingFixture(c, s)
			value(c, "spec", "storage").(object)["storageClass"] = "longhorn-wffc"
			if _, err := bindCNPGArchiveSource(c, s, expected); err == nil {
				t.Fatal("an unexpected source storage class was accepted")
			}
			value(c, "spec", "storage").(object)["storageClass"] = "hcloud"
			value(c, "status").(object)["readyInstances"] = float64(1)
			if _, err := bindCNPGArchiveSource(c, s, expected); err == nil {
				t.Fatal("a degraded provider source was accepted")
			}
		})
	}
}

func TestRecoveryRefusesWrongSourceArchiveAndCredentialScope(t *testing.T) {
	for name, change := range map[string]func(object, object, object){
		"unhealthy source":  func(c, _, _ object) { c["status"].(object)["readyInstances"] = float64(2) },
		"unrelated cluster": func(c, _, _ object) { c["metadata"].(object)["namespace"] = "unrelated" },
		"changed archive server": func(c, _, _ object) {
			value(c, "spec", "plugins").([]any)[0].(object)["parameters"].(object)["serverName"] = "another-server"
		},
		"explicit empty server": func(c, _, _ object) {
			value(c, "spec", "plugins").([]any)[0].(object)["parameters"].(object)["serverName"] = ""
		},
		"non-string server": func(c, _, _ object) {
			value(c, "spec", "plugins").([]any)[0].(object)["parameters"].(object)["serverName"] = float64(1)
		},
		"another R2 account": func(_, s, _ object) {
			value(s, "spec", "configuration").(object)["endpointURL"] = "https://" + strings.Repeat("b", 32) + ".r2.cloudflarestorage.com"
		},
		"another bucket": func(_, s, _ object) {
			value(s, "spec", "configuration").(object)["destinationPath"] = "s3://another-bucket/cnpg/umami-db"
		},
		"store revision changed": func(_, s, _ object) { s["metadata"].(object)["generation"] = float64(2) },
		"changed store":          func(_, s, _ object) { s["metadata"].(object)["name"] = "another-store" },
		"foreign backup":         func(_, _, b object) { value(b, "status", "pluginMetadata").(object)["clusterUID"] = "another-cluster" },
		"incomplete backup":      func(_, _, b object) { b["status"].(object)["phase"] = "running" },
		"unsafe endpoint": func(_, s, _ object) {
			value(s, "spec", "configuration").(object)["endpointURL"] = "https://example.invalid"
		},
		"another archive path": func(_, s, _ object) {
			value(s, "spec", "configuration").(object)["destinationPath"] = "s3://fixture-backups/cnpg/another-db"
		},
		"additional credential": func(_, s, _ object) {
			value(s, "spec", "configuration", "s3Credentials").(object)["sessionToken"] = object{"name": "unrelated", "key": "token"}
		},
		"foreign credential": func(_, s, _ object) {
			value(s, "spec", "configuration", "s3Credentials", "accessKeyId").(object)["name"] = "unrelated"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, s, b := recoverySourceFixture("umami-db")
			expected := archiveBindingFixture(c, s)
			change(c, s, b)
			if _, err := bindCNPGRecoverySource(c, s, b, text(b, "metadata", "uid"), expected); err == nil {
				t.Fatal("unsafe source recovery binding accepted")
			}
		})
	}
	c, s, b := recoverySourceFixture("umami-db")
	if _, err := bindCNPGRecoverySource(c, s, b, "older-backup", archiveBindingFixture(c, s)); err == nil {
		t.Fatal("a different backup replaced the fresh owned one")
	}
	if _, err := bindCNPGRecoverySource(c, s, b, text(b, "metadata", "uid"), cnpgArchiveBinding{}); err == nil {
		t.Fatal("missing pre-backup archive binding was accepted")
	}
	c, s, b = recoverySourceFixture("wedding-db")
	value(s, "spec", "configuration").(object)["destinationPath"] = "s3://another-bucket/cnpg/wedding-db"
	if _, err := bindCNPGRecoverySource(c, s, b, text(b, "metadata", "uid"), archiveBindingFixture(c, s)); err == nil {
		t.Fatal("Wedding's dedicated archive constraint was replaced")
	}
}
