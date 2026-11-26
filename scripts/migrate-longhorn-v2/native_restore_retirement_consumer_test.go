package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReclaimWaitsForTheRecordedTerminatingConsumer(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed termination", true: "replacement Pod"}[replace], func(t *testing.T) {
			_, _, state := nativeRestoreStateFixture(t, "completed")
			id := state.Resources["SecondPod"]
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, binary string, args ...string) ([]byte, error) {
				if input != nil || binary != "kubectl" || !strings.Contains(strings.Join(args, " "), " get pod fio ") {
					t.Fatalf("waiting for the owned consumer ran an unexpected command: %s %v", binary, args)
				}
				reads++
				uid := id.UID
				if reads > 1 {
					if !replace {
						return nil, nil
					}
					uid = "replacement-pod-uid"
				}
				return json.Marshal(object{"apiVersion": "v1", "kind": "Pod",
					"metadata": object{"name": id.Name, "namespace": id.Namespace, "uid": uid, "resourceVersion": "25",
						"labels":            object{"storage.platform.devantler.tech/benchmark-run": state.Run},
						"deletionTimestamp": "2026-11-26T12:40:00Z"},
					"spec": object{"nodeName": state.SecondNode},
				})
			}}
			reclaim := nativeRestoreReclaim{c: c, inputs: nativeRestoreReclaimInputs{state: state}}
			err := reclaim.removeConsumer(t.Context())
			if (err != nil) != replace || reads != 2 {
				t.Fatalf("terminating consumer readback: err=%v reads=%d", err, reads)
			}
		})
	}
}
