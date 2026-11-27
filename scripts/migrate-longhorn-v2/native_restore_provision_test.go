package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestNativeRestoreProvisionChecksTheOriginalActivationJournal(t *testing.T) {
	for _, fault := range []string{"none", "replaced", "changed", "deleting", "incomplete target roster"} {
		t.Run(fault, func(t *testing.T) {
			_, _, state := nativeRestoreStateFixture(t, "created")
			record := "original activation record"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(record)))
			for i := range state.Targets {
				state.Targets[i].Preparation.ActivationSHA256 = digest
			}
			id := state.Targets[0].Preparation.Activation
			journal := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{
				"name": id.Name, "namespace": id.Namespace, "uid": id.UID, "resourceVersion": "4",
			}, "data": object{"record": record}}
			switch fault {
			case "replaced":
				valueObject(journal, "metadata")["uid"] = "replacement"
			case "changed":
				valueObject(journal, "data")["record"] = "changed activation record"
			case "deleting":
				valueObject(journal, "metadata")["deletionTimestamp"] = "2026-11-23T00:00:00Z"
			case "incomplete target roster":
				state.Targets = state.Targets[:2]
			}
			calls := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				calls++
				if input != nil || program != "kubectl" || len(args) != 7 || args[2] != "get" || args[3] != "configmap" ||
					args[4] != temporaryActivationJournal || args[6] != "--namespace=longhorn-system" {
					t.Fatal("activation proof escaped its one native read")
				}
				return json.Marshal(journal)
			}}
			r := nativeRestoreRun{c: c, state: state}
			if err := r.checkActivation(t.Context()); (err == nil) != (fault == "none") || calls > 1 {
				t.Fatal("wrong activation identity verdict", err)
			}
			if fault == "incomplete target roster" && calls != 0 {
				t.Fatal("an incomplete restore plan reached the cluster")
			}
		})
	}
}
