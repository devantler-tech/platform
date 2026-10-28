package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestBackupTargetRequiresFixedObjectsAndCloudflareOrigin rejects invented deployment facts.
func TestBackupTargetRequiresFixedObjectsAndCloudflareOrigin(t *testing.T) {
	object := func(kind, namespace, name string, data map[string]string) []byte {
		b, err := json.Marshal(map[string]any{"kind": kind, "metadata": map[string]string{"name": name, "namespace": namespace}, "data": data})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := object("ConfigMap", "flux-system", "variables-base", map[string]string{"r2_endpoint": "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com", "r2_bucket": "fixture-backups"})
	cluster := object("ConfigMap", "flux-system", "variables-cluster", map[string]string{})
	secret := object("Secret", "flux-system", "variables-cluster", map[string]string{})
	keys := object("Secret", "openbao", "vault-snapshot-r2", map[string]string{"access_key_id": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32))), "secret_access_key": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 64)))})
	for _, test := range []string{"complete", "missing-target", "wrong-namespace", "external-origin", "invalid-key", "ambiguous-bucket"} {
		t.Run(test, func(t *testing.T) {
			b, c, s, k := append([]byte(nil), base...), cluster, secret, keys
			switch test {
			case "missing-target":
				b = object("ConfigMap", "flux-system", "variables-base", map[string]string{})
			case "wrong-namespace":
				k = []byte(strings.Replace(string(keys), `"namespace":"openbao"`, `"namespace":"other"`, 1))
			case "external-origin":
				b = []byte(strings.Replace(string(base), strings.Repeat("a", 32)+".r2.cloudflarestorage.com", "example.invalid", 1))
			case "invalid-key":
				k = object("Secret", "openbao", "vault-snapshot-r2", map[string]string{"access_key_id": "not-base64", "secret_access_key": "not-base64"})
			case "ambiguous-bucket":
				b = []byte(strings.Replace(string(base), "fixture-backups", "../other", 1))
			}
			_, err := backupTarget(b, c, s, k)
			if (err == nil) != (test == "complete") {
				t.Fatal("incorrect deployment-target verdict")
			}
		})
	}
}

// TestFreshSnapshotRequiresCompletedReplacementJob rejects stale successful history.
func TestFreshSnapshotRequiresCompletedReplacementJob(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	job := map[string]any{"kind": "Job", "metadata": map[string]any{"name": "vault-snapshot-init", "namespace": "openbao"}, "spec": map[string]any{"activeDeadlineSeconds": 3600, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "vault-snapshot", "automountServiceAccountToken": false, "volumes": []any{map[string]any{"name": "snapshots", "persistentVolumeClaim": map[string]any{"claimName": "vault-snapshots-recovery"}}}, "containers": []any{map[string]any{"name": "mirror", "image": mirrorImage, "env": []any{map[string]any{"name": "REQUIRE_REMOTE_SNAPSHOT", "value": "true"}}}}}}}, "status": map[string]any{"startTime": now.Add(-time.Minute).Format(time.RFC3339), "completionTime": now.Format(time.RFC3339), "succeeded": 1, "conditions": []any{map[string]string{"type": "Complete", "status": "True"}}}}
	b, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"complete", "old-volume", "incomplete", "old-success", "missing-time", "automatic-token"} {
		t.Run(test, func(t *testing.T) {
			var v map[string]any
			if json.Unmarshal(b, &v) != nil {
				t.Fatal("invalid fixture")
			}
			status := v["status"].(map[string]any)
			pod := v["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			switch test {
			case "old-volume":
				pod["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "vault-snapshots"
			case "incomplete":
				delete(status, "conditions")
			case "old-success":
				status["startTime"] = now.Add(-48 * time.Hour).Format(time.RFC3339)
				status["completionTime"] = now.Add(-47 * time.Hour).Format(time.RFC3339)
			case "missing-time":
				delete(status, "completionTime")
			case "automatic-token":
				pod["automountServiceAccountToken"] = true
			}
			encoded, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = freshRecoveryWindow(encoded, now)
			if (err == nil) != (test == "complete") {
				t.Fatal("incorrect fresh-job verdict")
			}
		})
	}
}
