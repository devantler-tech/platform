package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestObserveProductionPreparedTalosProfiles(t *testing.T) {
	if os.Getenv("LONGHORN_V2_TALOS_PROFILE_READBACK") != "prod" {
		t.Skip("requires the protected production Talos preview")
	}
	if os.Getenv("GITHUB_WORKFLOW") != "Observe Talos Storage Pool Layout" ||
		os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("GITHUB_REF") != "refs/heads/main" ||
		os.Getenv("GITHUB_RUN_ATTEMPT") != "1" {
		t.Fatal("the protected Talos preview is required")
	}

	config, err := os.ReadFile("../../ksail.prod.yaml")
	if err != nil {
		t.Fatal("cannot read the reviewed pool declarations")
	}
	pools, err := configuredMigrationPools(config)
	if err != nil {
		t.Fatal("the reviewed migration pools are unverified")
	}
	c := client{context: "admin@prod", run: preparedProfileReadCommand}
	before, err := c.list(t.Context(), "nodes", "")
	if err != nil {
		t.Fatal("cannot read the current worker inventory")
	}
	nodes, err := indexed(before)
	if err != nil {
		t.Fatal("the current worker inventory is incomplete")
	}

	prepared := 0
	for _, selection := range []string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"} {
		name, _, err := resolveTemporaryInventoryTarget(selection, before, pools)
		if err != nil {
			t.Fatal("the prepared worker roster is unverified")
		}
		matched, err := observeDesiredPreparedProfile(t.Context(), c, nodes[name], selection)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			prepared++
		}
	}
	fmt.Printf("LONGHORN_PREPARED_TALOS_PROFILES=OBSERVED workers=3 matching=%d writes=0\n", prepared)
}

func observeDesiredPreparedProfile(ctx context.Context, c client, node object, selection string) (bool, error) {
	address, err := workerPrivateAddress(node)
	if err != nil || !ready(valueObject(node, "status")) || c.verifyHost(ctx, address, node) != nil {
		return false, errors.New("the prepared worker hardware is unverified")
	}
	p := temporaryPreparation{preparation: preparation{c: c, temporarySelection: selection}}
	configs, err := p.readRetainedVolumeConfigs(ctx, address)
	if err != nil {
		return false, errors.New("the prepared worker volume declarations could not be observed")
	}
	repeated, err := p.readRetainedVolumeConfigs(ctx, address)
	if err != nil || !reflect.DeepEqual(configs, repeated) || c.verifyHost(ctx, address, node) != nil {
		return false, errors.New("the volume declarations or worker identity changed during observation")
	}
	if selection == "temporary-v2-1" {
		return retainedConfigRange(configs["r-longhorn"], permanentRawDiskGiB, permanentRawDiskGiB) &&
			retainedConfigRange(configs["r-longhorn-stage"], stagingRawDiskGiB, stagingRawDiskGiB) &&
			retainedConfigRange(configs["EPHEMERAL"], 40, 44), nil
	}
	return configs["r-longhorn-stage"] == nil && verifyStagingLayout(configs["r-longhorn"], configs["EPHEMERAL"]) == nil, nil
}

func preparedProfileReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !preparedProfileReadAllowed(program, args) {
		return nil, errors.New("the prepared Talos profile observation only permits scoped reads")
	}
	if program == "talosctl" {
		config := os.Getenv("TALOSCONFIG")
		if !strings.HasPrefix(config, "/") {
			return nil, errors.New("the protected Talos config path is unverified")
		}
		args = append([]string{"--talosconfig", config}, args...)
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

func preparedProfileReadAllowed(program string, args []string) bool {
	if program == "kubectl" {
		return slices.Equal(args, []string{"--context=admin@prod", "--request-timeout=20s", "get", "nodes", "--output=json"})
	}
	if program != "talosctl" || len(args) < 4 || !strings.HasPrefix(args[0], "--nodes=") {
		return false
	}
	address, err := netip.ParseAddr(strings.TrimPrefix(args[0], "--nodes="))
	return err == nil && netip.MustParsePrefix("10.0.0.0/16").Contains(address) &&
		(slices.Equal(args[1:], []string{"get", "volumeconfig", "--output=json"}) ||
			slices.Equal(args[1:], []string{"get", "systeminformation", "systeminformation", "--output=json"}))
}

func TestPreparedProfileReadOnlyAllowsItsRequiredQueries(t *testing.T) {
	for _, query := range []struct {
		program string
		args    []string
		allowed bool
	}{
		{"kubectl", []string{"--context=admin@prod", "--request-timeout=20s", "get", "nodes", "--output=json"}, true},
		{"kubectl", []string{"--context=admin@prod", "--request-timeout=20s", "get", "secrets", "--output=json"}, false},
		{"talosctl", []string{"--nodes=10.0.1.2", "get", "volumeconfig", "--output=json"}, true},
		{"talosctl", []string{"--nodes=10.0.1.2", "get", "systeminformation", "systeminformation", "--output=json"}, true},
		{"talosctl", []string{"--nodes=10.0.1.2", "get", "machineconfig", "--output=json"}, false},
		{"talosctl", []string{"--nodes=192.0.2.1", "get", "volumeconfig", "--output=json"}, false},
		{"talosctl", []string{"--nodes=10.0.1.2", "reset", "--reboot", "--graceful"}, false},
	} {
		if got := preparedProfileReadAllowed(query.program, query.args); got != query.allowed {
			t.Errorf("authorization for %s %v = %t, want %t", query.program, query.args, got, query.allowed)
		}
	}
}
