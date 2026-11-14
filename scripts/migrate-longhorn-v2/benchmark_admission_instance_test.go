package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBenchmarkInstanceAdmissionRequiresExactNativeDenial(t *testing.T) {
	for _, fault := range []string{"", "changed child", "stale checkpoint", "generic denial", "negative accepted"} {
		t.Run(fault, func(t *testing.T) {
			record, groups := retirementInstanceFixture(t)
			children, err := qualifyRetirementInstances(record, groups)
			if err != nil {
				t.Fatal(err)
			}
			child := children[slices.IndexFunc(children, func(child retirementChild) bool { return child.Resource == "engines" })]
			actual := groups["engines"][0]
			actual["kind"], actual["apiVersion"] = "Engine", "longhorn.io/v1beta2"
			checkpoint := record.Run + "-" + strings.Repeat("a", 32)
			policy := "lh-benchmark-retire-instances-" + record.Phase + "-" + record.Run
			path := "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/engines/" + child.Identity.Name
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPatch || req.URL.Path != path || req.URL.RawQuery != "dryRun=All" {
					t.Error("unexpected native admission request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var ops []object
				if err := json.NewDecoder(req.Body).Decode(&ops); err != nil || len(ops) != 4 ||
					text(ops[0], "value") != child.Identity.UID || text(ops[1], "value") != child.ResourceVersion {
					t.Error("admission request was not scoped to the child UID and revision")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				current := calls.Add(1)
				if current == 1 || fault == "negative accepted" {
					out := cloneBenchmarkProbeObject(t, actual)
					annotations := object{retirementCheckpointAnnotation: text(ops[3], "value")}
					valueObject(out, "metadata")["annotations"] = annotations
					if fault == "stale checkpoint" {
						annotations[retirementCheckpointAnnotation] = "old"
					}
					_ = json.NewEncoder(w).Encode(out)
					return
				}
				cause := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", policy, policy, benchmarkInstanceFenceMessage)
				if fault == "generic denial" {
					cause = "denied by unrelated admission"
				}
				status := metav1.Status{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
					Status:   "Failure", Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
					Message: fmt.Sprintf("engines.longhorn.io %q is forbidden: %s", child.Identity.Name, cause),
					Details: &metav1.StatusDetails{Name: child.Identity.Name, Group: "longhorn.io", Kind: "engines", Causes: []metav1.StatusCause{{Message: cause}}},
				}
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(status)
			}))
			defer server.Close()
			if fault == "changed child" {
				valueObject(actual, "metadata")["uid"] = "foreign"
			}
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = probeBenchmarkInstanceAdmission(context.Background(), server.Client(), endpoint, child, actual, policy, checkpoint)
			if (err == nil) != (fault == "") {
				t.Fatalf("native proof result was %v for fault %q", err, fault)
			}
			want := int32(2)
			if fault == "changed child" {
				want = 0
			}
			if fault == "stale checkpoint" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("got %d probes, want %d", calls.Load(), want)
			}
		})
	}
}

func cloneBenchmarkProbeObject(t *testing.T, original object) object {
	t.Helper()
	body, err := json.Marshal(original)
	if err != nil {
		t.Error(err)
		return nil
	}
	var result object
	if err := json.Unmarshal(body, &result); err != nil {
		t.Error(err)
	}
	return result
}
