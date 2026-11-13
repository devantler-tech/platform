package main

import (
	"context"
	"encoding/json"
	"testing"
)

func releasedStorage() object {
	return object{"metadata": object{"name": "longhorn", "generation": float64(46)}, "spec": object{"chart": object{"spec": object{"version": "1.13.0"}}}, "status": object{"observedGeneration": float64(46), "lastAttemptedRevision": "1.13.0", "history": []any{object{"chartVersion": "1.13.0"}}, "conditions": []any{object{"type": "Ready", "status": "True"}}}}
}
func TestVersionPreflightRequiresTheObservedReleaseAndReadyNodes(t *testing.T) {
	for _, bad := range []string{"", "unobserved", "missing-generations", "string-generations", "zero-generations", "fractional-generations", "old-release", "failed-release", "wrong-os", "previous-os", "unready-node"} {
		t.Run(bad, func(t *testing.T) {
			hr := releasedStorage()
			switch bad {
			case "unobserved":
				child(hr, "status")["observedGeneration"] = float64(45)
			case "missing-generations":
				delete(child(hr, "metadata"), "generation")
				delete(child(hr, "status"), "observedGeneration")
			case "string-generations":
				child(hr, "metadata")["generation"] = "46"
				child(hr, "status")["observedGeneration"] = "46"
			case "zero-generations":
				child(hr, "metadata")["generation"] = float64(0)
				child(hr, "status")["observedGeneration"] = float64(0)
			case "fractional-generations":
				child(hr, "metadata")["generation"] = 46.5
				child(hr, "status")["observedGeneration"] = 46.5
			case "old-release":
				items(child(hr, "status"), "history")[0]["chartVersion"] = "1.12.0"
			case "failed-release":
				items(child(hr, "status"), "conditions")[0]["status"] = "False"
			}
			node := object{"status": object{"nodeInfo": object{"osImage": "Talos (v1.13.12)"}, "conditions": []any{object{"type": "Ready", "status": "True"}}}}
			if bad == "wrong-os" {
				child(child(node, "status"), "nodeInfo")["osImage"] = "Talos (other)"
			}
			if bad == "previous-os" {
				child(child(node, "status"), "nodeInfo")["osImage"] = "Talos (v1.13.11)"
			}
			if bad == "unready-node" {
				items(child(node, "status"), "conditions")[0]["status"] = "False"
			}
			k := &fakeKubernetes{invoke: func(_ []byte, args []string) ([]byte, error) {
				if args[0] != "get" {
					t.Fatal("preflight mutated the cluster")
				}
				if args[1] == "nodes" {
					return json.Marshal(object{"items": []any{node, node, node}})
				}
				return json.Marshal(hr)
			}}
			r := runner{k: k}
			err := r.preflight(context.Background())
			if (err == nil) != (bad == "") {
				t.Fatal("desired version or partial readiness substituted for the deployed stack")
			}
		})
	}
}
