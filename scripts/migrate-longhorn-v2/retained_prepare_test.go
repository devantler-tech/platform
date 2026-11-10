package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/devantler-tech/platform/internal/longhornmigration"
)

const retainedFixtureStage = "12345678-1234-4234-8234-000000000090"

type retainingFixture struct {
	*unusedPreparationFixture
	closed, uncertainClose, changedExtent bool
	closes, reopens                       int
}

type retainingReader struct {
	*unusedParentReaderFixture
	fixture *retainingFixture
}

func (r *retainingReader) ListRetainedBlockLayout(_ context.Context, closed bool) (longhorninventory.BlockLayoutReport, longhorninventory.RetainedLayoutIdentity, error) {
	f := r.fixture
	if closed != f.closed {
		return longhorninventory.BlockLayoutReport{}, longhorninventory.RetainedLayoutIdentity{}, errInventory
	}
	report := longhorninventory.BlockLayoutReport{RawExtentObserved: true, VolumeStatuses: 3, DiscoveredVolumes: 4,
		EphemeralPhase: "ready", EphemeralSizeBytes: 197 << 30, EphemeralLUKS2: true, EphemeralLockedToState: true,
		EphemeralNodeIDOnly: true, Digest: strings.Repeat("a", 64)}
	identity := longhorninventory.RetainedLayoutIdentity{
		PartitionUUID: "12345678-1234-4234-8234-000000000080", ExtentDigest: strings.Repeat("b", 64),
		StageStatusPresent: true, EphemeralUUID: journalFixture().EphemeralUUID,
	}
	if f.base.postReset {
		identity.EphemeralUUID, identity.StagePartitionUUID = "recovered-ephemeral", retainedFixtureStage
		report.EphemeralSizeBytes = 42 << 30
	}
	if f.changedExtent {
		identity.ExtentDigest = strings.Repeat("c", 64)
	}
	return report, identity, nil
}

func retainingPreparationAPI(t *testing.T) (*retainingFixture, temporaryPreparation) {
	t.Helper()
	ordinary, p := unusedPreparationAPI(t)
	f := &retainingFixture{unusedPreparationFixture: ordinary}
	f.base.node, f.base.storage = f.base.s.nodes[5], f.base.s.storageNodes[5]
	p.target, p.name, p.temporarySelection = text(f.base.node, "metadata", "name"), "longhorn-v2-prepare-temporary-v2-1", "temporary-v2-1"
	p.staging, p.c.run = true, f.run
	p.reader = func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
		if address != "10.0.99.9" {
			t.Fatal("retaining preparation selected another host")
		}
		f.nativeCalls++
		boot := text(f.base.node, "status", "nodeInfo", "bootID")
		return &retainingReader{&unusedParentReaderFixture{temporaryNativeReader: temporaryNativeReader{boot: boot}, mode: "cache directories only"}, f}, func() error { return nil }, nil
	}
	return f, p
}

func (f *retainingFixture) run(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if program != "talosctl" || len(args) < 2 {
		return f.unusedPreparationFixture.run(ctx, input, program, args...)
	}
	if args[0] != "--nodes=10.0.99.9" {
		f.base.t.Fatal("retaining preparation reached an unrelated host")
	}
	switch {
	case args[1] == "patch":
		var j journal
		if len(args) != 5 || args[2] != "machineconfig" || args[3] != "--mode=no-reboot" ||
			json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &j) != nil || j.Schema != 3 ||
			value(f.base.node, "spec", "unschedulable") != true {
			f.base.t.Fatal("layout mutation lacked its original cordon, journal or host")
		}
		switch args[4] {
		case "--patch=" + closeRetainedRawConfig:
			if f.closed || f.base.postReset || j.Phase != "draining" {
				f.base.t.Fatal("unsafe old mapper closure")
			}
			f.closed = true
			f.closes++
			if f.uncertainClose {
				return nil, errors.New("closure accepted but response lost")
			}
		case "--patch=@talos/workers/reserve-longhorn-data.yaml":
			if !f.closed || !f.base.postReset || j.Phase != "reset-requested" {
				f.base.t.Fatal("old mapper reopened before verified recovery")
			}
			f.closed = false
			f.reopens++
		default:
			f.base.t.Fatal("unexpected configuration mutation")
		}
		return nil, nil
	case args[1] == "reset":
		want := []string{"--nodes=10.0.99.9", "reset", "--system-labels-to-wipe=EPHEMERAL", "--reboot", "--graceful=true", "--wait", "--timeout=15m"}
		var j journal
		if !reflect.DeepEqual(args, want) || json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &j) != nil ||
			j.Schema != 3 || j.Phase != "reset-requested" || !f.closed || f.base.resets != 0 {
			f.base.t.Fatal("reset did not preserve both old partitions or preceded durable intent")
		}
		f.base.resets++
		f.base.postReset = true
		valueObject(f.base.node, "status", "nodeInfo")["bootID"] = "12345678-1234-4234-8234-000000000099"
		return nil, errors.New("reset accepted but response lost")
	case len(args) == 5 && args[2] == "volumeconfig":
		raw, ephemeral := desiredConfigFixture()
		switch args[3] {
		case longhornmigration.RetainedStageRawID:
			raw["metadata"].(object)["id"] = longhornmigration.RetainedStageRawID
			spec := valueObject(raw, "spec", "provisioning", "partitionSpec")
			spec["minSize"], spec["maxSize"] = float64(144<<30), float64(144<<30)
			return json.Marshal(raw)
		case "EPHEMERAL":
			spec := valueObject(ephemeral, "spec", "provisioning", "partitionSpec")
			spec["maxSize"], spec["negativeMaxSize"] = float64(44<<30), false
			return json.Marshal(ephemeral)
		case "r-longhorn":
			if f.closed {
				return nil, errInventory
			}
			return json.Marshal(raw)
		}
	}
	return f.unusedPreparationFixture.run(ctx, input, program, args...)
}

func TestRetainingPreparationRecoversAnUncertainCloseAndNeverReplaysReset(t *testing.T) {
	f, p := retainingPreparationAPI(t)
	f.uncertainClose = true
	if err := p.prepare(t.Context()); err == nil || f.closes != 1 || f.base.resets != 0 || p.j.Phase != "draining" {
		t.Fatal("uncertain closure reached a reset", err)
	}
	if err := p.prepare(t.Context()); err != nil || f.closes != 1 || f.base.resets != 1 || f.reopens != 1 ||
		p.j.Phase != "prepared" || p.j.PreparedRawUUID != retainedFixtureStage || !unusedStorageFenced(f.base.storage) {
		t.Fatalf("preserving recovery failed: phase=%s closes=%d resets=%d reopens=%d: %v", p.j.Phase, f.closes, f.base.resets, f.reopens, err)
	}
	if err := p.prepare(t.Context()); err != nil || f.base.resets != 1 || f.closes != 1 || f.reopens != 1 {
		t.Fatal("a completed preparation replayed a host write", err)
	}
	f.changedExtent = true
	if err := p.prepare(t.Context()); err == nil || f.base.resets != 1 {
		t.Fatal("a changed retained extent qualified or replayed reset")
	}
}
