package main

import (
	"context"
	"os"
	"regexp"
	"slices"
	"testing"
)

// This opt-in reaches the changed Kubernetes read routes with the existing
// read-only identity. It never reads host APIs or Secrets, measures provider
// filesystem use, changes retention, or grants a migration/capacity clearance.
func TestProviderProjectionNativeReadOnlyAPI(t *testing.T) {
	if os.Getenv("LONGHORN_PROVIDER_PROJECTION_NATIVE_READ") != "1" {
		t.Skip("set LONGHORN_PROVIDER_PROJECTION_NATIVE_READ=1 for the existing read-only production identity")
	}
	source := os.Getenv("LONGHORN_PROVIDER_PROJECTION_NATIVE_SOURCE")
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) {
		t.Fatal("the current full main source is required for the native read")
	}
	c := client{context: "admin@prod", run: func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || program != "git" && program != "kubectl" || !providerFootprintReadAllowed(program, args...) {
			t.Fatal("native projection selected an operation outside its fixed Kubernetes/source read boundary")
		}
		actual := slices.Clone(args)
		if program == "kubectl" {
			actual[0] = "--context=oidc@prod"
		}
		return inventoryExecuteCommand(ctx, program, actual...)
	}}
	if err := sourceCurrent(t.Context(), c.run, source); err != nil {
		t.Fatal(err)
	}
	claims, mounts, err := observeProviderVolumeProjection(t.Context(), c)
	if err != nil || len(claims) == 0 {
		t.Fatal("the complete native provider binding/consumer projection did not qualify", err)
	}
	identityClaims := 0
	for _, claim := range claims {
		if claim.Namespace == "customer-identity" {
			identityClaims++
		}
	}
	if identityClaims == 0 {
		t.Fatal("the newly initialized identity database was not included in the native provider projection")
	}
	for _, namespace := range []string{"customer-identity", "business-site"} {
		if _, err := c.list(t.Context(), "pods", namespace); err != nil {
			t.Fatal("a new fixed namespace's native Pod read did not qualify", err)
		}
	}
	if err := sourceCurrent(t.Context(), c.run, source); err != nil {
		t.Fatal(err)
	}
	t.Logf("LONGHORN_PROVIDER_PROJECTION_NATIVE_READ=OBSERVED claims=%d mounts=%d identityClaims=%d futurePortalNamespaceRead=OBSERVED physicalUsage=UNVERIFIED retention=UNVERIFIED writes=0", len(claims), len(mounts), identityClaims)
}
