package main

import (
	"strings"
	"testing"
)

// TestRecoveryCiliumAgentCannotUseAnUnpublishedRuntimeDigest binds the executable release.
func TestRecoveryCiliumAgentCannotUseAnUnpublishedRuntimeDigest(t *testing.T) {
	agent, ds, node := recoveryAgentFixture()
	value(agent, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "quay.io/cilium/cilium@sha256:" + strings.Repeat("f", 64)
	if _, err := bindRecoveryCiliumAgent(agent, ds, node); err == nil {
		t.Fatal("unpublished Cilium executable accepted")
	}
}

func TestRecoveryCiliumAgentBindsPublishedArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch, digest string
		valid                  bool
	}{
		{"index amd64", "linux", "amd64", recoveryAgentIndex, true},
		{"index arm64", "linux", "arm64", recoveryAgentIndex, true},
		{"child amd64", "linux", "amd64", "sha256:a54ced05354670afb7fecd075e91130dff5cca16518da7ff3d1ef093c5c2d0ae", true},
		{"child arm64", "linux", "arm64", "sha256:dc2a1862e29de053e588980e121acb8b2e534206e9b3047ef5386f8c1eb13c29", true},
		{"wrong child architecture", "linux", "amd64", "sha256:dc2a1862e29de053e588980e121acb8b2e534206e9b3047ef5386f8c1eb13c29", false},
		{"unknown architecture", "linux", "riscv64", recoveryAgentIndex, false},
		{"missing architecture", "linux", "", recoveryAgentIndex, false},
		{"wrong operating system", "windows", "amd64", recoveryAgentIndex, false},
		{"unpublished digest", "linux", "amd64", "sha256:" + strings.Repeat("f", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, ds, node := recoveryAgentFixture()
			info := value(node, "status", "nodeInfo").(object)
			info["operatingSystem"], info["architecture"] = tc.os, tc.arch
			value(agent, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "quay.io/cilium/cilium@" + tc.digest
			_, err := bindRecoveryCiliumAgent(agent, ds, node)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestRecoveryCiliumAgentRejectsMatchingUnpublishedDeclarations(t *testing.T) {
	agent, ds, node := recoveryAgentFixture()
	for _, raw := range []any{value(agent, "spec", "containers"), value(ds, "spec", "template", "spec", "containers")} {
		raw.([]any)[0].(object)["image"] = "quay.io/cilium/cilium:v1.20.5@sha256:" + strings.Repeat("f", 64)
	}
	if _, err := bindRecoveryCiliumAgent(agent, ds, node); err == nil {
		t.Fatal("matching unpublished Pod and DaemonSet accepted")
	}
}
