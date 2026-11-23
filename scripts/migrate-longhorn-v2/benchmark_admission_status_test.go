package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBenchmarkDenialMustComeFromExactNativePolicy(t *testing.T) {
	const name = "pvc-11111111-1111-4111-8111-111111111111"
	const policy = "lh-benchmark-retire-pv-v1-1234"
	const binding = "lh-benchmark-retire-pv-v1-1234"
	const explanation = "Benchmark backend references are fenced for retirement"
	const cause = "ValidatingAdmissionPolicy '" + policy + "' with binding '" + binding + "' denied request: " + explanation
	messagePrefix := "persistentvolumes " + strconv.Quote(name) + " is forbidden: "
	for _, test := range []struct {
		name   string
		code   int
		edit   func(*metav1.Status)
		proven bool
	}{
		{"exact denial", 403, func(*metav1.Status) {}, true},
		{"successful HTTP response", 200, func(*metav1.Status) {}, false},
		{"unauthenticated", 401, func(s *metav1.Status) { s.Reason = metav1.StatusReasonUnauthorized }, false},
		{"RBAC denial", 403, func(s *metav1.Status) { s.Details.Causes = nil }, false},
		{"only top-level echo", 403, func(s *metav1.Status) { s.Details.Causes[0].Message = "unrelated" }, false},
		{"another binding", 403, func(s *metav1.Status) {
			s.Details.Causes[0].Message = "ValidatingAdmissionPolicy '" + policy + "' with binding 'other' denied request: " + explanation
			s.Message = messagePrefix + s.Details.Causes[0].Message
		}, false},
		{"another policy", 403, func(s *metav1.Status) {
			s.Details.Causes[0].Message = "ValidatingAdmissionPolicy 'other' with binding '" + binding + "' denied request: " + explanation
			s.Message = messagePrefix + s.Details.Causes[0].Message
		}, false},
		{"extra cause field", 403, func(s *metav1.Status) { s.Details.Causes[0].Field = "spec" }, false},
		{"evaluation error", 403, func(s *metav1.Status) {
			s.Details.Causes[0].Message = "ValidatingAdmissionPolicy '" + policy + "' with binding '" + binding + "' denied request: evaluation error"
		}, false},
		{"different resource", 403, func(s *metav1.Status) { s.Details.Kind = "pods" }, false},
		{"different name", 403, func(s *metav1.Status) { s.Details.Name = "other" }, false},
		{"wrong API group", 403, func(s *metav1.Status) { s.Details.Group = "other" }, false},
		{"incomplete status", 403, func(s *metav1.Status) { s.Kind = "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
				Message: messagePrefix + cause,
				Details: &metav1.StatusDetails{Name: name, Kind: "persistentvolumes", Causes: []metav1.StatusCause{{Message: cause}}},
			}
			test.edit(&s)
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if got := benchmarkDenialMatches(test.code, raw, "", "persistentvolumes", name, policy, binding, explanation); got != test.proven {
				t.Fatalf("denial attributed = %t, want %t", got, test.proven)
			}
		})
	}
}
