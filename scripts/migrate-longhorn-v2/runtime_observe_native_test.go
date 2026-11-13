package main

import (
	"context"
	"os"
	"regexp"
	"slices"
	"testing"
)

// This opt-in exercises the same fixed API reads with the operator's existing
// read-only identity. It cannot exercise the protected main workflow or install
// the image component. It never reads Secrets, host APIs or process RPCs.
func TestRuntimeInventoryNativeReadOnlyAPI(t *testing.T) {
	if os.Getenv("LONGHORN_RUNTIME_NATIVE_READ") != "1" {
		t.Skip("set LONGHORN_RUNTIME_NATIVE_READ=1 for the existing read-only production identity")
	}
	source := os.Getenv("LONGHORN_RUNTIME_NATIVE_SOURCE")
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) {
		t.Fatal("the current full main source is required for the native read")
	}
	c := client{context: "admin@prod", run: func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || !runtimeObservationReadAllowed(program, args...) {
			t.Fatal("native fixture selected an operation outside the fixed read boundary")
		}
		actual := slices.Clone(args)
		if program == "kubectl" {
			// Only the credential identity differs; all verbs and API paths
			// remain the production reader's exact compiled requests.
			actual[0] = "--context=oidc@prod"
		}
		return inventoryExecuteCommand(ctx, program, actual...)
	}}
	got, err := observeRuntimeInventory(t.Context(), c, source)
	if err != nil {
		t.Fatal("native read-only runtime inventory did not qualify:", err)
	}
	t.Logf("LONGHORN_RUNTIME_NATIVE_READ=OBSERVED nodes=%d storageNodes=%d instanceManagers=%d v1Volumes=%d v2Volumes=%d default=%s adoption=UNVERIFIED headroom=UNVERIFIED writes=0", got.KubernetesNodes, got.Nodes, got.InstanceManagers, got.V1Volumes, got.V2Volumes, got.Default)
}
