package main

import "testing"

func TestNativeRestoreManagersBindExactlyOneV2PodPerActivatedTarget(t *testing.T) {
	for _, fault := range []string{"none", "missing Pod", "wrong engine", "duplicate manager for target", "foreign pod epoch"} {
		t.Run(fault, func(t *testing.T) {
			targets, _, storage := nativeRestoreTargetsFixture()
			var managers, pods []object
			for i, target := range targets {
				manager, pod := nativeRestoreAttachedManagerFixture(target, i)
				managers, pods = append(managers, manager), append(pods, pod)
			}
			switch fault {
			case "missing Pod":
				pods = pods[:2]
			case "wrong engine":
				valueObject(managers[2], "spec")["dataEngine"] = "v1"
			case "duplicate manager for target":
				manager, pod := nativeRestoreManagerObjects(targets[2], "instance-manager-extra", "extra-manager", "extra-pod", "a")
				managers, pods = append(managers, manager), append(pods, pod)
			case "foreign pod epoch":
				valueObject(pods[2], "metadata")["uid"] = text(pods[1], "metadata", "uid")
			}
			bound, err := qualifyNativeRestoreManagers(targets, storage, managers, pods)
			if fault == "none" {
				if err != nil || len(bound) != 3 || bound[targets[2].Name].PodUID != text(pods[2], "metadata", "uid") {
					t.Fatal("the activated pool did not bind its complete V2 cohort:", err)
				}
			} else if err == nil || bound != nil {
				t.Fatal("an incomplete or ambiguous manager cohort was admitted")
			}
		})
	}
}
