package preparation_test

import "testing"

func assertRetainedMailStorage(t *testing.T, objects []map[string]any) {
	t.Helper()
	namespace := object(t, objects, "Namespace", "private-mail")
	pvc := object(t, objects, "PersistentVolumeClaim", "private-mail-data")
	for _, resource := range []map[string]any{namespace, pvc} {
		metadata := mapping(t, resource["metadata"])
		if mapping(t, metadata["annotations"])["kustomize.toolkit.fluxcd.io/prune"] != "disabled" {
			t.Fatal("mail persistence must survive a failed serving deployment")
		}
	}
	if mapping(t, pvc["metadata"])["namespace"] != "private-mail" {
		t.Fatal("mail storage must retain its existing namespace identity")
	}
	spec := mapping(t, pvc["spec"])
	modes := sequence(t, spec["accessModes"])
	if len(modes) != 1 || modes[0] != "ReadWriteOnce" || mapping(t, mapping(t, spec["resources"])["requests"])["storage"] != "2Gi" {
		t.Fatal("storage staging must preserve the existing claim's access mode and requested capacity")
	}
}

func TestProductionRetainsMailStorage(t *testing.T) {
	assertRetainedMailStorage(t, render(t, "k8s/providers/hetzner/apps"))
}

func TestMailStorageBundleDoesNotStartOrExposeAService(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-storage")
	if len(objects) != 2 {
		t.Fatal("the independent storage bundle must contain only its namespace and claim")
	}
	assertRetainedMailStorage(t, objects)
	assertRetainedMailStorage(t, render(t, "k8s/bases/apps/private-mail"))
}
