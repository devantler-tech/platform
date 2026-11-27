package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUmamiNameFenceAcceptsOnlyTheNativeInvalidDenial(t *testing.T) {
	const policy = "lh-umami-claim-release-1234"
	const claim = "umami-db-1"
	for _, tc := range []struct {
		name, mutate string
		proven       bool
	}{
		{name: "exact native policy denial", proven: true},
		{name: "another admission policy", mutate: "policy"},
		{name: "ordinary API validation failure", mutate: "validation"},
		{name: "unverified binding", mutate: "binding"},
		{name: "foreign resource identity", mutate: "name"},
		{name: "wrong native status", mutate: "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.RawQuery != "dryRun=All" {
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				resource := path[len(path)-1]
				var group, name string
				switch resource {
				case "pods":
					name = "longhorn-v2-name-fence-probe"
				case "jobs":
					group, name = "batch", claim+"-join"
				case "persistentvolumeclaims":
					name = claim
				default:
					t.Errorf("unexpected admission path %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				code, reason := http.StatusUnprocessableEntity, "Invalid"
				policyName, bindingName := policy, policy
				denial := umamiNameFenceMessage
				switch tc.mutate {
				case "policy":
					policyName = "another-policy"
				case "validation":
					denial = "ordinary validation failure"
				case "binding":
					bindingName = "another-binding"
				case "name":
					name = "another-object"
				case "status":
					code, reason = http.StatusForbidden, "Forbidden"
				}
				cause := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", policyName, bindingName, denial)
				details := object{"kind": resource, "name": name, "causes": []any{object{"message": cause}}}
				qualified := resource
				if group != "" {
					details["group"] = group
					qualified += "." + group
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				// This is the response shape emitted by the native API while
				// the target PVC and Join job still exist.
				if err := json.NewEncoder(w).Encode(object{"apiVersion": "v1", "kind": "Status", "metadata": object{},
					"status": "Failure", "reason": reason, "code": code,
					"message": fmt.Sprintf("%s %q is forbidden: %s", qualified, name, cause), "details": details}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			profile, _ := cnpgStandbyProfileForSource(cnpgSource{Namespace: "umami", Name: "umami-db"})
			err = probeCNPGNameFence(context.Background(), server.Client(), endpoint, profile, policy, claim)
			if (err == nil) != tc.proven {
				t.Fatalf("admission probe result = %v, want success = %t", err, tc.proven)
			}
			if tc.proven && calls.Load() != 3 {
				t.Fatalf("proved %d resources, want three", calls.Load())
			}
		})
	}
}
