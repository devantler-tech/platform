package recovery_test

import "testing"

// TestOnlySnapshotContainerReceivesKubernetesIdentity prevents token access by tools and mirror.
func TestOnlySnapshotContainerReceivesKubernetesIdentity(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/infrastructure")
	for _, workload := range []struct{ kind, name string }{{"Job", "vault-snapshot-init"}, {"CronJob", "vault-snapshot"}} {
		t.Run(workload.name, func(t *testing.T) {
			spec := object(t, objects, workload.kind, workload.name)["spec"].(map[string]any)
			if workload.kind == "CronJob" {
				spec = spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)
			}
			pod := spec["template"].(map[string]any)["spec"].(map[string]any)
			if value, ok := pod["automountServiceAccountToken"].(bool); !ok || value {
				t.Fatal("automatic identity must not reach every container")
			}
			found := false
			for _, entry := range pod["volumes"].([]any) {
				volume := entry.(map[string]any)
				if volume["name"] != "kubernetes-identity" {
					continue
				}
				projected, ok := volume["projected"].(map[string]any)
				if !ok || projected["defaultMode"] != 288 {
					t.Fatal("identity projection must remain group-readable and private")
				}
				sources, ok := projected["sources"].([]any)
				if !ok || len(sources) != 1 {
					t.Fatal("identity projection must have one bounded token")
				}
				token, ok := sources[0].(map[string]any)["serviceAccountToken"].(map[string]any)
				if !ok || token["path"] != "token" || token["expirationSeconds"] != 3600 {
					t.Fatal("identity token must retain its fixed path and bounded lifetime")
				}
				found = true
			}
			if !found {
				t.Fatal("snapshot requires its explicit Kubernetes identity")
			}
			mounts := 0
			for _, field := range []string{"initContainers", "containers"} {
				for _, entry := range pod[field].([]any) {
					container := entry.(map[string]any)
					for _, mount := range container["volumeMounts"].([]any) {
						m := mount.(map[string]any)
						if m["name"] != "kubernetes-identity" {
							continue
						}
						if container["name"] != "snapshot" || m["readOnly"] != true || m["mountPath"] != "/var/run/secrets/kubernetes.io/serviceaccount" {
							t.Fatal("Kubernetes identity escaped its authenticating container")
						}
						mounts++
					}
				}
			}
			if mounts != 1 {
				t.Fatal("only the snapshot container may receive Kubernetes identity")
			}
		})
	}
}
