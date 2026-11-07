package recovery_test

import "testing"

func podTemplateAnnotations(t *testing.T, objects []map[string]any) map[string]any {
	t.Helper()
	spec := object(t, objects, "Job", "vault-snapshot-init")["spec"].(map[string]any)
	metadata, _ := spec["template"].(map[string]any)["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	return annotations
}

// The production recovery patch is a JSON Patch `add` of the whole pod-template
// annotations map, and `add` replaces a map that already exists. Any annotation
// the base adds to that pod template must therefore still be present in
// production, or the patch has started dropping it.
func TestProductionRecoveryPatchKeepsBasePodTemplateAnnotations(t *testing.T) {
	base := podTemplateAnnotations(t, render(t, "k8s/bases/infrastructure/vault-backup"))
	production := podTemplateAnnotations(t, render(t, "k8s/providers/hetzner/infrastructure"))
	for key, value := range base {
		if production[key] != value {
			t.Errorf("production vault-snapshot-init pod template lost base annotation %q; "+
				"patches/recover-vault-snapshot.yaml replaces the whole map, so add the key there instead", key)
		}
	}
	if production["backup.devantler.tech/recovery-id"] != "2026-09-08-csi-metadata" {
		t.Fatal("production vault-snapshot-init must keep its recovery-id, or Flux replaces the Job and re-runs it")
	}
}
