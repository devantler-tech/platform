package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDiskRemovalRetryPreservesAForeignDisk(t *testing.T) {
	poll := readinessPoll
	readinessPoll = time.Millisecond
	t.Cleanup(func() { readinessPoll = poll })
	f, p := newPreparationAPI(t, "draining")
	f.storage["spec"].(object)["disks"] = disabledDisks(p.j.OriginalDisks)
	clearAttempts := 0
	p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "kubectl" && len(args) > 3 && args[2] == "patch" && args[3] == "nodes.longhorn.io" {
			var patches []object
			if json.Unmarshal(input, &patches) != nil {
				t.Fatal("invalid patch")
			}
			for _, patch := range patches {
				if text(patch, "path") == "/spec/disks" {
					clearAttempts++
					if clearAttempts == 1 {
						f.storage["spec"].(object)["disks"].(object)["foreign"] = object{"path": "/foreign", "diskType": "filesystem", "allowScheduling": false}
						return nil, errors.New("webhook has not reconciled the first removal")
					}
				}
			}
		}
		return f.run(ctx, input, program, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := p.removeOriginalDisks(ctx, f.storage, false)
	if !errors.Is(err, errReadinessTerminal) || clearAttempts != 1 || value(f.storage, "spec", "disks", "foreign") == nil {
		t.Fatalf("a retry cleared changed storage: attempts=%d error=%v", clearAttempts, err)
	}
}

func TestDiskRemovalDoesNotReplayAnAlreadyAcceptedRemoval(t *testing.T) {
	f, p := newPreparationAPI(t, "draining")
	f.storage["spec"].(object)["disks"] = object{}
	p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "kubectl" && len(args) > 2 && args[2] == "patch" {
			t.Fatal("empty disk map was rewritten after positive readback")
		}
		return f.run(ctx, input, program, args...)
	}
	if err := p.removeOriginalDisks(context.Background(), f.storage, false); err != nil {
		t.Fatal(err)
	}
}

func TestDiskRemovalHoldsWhenTheEvacuationFenceChanges(t *testing.T) {
	for _, change := range []func(object){
		func(spec object) { spec["allowScheduling"] = true },
		func(spec object) { spec["evictionRequested"] = false },
	} {
		f, p := newPreparationAPI(t, "draining")
		f.storage["spec"].(object)["disks"] = disabledDisks(p.j.OriginalDisks)
		change(f.storage["spec"].(object))
		p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
			if program == "kubectl" && len(args) > 2 && args[2] == "patch" {
				t.Fatal("changed evacuation fence permitted a disk write")
			}
			return f.run(ctx, input, program, args...)
		}
		if !errors.Is(p.removeOriginalDisks(context.Background(), f.storage, false), errReadinessTerminal) {
			t.Fatal("changed evacuation fence remained retryable")
		}
	}
}
