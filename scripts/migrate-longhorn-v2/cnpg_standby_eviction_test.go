package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestUmamiStandbyEvictionPreflightOnlyUsesNativeDryRun(t *testing.T) {
	const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name, status, change string
		allowed              bool
		mustNotReachEviction bool
	}{
		{name: "umami-db-2", status: "Success", allowed: true},
		{name: "umami-db-2", status: "Failure"},
		{name: "coroot-db-2", status: "Success", mustNotReachEviction: true},
		{name: "umami-db-2", status: "Success", change: "PDB missing before dry-run", mustNotReachEviction: true},
		{name: "umami-db-2", status: "Success", change: "pod changed before dry-run", mustNotReachEviction: true},
		{name: "umami-db-2", status: "Success", change: "PDB removed"},
		{name: "umami-db-2", status: "Success", change: "PDB version changed"},
		{name: "umami-db-2", status: "Success", change: "Pod version changed"},
		{name: "umami-db-2", status: "Success", change: "another claim consumer"},
		{name: "umami-db-2", status: "Success", change: "PV loses Retain"},
	} {
		observation, pod, claim, pv, volume, budget := umamiStandbyCutoverFixture(t)
		pods := observation.Pods
		witness, err := bindUmamiStandbyCutover(observation, pods, "umami-db-2", claim, pv, volume, budget)
		if err != nil {
			t.Fatal(err)
		}
		witness.Consumer.Name = tc.name
		if tc.change == "pod changed before dry-run" {
			valueObject(pod, "metadata")["resourceVersion"] = "24"
		}
		calls := 0
		c := client{context: "admin@prod", run: func(_ context.Context, input []byte, tool string, args ...string) ([]byte, error) {
			switch tool {
			case "git":
				return []byte(source + "\trefs/heads/main\n"), nil
			case "kubectl":
				if args[2] == "get" {
					if len(input) != 0 {
						t.Fatal("unexpected input for observation request")
					}
					switch args[3] {
					case "clusters.postgresql.cnpg.io":
						return json.Marshal(observation.Cluster)
					case "pods":
						return json.Marshal(object{"items": pods})
					case "persistentvolumeclaim":
						return json.Marshal(claim)
					case "persistentvolume":
						return json.Marshal(pv)
					case "volumes.longhorn.io":
						return json.Marshal(volume)
					case "poddisruptionbudget":
						if tc.change == "PDB missing before dry-run" || tc.change == "PDB removed" && calls != 0 {
							return nil, errors.New("object absent")
						}
						return json.Marshal(budget)
					default:
						t.Fatalf("unexpected Kubernetes observation %v", args)
					}
				}
				calls++
				want := []string{"--context=admin@prod", "--request-timeout=20s", "create", "--raw", "/api/v1/namespaces/umami/pods/umami-db-2/eviction?dryRun=All", "-f", "-"}
				if !slices.Equal(args, want) {
					t.Fatalf("unexpected Kubernetes request %v", args)
				}
				var body object
				if err := json.Unmarshal(input, &body); err != nil ||
					text(body, "apiVersion") != "policy/v1" || text(body, "kind") != "Eviction" ||
					text(body, "metadata", "name") != "umami-db-2" || text(body, "metadata", "namespace") != "umami" ||
					text(body, "deleteOptions", "preconditions", "uid") != text(pod, "metadata", "uid") ||
					text(body, "deleteOptions", "preconditions", "resourceVersion") != "23" {
					t.Fatal("eviction request lost its checked API or native Pod identity")
				}
				opts, _ := value(body, "deleteOptions", "dryRun").([]any)
				if !slices.Equal(opts, []any{"All"}) {
					t.Fatal("the eviction body must independently require server dry-run")
				}
				switch tc.change {
				case "PDB version changed":
					valueObject(budget, "metadata")["resourceVersion"] = "51"
				case "Pod version changed":
					valueObject(pod, "metadata")["resourceVersion"] = "24"
				case "another claim consumer":
					pods = append(pods, object{"metadata": object{"name": "foreign", "namespace": "umami", "uid": "foreign-pod"}, "spec": valueObject(pod, "spec")})
				case "PV loses Retain":
					valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
				}
				return json.Marshal(object{"apiVersion": "v1", "kind": "Status", "status": tc.status})
			default:
				t.Fatalf("unexpected executable %s", tool)
				return nil, nil
			}
		}}
		if err := verifyUmamiStandbyEvictionAdmission(t.Context(), c, source, witness); (err == nil) != tc.allowed {
			t.Errorf("unexpected admission for %s (%s): %v", tc.name, tc.change, err)
		}
		if tc.mustNotReachEviction && calls != 0 {
			t.Errorf("unqualified standby reached the Eviction API (%s)", tc.change)
		}
	}
}
