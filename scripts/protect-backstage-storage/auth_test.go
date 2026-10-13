package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func syntheticGH(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	// Only synthetic tokens enter this fixture or its assertions.
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '%s:%s' \"$GH_TOKEN\" \"$PORTAL_STORAGE_GHCR_READER_TOKEN\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GH_TOKEN", "synthetic-actions-token")
}

func TestProductionExecutorScopesRegistryTokenOnlyToAttestation(t *testing.T) {
	syntheticGH(t)
	t.Setenv("PORTAL_STORAGE_GHCR_READER_TOKEN", "synthetic-registry-token")
	api, err := execute(context.Background(), "gh", "api", "--hostname=github.com", "repos/devantler-tech/platform/actions/runs/456")
	if err != nil || string(api) != "synthetic-actions-token:" {
		t.Fatal("API child lost Actions identity or inherited registry reader")
	}
	proof, err := execute(context.Background(), "gh", "attestation", "verify", "oci://ghcr.io/devantler-tech/platform/manifests@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || string(proof) != "synthetic-registry-token:" {
		t.Fatal("attestation child did not receive only scoped registry authentication")
	}
}

func TestProductionExecutorRefusesAttestationWithoutRegistryReader(t *testing.T) {
	syntheticGH(t)
	t.Setenv("PORTAL_STORAGE_GHCR_READER_TOKEN", "")
	b, err := execute(context.Background(), "gh", "attestation", "verify", "oci://ghcr.io/devantler-tech/platform/manifests@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err == nil || len(b) != 0 {
		t.Fatal("missing registry reader executed a fallback verifier")
	}
}
