package main

import "testing"

func TestNativeRestoreRetirementBindsVeleroArtifacts(t *testing.T) {
	for _, fault := range []string{
		"", "expired original", "replacement claimed the transfer", "unfinished restore journal", "replaced restore", "different backup",
		"wrong restore policy", "replaced modifier", "changed modifier", "changed modifier run",
		"missing transfer", "replaced transfer", "different snapshot", "foreign pod reference",
		"additional transfer", "additional restore owner", "replaced repository",
	} {
		t.Run(fault, func(t *testing.T) {
			record, seed, original, restore, pod, transfer, repository := nativeRestoreTransferFixture(t)
			_, _, state := nativeRestoreStateFixture(t, "completed")
			state.SeedDigest = original.SeedDigest
			state.Resources["FirstPod"] = benchmarkIdentity(pod)
			state.Resources["PodVolumeRestore"] = benchmarkIdentity(transfer)
			if fault == "expired original" || fault == "replacement claimed the transfer" {
				originalPod := state.Resources["FirstPod"]
				replacement := originalPod
				replacement.UID = "verified-replacement-pod"
				state.Resources["FirstPod"] = replacement
				state.Expired = &nativeRestoreExpiredReceipt{Pod: originalPod, Transfer: state.Resources["PodVolumeRestore"], Replacement: replacement}
				if fault == "replacement claimed the transfer" {
					valueObject(transfer, "spec", "pod")["uid"] = replacement.UID
				}
			}

			targets := make(map[string]string, len(state.Targets))
			for _, target := range state.Targets {
				targets[target.Name] = target.UID
			}
			assets, err := nativeRestoreAssets(record, seed, state.Run, targets)
			if err != nil {
				t.Fatal(err)
			}
			var modifier object
			for _, asset := range assets {
				if text(asset, "kind") == "ConfigMap" {
					modifier = asset
				}
			}
			valueObject(modifier, "metadata")["uid"] = state.Resources["ConfigMap"].UID
			valueObject(modifier, "metadata")["resourceVersion"] = "40"
			transfers := []object{transfer}
			if state.validate(record, seed) != nil {
				t.Fatal("completed journal fixture is invalid")
			}

			switch fault {
			case "unfinished restore journal":
				state.Phase = "reattach-verified"
				state.CompletedAt = ""
			case "replaced restore":
				valueObject(restore, "metadata")["uid"] = "replacement"
			case "different backup":
				valueObject(restore, "spec")["backupName"] = "different-backup"
			case "wrong restore policy":
				valueObject(restore, "spec")["restorePVs"] = true
			case "replaced modifier":
				valueObject(modifier, "metadata")["uid"] = "replacement"
			case "changed modifier":
				valueObject(modifier, "data")["modifiers.json"] = "{}"
			case "changed modifier run":
				valueObject(modifier, "metadata", "labels")["storage.platform.devantler.tech/benchmark-run"] = record.Run
			case "missing transfer":
				transfers = []object{}
			case "replaced transfer":
				valueObject(transfer, "metadata")["uid"] = "replacement"
			case "different snapshot":
				valueObject(transfer, "spec")["snapshotID"] = "different-snapshot"
			case "foreign pod reference":
				valueObject(transfer, "spec", "pod")["uid"] = state.Resources["SecondPod"].UID
			case "additional transfer":
				transfers = append(transfers, transfer)
			case "additional restore owner":
				meta := valueObject(transfer, "metadata")
				refs := meta["ownerReferences"].([]any)
				meta["ownerReferences"] = append(refs, object{"kind": "Backup", "uid": seed.Backup.UID})
			case "replaced repository":
				valueObject(repository, "metadata")["uid"] = "replacement"
			}

			err = verifyNativeRestoreRetirementVelero(record, seed, state, restore, modifier, transfers, []object{repository})
			if (err != nil) != (fault != "" && fault != "expired original") {
				t.Fatalf("restore retirement verdict for %q: %v", fault, err)
			}
		})
	}
}
