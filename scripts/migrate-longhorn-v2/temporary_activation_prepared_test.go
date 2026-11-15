package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// temporaryPreparedFixture records an earlier encrypted filesystem and boot,
// both distinct from the post-reset binding used for activation.
func temporaryPreparedFixture(t *testing.T, binding temporaryV2Binding) object {
	t.Helper()
	j := journal{Schema: 2, Target: binding.Name, Source: strings.Repeat("1", 40), OwnerRun: "7777", Phase: "prepared",
		NodeUID: binding.NodeUID, ProviderID: binding.ProviderID, MachineUUID: binding.MachineUUID, LonghornUID: binding.LonghornUID,
		EphemeralUUID: "original-ephemeral", OriginalDisks: object{}, TemporarySelection: binding.Selection,
		NativeEmptyDigest: strings.Repeat("c", 64), OriginalBootID: "12345678-1234-4234-8234-000000000099", RawVolumeAbsent: true, StagingLayout: true}
	if binding.Selection == "temporary-v2-1" {
		j.Schema, j.RawVolumeAbsent = 3, false
		j.RetainedExtentDigest = strings.Repeat("f", 64)
		j.RetainedRawUUID = "12345678-1234-4234-8234-000000000089"
		j.PreparedRawUUID = binding.RawPartitionUUID
	}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	cm := resource("longhorn-v2-prepare-" + binding.Selection)
	cm["metadata"].(object)["namespace"] = "longhorn-system"
	cm["data"] = object{"record": string(data)}
	return cm
}

// Foreign, unfinished, replaced or ambiguous preparation cannot authorize a
// raw disk, and completion must prove the original filesystem was replaced.
func TestTemporaryActivationRequiresTheCompletedPreparationIdentity(t *testing.T) {
	n, lh, raw, eph, pool := temporaryRawBindingFixture()
	binding, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID"))
	if err != nil {
		t.Fatal(err)
	}
	cm := temporaryPreparedFixture(t, binding)
	if _, err = preparedTemporaryBinding(cm, binding); err != nil {
		t.Fatal("completed matching preparation was refused:", err)
	}
	for name, change := range map[string]func(object){
		"foreign scope":    func(cm object) { cm["metadata"].(object)["namespace"] = "other" },
		"missing identity": func(cm object) { delete(cm["metadata"].(object), "uid") },
		"pending deletion": func(cm object) { cm["metadata"].(object)["deletionTimestamp"] = "2026-11-03T09:00:00Z" },
		"unfinished preparation": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), `"phase":"prepared"`, `"phase":"reset-requested"`, 1)
		},
		"old smaller preparation": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), `,"stagingLayout":true`, "", 1)
		},
		"replaced worker": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), binding.NodeUID, "other-node", 1)
		},
		"unchanged boot": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), "12345678-1234-4234-8234-000000000099", binding.BootID, 1)
		},
		"unchanged ephemeral partition": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), "original-ephemeral", binding.EphemeralUUID, 1)
		},
		"ambiguous phase": func(cm object) {
			cm["data"].(object)["record"] = strings.Replace(text(cm, "data", "record"), `"phase":"prepared"`, `"phase":"reset-requested","phase":"prepared"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cm := temporaryPreparedFixture(t, binding)
			change(cm)
			if _, err := preparedTemporaryBinding(cm, binding); err == nil {
				t.Fatal("incomplete or foreign preparation authorized raw storage")
			}
		})
	}
}

func TestTemporaryActivationRequiresThePreservingPreparationIdentity(t *testing.T) {
	n, lh, raw, eph, pool := temporaryRawBindingFixture()
	binding, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID"))
	if err != nil {
		t.Fatal(err)
	}
	binding.Selection = "temporary-v2-1"
	binding.Name = strings.Replace(binding.Name, "-v1-", "-v2-", 1)
	cm := temporaryPreparedFixture(t, binding)
	var record journal
	if err = json.Unmarshal([]byte(text(cm, "data", "record")), &record); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*journal)
	}{
		{"complete", nil},
		{"missing extent proof", func(r *journal) { r.RetainedExtentDigest = "" }},
		{"unexpected raw absence", func(r *journal) { r.RawVolumeAbsent = true }},
		{"wrong destination", func(r *journal) { r.PreparedRawUUID = "12345678-1234-4234-8234-000000000099" }},
		{"retained partition reused", func(r *journal) { r.RetainedRawUUID = r.PreparedRawUUID }},
		{"unfinished reset", func(r *journal) { r.Phase = "reset-requested" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := record
			if tc.change != nil {
				tc.change(&candidate)
			}
			data, marshalErr := json.Marshal(candidate)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			cm["data"] = object{"record": string(data)}
			if _, err := preparedTemporaryBinding(cm, binding); (err == nil) != (tc.change == nil) {
				t.Fatalf("unexpected preserving-preparation verdict: %v", err)
			}
		})
	}
}
