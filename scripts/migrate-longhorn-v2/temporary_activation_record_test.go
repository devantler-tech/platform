package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// temporaryActivationRecordFixture gives each selected worker independent
// provider, hardware, partition and preparation identities.
func temporaryActivationRecordFixture(t *testing.T) temporaryActivationRecord {
	t.Helper()
	n, lh, raw, eph, pool := temporaryRawBindingFixture()
	binding, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID"))
	if err != nil {
		t.Fatal(err)
	}
	record := temporaryActivationRecord{Version: 1, Source: strings.Repeat("a", 40), OwnerRun: "123", Targets: map[string]temporaryV2Binding{}}
	for i, selection := range []string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"} {
		b := binding
		b.Selection = selection
		b.Name = fmt.Sprintf("autoscale-longhorn-%s-%016x", strings.Split(selection, "-")[1], i)
		b.NodeUID, b.ProviderID, b.MachineUUID = fmt.Sprintf("node-%d", i), fmt.Sprintf("hcloud://%d", 100+i), fmt.Sprintf("hardware-%d", i)
		b.LonghornUID, b.EphemeralUUID = fmt.Sprintf("longhorn-%d", i), fmt.Sprintf("ephemeral-%d", i)
		b.RawPartitionUUID = fmt.Sprintf("12345678-1234-4234-8234-%012x", i+10)
		b.PreparationUID = fmt.Sprintf("preparation-%d", i)
		record.Targets[selection] = b
	}
	return record
}

// The decoder must reject missing or reused identities and ambiguous JSON
// rather than let a partial or differently interpreted plan authorize storage.
func TestTemporaryActivationRecordRequiresThreeDistinctCompleteBindings(t *testing.T) {
	record := temporaryActivationRecordFixture(t)
	data, _ := json.Marshal(record)
	if _, err := decodeTemporaryActivationRecord(data); err != nil {
		t.Fatal("complete fixed temporary activation record rejected:", err)
	}
	for name, change := range map[string]func(*temporaryActivationRecord){
		"unreviewed source":  func(r *temporaryActivationRecord) { r.Source = "unknown" },
		"missing owner":      func(r *temporaryActivationRecord) { r.OwnerRun = "" },
		"missing third disk": func(r *temporaryActivationRecord) { delete(r.Targets, "temporary-v2-1") },
		"missing preparation identity": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v1-1"]
			b.PreparationUID = ""
			r.Targets["temporary-v1-1"] = b
		},
		"missing preserving preparation": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v2-1"]
			b.PreparationUID = ""
			r.Targets["temporary-v2-1"] = b
		},
		"duplicated preparation identity": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v1-2"]
			b.PreparationUID = r.Targets["temporary-v1-1"].PreparationUID
			r.Targets["temporary-v1-2"] = b
		},
		"wrong target pool": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v2-1"]
			b.Name = r.Targets["temporary-v1-1"].Name
			r.Targets["temporary-v2-1"] = b
		},
		"duplicate provider": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v1-2"]
			b.ProviderID = r.Targets["temporary-v1-1"].ProviderID
			r.Targets["temporary-v1-2"] = b
		},
		"duplicate partition": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v1-2"]
			b.RawPartitionUUID = r.Targets["temporary-v1-1"].RawPartitionUUID
			r.Targets["temporary-v1-2"] = b
		},
		"missing native identity": func(r *temporaryActivationRecord) {
			b := r.Targets["temporary-v1-2"]
			b.RawPartitionUUID = ""
			r.Targets["temporary-v1-2"] = b
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := temporaryActivationRecordFixture(t)
			change(&r)
			data, _ := json.Marshal(r)
			if _, err := decodeTemporaryActivationRecord(data); err == nil {
				t.Fatal("unproven activation record was accepted")
			}
		})
	}
	for name, altered := range map[string]string{
		"duplicate field":   strings.Replace(string(data), `"completed":false`, `"completed":false,"completed":true`, 1),
		"root case alias":   strings.Replace(string(data), `"source":`, `"Source":`, 1),
		"nested case alias": strings.Replace(string(data), `"nodeUID":`, `"NodeUID":`, 1),
		"unknown field":     strings.Replace(string(data), `"version":1`, `"version":1,"extra":true`, 1),
		"trailing document": string(data) + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeTemporaryActivationRecord([]byte(altered)); err == nil {
				t.Fatal("ambiguous activation record was accepted")
			}
		})
	}
}
