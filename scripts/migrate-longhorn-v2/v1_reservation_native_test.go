package main

import (
	"context"
	"maps"
	"os"
	"regexp"
	"slices"
	"testing"
)

// This evaluates the changed allocation decision against native API state. It
// cannot execute the mutation, read a Secret or call a host/process API.
func TestV1ReservationNativeReadOnlyPlan(t *testing.T) {
	if os.Getenv("LONGHORN_V1_REPAIR_NATIVE_READ") != "1" {
		t.Skip("set LONGHORN_V1_REPAIR_NATIVE_READ=1 for the existing read-only production identity")
	}
	source := os.Getenv("LONGHORN_V1_REPAIR_NATIVE_SOURCE")
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) {
		t.Fatal("the current full main source is required for the native read")
	}
	c := client{context: "admin@prod", run: func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		allowed := input == nil && program == "git" && slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"})
		if input == nil && program == "kubectl" {
			prefix := []string{"--context=admin@prod", "--request-timeout=20s", "get"}
			for _, query := range [][]string{
				{"nodes", "--output=json"},
				{"nodes.longhorn.io", "--output=json", "--namespace=longhorn-system"},
				{"volumes.longhorn.io", "--output=json", "--namespace=longhorn-system"},
				{"replicas.longhorn.io", "--output=json", "--namespace=longhorn-system"},
				{"engines.longhorn.io", "--output=json", "--namespace=longhorn-system"},
				{"orphans.longhorn.io", "--output=json", "--namespace=longhorn-system"},
				{"pods", "--output=json", "--all-namespaces"},
				{"settings.longhorn.io", "storage-over-provisioning-percentage", "--output=json", "--namespace=longhorn-system"},
				{"settings.longhorn.io", "storage-minimal-available-percentage", "--output=json", "--namespace=longhorn-system"},
			} {
				allowed = allowed || slices.Equal(args, append(slices.Clone(prefix), query...))
			}
		}
		if !allowed {
			t.Fatal("native planner attempted a request outside its fixed read boundary")
		}
		actual := slices.Clone(args)
		if program == "kubectl" {
			actual[0] = "--context=oidc@prod"
		}
		return inventoryExecuteCommand(ctx, program, actual...)
	}}
	if sourceCurrent(t.Context(), c.run, source) != nil || verifyV1ReservationSettings(t.Context(), c) != nil {
		t.Fatal("the native source or scheduling policy did not qualify")
	}
	var original v1ReservationRepair
	for i := range 2 {
		s, err := c.snapshot(t.Context())
		if err != nil {
			t.Fatal("the complete native inventory could not be read")
		}
		plan, err := planV1ReservationRepair(s)
		if err != nil {
			// Planner failures are fixed control messages, never transport text.
			t.Fatal("the native inventory did not qualify the proposed repair:", err)
		}
		if i == 0 {
			original = plan
		} else if text(plan.node, "metadata", "uid") != text(original.node, "metadata", "uid") ||
			plan.disk != original.disk || plan.diskUUID != original.diskUUID || plan.reserve != original.reserve || !maps.Equal(plan.volumes, original.volumes) {
			t.Fatal("the native repair identities or allocation decision changed")
		}
	}
	if sourceCurrent(t.Context(), c.run, source) != nil || verifyV1ReservationSettings(t.Context(), c) != nil {
		t.Fatal("the native source or scheduling policy changed")
	}
	t.Logf("LONGHORN_V1_REPAIR_NATIVE=PLANNED observations=2 volumes=%d writes=0 recovery=UNVERIFIED", len(original.volumes))
}
