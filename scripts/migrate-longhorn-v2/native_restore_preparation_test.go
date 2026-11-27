package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func nativeRestorePreparationFixture(selection string, index int) nativeRestorePreparationReceipt {
	profile := nativeRestoreUnusedProfile
	if selection == "temporary-v2-1" {
		profile = nativeRestoreRetainedProfile
	}
	return nativeRestorePreparationReceipt{Profile: profile,
		ProviderID: fmt.Sprintf("hcloud://%d", index), MachineUUID: fmt.Sprintf("machine-%d", index), LonghornUID: fmt.Sprintf("storage-node-%d", index), DiskUUID: fmt.Sprintf("disk-%d", index),
		Journal:       benchmarkResource{Name: "longhorn-v2-prepare-" + selection, Namespace: "longhorn-system", UID: fmt.Sprintf("preparation-journal-%d", index)},
		JournalSHA256: fmt.Sprintf("%064x", index), BootID: fmt.Sprintf("00000000-0000-4000-8000-%012d", index), RawPartitionUUID: fmt.Sprintf("10000000-0000-4000-8000-%012d", index),
		Activation:       benchmarkResource{Name: temporaryActivationJournal, Namespace: "longhorn-system", UID: "native-activation-journal-uid"},
		ActivationSHA256: strings.Repeat("a", 64),
	}
}

func TestNativeRestoreTemporaryRosterUsesJournalBoundDiskProfiles(t *testing.T) {
	targets, nodes, storage := nativeRestoreTargetsFixture()
	staged := targets[2]
	before, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture())
	if err != nil || before[staged.Name].Preparation != staged.Preparation || before[staged.Name].DiskUUID == staged.Preparation.RawPartitionUUID {
		t.Fatal("prepared temporary profiles were not bound:", err)
	}
	targets[2].Preparation.Profile = nativeRestoreUnusedProfile
	if got, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture()); err == nil || got != nil {
		t.Fatal("profile substitution accepted a staged mapper")
	}
}

func TestNativeRestorePreparationReceiptsAreCompleteAndImmutable(t *testing.T) {
	for _, fault := range []string{"missing profile", "missing journal", "wrong journal", "wrong namespace", "missing digest", "bad digest", "missing partition", "missing boot", "changed boot"} {
		t.Run(fault, func(t *testing.T) {
			targets, nodes, storage := nativeRestoreTargetsFixture()
			p := &targets[0].Preparation
			switch fault {
			case "missing profile":
				p.Profile = ""
			case "missing journal":
				p.Journal.UID = ""
			case "wrong journal":
				p.Journal.Name = "borrowed"
			case "wrong namespace":
				p.Journal.Namespace = "other"
			case "missing digest":
				p.JournalSHA256 = ""
			case "bad digest":
				p.JournalSHA256 = strings.Repeat("z", 64)
			case "missing partition":
				p.RawPartitionUUID = ""
			case "missing boot":
				p.BootID = ""
			case "changed boot":
				valueObject(nodes[0], "status", "nodeInfo")["bootID"] = "20000000-0000-4000-8000-000000000001"
			}
			if got, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture()); err == nil || got != nil {
				t.Fatal("incomplete prepared target accepted")
			}
		})
	}
	targets, nodes, storage := nativeRestoreTargetsFixture()
	before, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"journal UID", "journal digest", "native partition"} {
		changed := append([]nativeRestoreTarget{}, targets...)
		switch field {
		case "journal UID":
			changed[0].Preparation.Journal.UID = "new-journal"
		case "journal digest":
			changed[0].Preparation.JournalSHA256 = strings.Repeat("f", 64)
		case "native partition":
			changed[0].Preparation.RawPartitionUUID = "30000000-0000-4000-8000-000000000001"
		}
		after, err := qualifyNativeRestoreTargets(changed, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture())
		if err != nil || reflect.DeepEqual(before, after) {
			t.Fatal("target replacement cannot be distinguished:", field, err)
		}
	}
}
