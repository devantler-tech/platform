package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// credentialLocationObject supplies only synthetic substitution inputs.
func credentialLocationObject(t *testing.T, kind, name, prefix string) []byte {
	t.Helper()
	if kind == "Secret" {
		prefix = base64.StdEncoding.EncodeToString([]byte(prefix))
	}
	b, err := json.Marshal(map[string]any{"kind": kind, "metadata": map[string]string{"name": name, "namespace": "flux-system"}, "data": map[string]string{"r2_prefix_velero": prefix}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCredentialArchiveLocationBindsTheDeclaredTarget catches cross-store or stale-prefix recovery.
func TestCredentialArchiveLocationBindsTheDeclaredTarget(t *testing.T) {
	target := target{Endpoint: "https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com", Bucket: "fixture-backups"}
	base := credentialLocationObject(t, "ConfigMap", "variables-base", "velero/base")
	cluster := credentialLocationObject(t, "ConfigMap", "variables-cluster", "velero/cluster")
	variables := credentialLocationObject(t, "Secret", "variables-cluster", "velero/prod")
	for _, scenario := range []string{"matching", "wrong-bucket", "wrong-endpoint", "wrong-prefix", "unavailable", "wrong-location-name"} {
		t.Run(scenario, func(t *testing.T) {
			storage := map[string]any{"bucket": "fixture-backups", "prefix": "velero/prod"}
			config := map[string]any{"region": "auto", "s3ForcePathStyle": "true", "s3Url": target.Endpoint}
			metadata := map[string]any{"name": "default", "namespace": "velero"}
			status := map[string]any{"phase": "Available"}
			switch scenario {
			case "wrong-bucket":
				storage["bucket"] = "unrelated-backups"
			case "wrong-endpoint":
				config["s3Url"] = "https://unrelated.example.test"
			case "wrong-prefix":
				storage["prefix"] = "velero/base"
			case "unavailable":
				status["phase"] = "Unavailable"
			case "wrong-location-name":
				metadata["name"] = "unrelated"
			}
			raw, err := json.Marshal(map[string]any{"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation", "metadata": metadata, "spec": map[string]any{"provider": "aws", "objectStorage": storage, "config": config}, "status": status})
			if err != nil {
				t.Fatal(err)
			}
			got, err := credentialArchivePrefix(base, cluster, variables, raw, target)
			if scenario == "matching" {
				if err != nil || got != "velero/prod" {
					t.Fatal("effective declared backup location was not bound")
				}
			} else if err == nil {
				t.Fatal("unrelated or unavailable backup location was accepted")
			}
		})
	}
}
