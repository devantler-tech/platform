package kyvernologs

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

// requireVerifiedConsumer binds the consumer witness to the node-agent
// source verified for this profile, not to whatever module Renovate proposes.
// Source: coroot/coroot-node-agent@ee62018e73b90b1549f5b81ba0d3d6e28851a1df/go.mod.
// A deliberate refresh must reverify the deployed agent's source before changing
// this profile. This offline check does not attest the current live deployment.
func requireVerifiedConsumer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-m", "-json", "github.com/coroot/logparser").Output()
	if err != nil {
		t.Fatalf("cannot establish selected consumer module: %v", err)
	}
	var module struct {
		Path, Version string
		Replace       json.RawMessage
	}
	if err := json.Unmarshal(out, &module); err != nil {
		t.Fatalf("cannot decode selected consumer module: %v", err)
	}
	if module.Path != "github.com/coroot/logparser" || module.Version != "v1.4.0" || len(module.Replace) != 0 {
		t.Fatalf("consumer is not the verified node-agent parser: selected=%s@%s; refresh the source binding before claiming deployed-consumer coverage", module.Path, module.Version)
	}
}
