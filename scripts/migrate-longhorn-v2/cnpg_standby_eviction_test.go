package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestUmamiStandbyEvictionPreflightOnlyUsesNativeDryRun(t *testing.T) {
	const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const uid = "ac005841-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, status string
		allowed      bool
	}{
		{name: "umami-db-2", status: "Success", allowed: true},
		{name: "umami-db-2", status: "Failure"},
		{name: "coroot-db-2", status: "Success"},
	} {
		calls := 0
		c := client{context: "admin@prod", run: func(_ context.Context, input []byte, tool string, args ...string) ([]byte, error) {
			switch tool {
			case "git":
				return []byte(source + "\trefs/heads/main\n"), nil
			case "kubectl":
				calls++
				want := []string{"--context=admin@prod", "--request-timeout=20s", "create", "--raw", "/api/v1/namespaces/umami/pods/umami-db-2/eviction?dryRun=All", "-f", "-"}
				if !slices.Equal(args, want) {
					t.Fatalf("unexpected Kubernetes request %v", args)
				}
				var body object
				if err := json.Unmarshal(input, &body); err != nil ||
					text(body, "apiVersion") != "policy/v1" || text(body, "kind") != "Eviction" ||
					text(body, "metadata", "name") != "umami-db-2" || text(body, "metadata", "namespace") != "umami" ||
					text(body, "deleteOptions", "preconditions", "uid") != uid ||
					text(body, "deleteOptions", "preconditions", "resourceVersion") != "23" {
					t.Fatal("eviction request lost its checked API or native Pod identity")
				}
				opts, _ := value(body, "deleteOptions", "dryRun").([]any)
				if !slices.Equal(opts, []any{"All"}) {
					t.Fatal("the eviction body must independently require server dry-run")
				}
				return json.Marshal(object{"apiVersion": "v1", "kind": "Status", "status": tc.status})
			default:
				t.Fatalf("unexpected executable %s", tool)
				return nil, nil
			}
		}}
		witness := cnpgStandbyCutover{Consumer: benchmarkResource{Name: tc.name, Namespace: "umami", UID: uid}, ConsumerVersion: "23"}
		if err := verifyUmamiStandbyEvictionAdmission(t.Context(), c, source, witness); (err == nil) != tc.allowed {
			t.Fatalf("unexpected admission for %s: %v", tc.name, err)
		}
		if tc.name != "umami-db-2" && calls != 0 {
			t.Fatal("foreign standby reached Kubernetes")
		}
	}
}
