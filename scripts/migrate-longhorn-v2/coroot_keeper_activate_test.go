package main

import (
	"context"
	"errors"
	"testing"
)

func TestKeeperFenceReleaseRechecksDestinationAfterBindingDisappears(t *testing.T) {
	for _, remaining := range []string{"policy only", "still bound", "binding read failed"} {
		t.Run(remaining, func(t *testing.T) {
			checkedClaim := false
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" {
					t.Fatal("a failed destination read must stop before a write")
				}
				switch args[3] {
				case "statefulset":
					return nil, errors.New("the former two-member observation has expired")
				case keeperBindingKind:
					switch remaining {
					case "still bound":
						return []byte(`{"metadata":{"uid":"recorded-binding","resourceVersion":"31"}}`), nil
					case "binding read failed":
						return nil, errors.New("binding presence could not be resolved")
					default:
						return nil, nil
					}
				case keeperPolicyKind:
					return []byte(`{"metadata":{"uid":"recorded-policy","resourceVersion":"12"}}`), nil
				case "persistentvolumeclaim":
					checkedClaim = true
					return nil, errors.New("destination binding is unverified")
				default:
					t.Fatalf("unexpected Kubernetes read: %s", args[3])
					return nil, nil
				}
			}}
			record := corootKeeperCutoverRecord{Phase: "destination-bound",
				NewClaim: benchmarkResource{Name: "keeper-destination", Namespace: corootKeeperNamespace}}
			j := corootKeeperJournal{c: c, record: record}
			fences := corootKeeperFences{pod: object{"metadata": object{"name": "keeper-owned-fence"}}}
			err := j.releaseKeeperPodFence(t.Context(), fences)
			if err == nil || checkedClaim != (remaining == "policy only") {
				t.Fatalf("destination rechecked=%t, error=%v", checkedClaim, err)
			}
		})
	}
}
