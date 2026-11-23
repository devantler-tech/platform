package main

import (
	"maps"
	"testing"
)

func TestBenchmarkReferenceFenceRejectsUnreceiptedPolicy(t *testing.T) {
	const name = "lh-benchmark-retire-pv-v1-102"
	desired := object{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   object{"name": name},
		"spec":       object{"failurePolicy": "Fail"},
	}
	policy := object{
		"apiVersion": desired["apiVersion"], "kind": desired["kind"], "spec": desired["spec"],
		"metadata": object{"name": name, "uid": "policy-uid", "resourceVersion": "17"},
	}
	binding := benchmarkDenyBinding(name, "policy-uid")
	bindingMetadata := value(binding, "metadata").(object)
	bindingMetadata["uid"], bindingMetadata["resourceVersion"] = "binding-uid", "25"
	receipts := benchmarkFenceReceipt{PolicyUID: "policy-uid", BindingUID: "binding-uid"}
	if !matchesOwnedBenchmarkFence(policy, binding, desired, receipts) {
		t.Fatal("the exact journaled policy and binding must qualify")
	}
	if matchesOwnedBenchmarkFence(policy, binding, desired, benchmarkFenceReceipt{}) {
		t.Fatal("an identical name and spec cannot establish ownership")
	}
	bindingMetadata["uid"] = "replacement-binding-uid"
	if matchesOwnedBenchmarkFence(policy, binding, desired, receipts) {
		t.Fatal("the replacement binding must not qualify")
	}
	bindingMetadata["uid"] = "binding-uid"
	policyMetadata := value(policy, "metadata").(object)
	policyMetadata["uid"] = "replacement-policy-uid"
	if matchesOwnedBenchmarkFence(policy, binding, desired, receipts) {
		t.Fatal("the replacement policy must not qualify")
	}
}

func TestBenchmarkFenceJournalRejectsAmbiguousOwnership(t *testing.T) {
	record := retirementFenceFixture(t)
	record.Source = "1234567890abcdef1234567890abcdef12345678"
	names := benchmarkFenceNames(record.Phase, record.Run)
	record.Fences = map[string]benchmarkFenceReceipt{
		names[0]: {PolicyUID: "pv-policy", BindingUID: "pv-binding"},
	}
	record.FencePending = names[1] + "/policy"
	if err := validateBenchmarkFenceReceipts(record); err != nil {
		t.Fatalf("verified intent and existing receipt should coexist: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*benchmarkRecord)
	}{
		{"unknown fence", func(r *benchmarkRecord) { r.Fences["foreign"] = benchmarkFenceReceipt{PolicyUID: "unknown"} }},
		{"duplicate UID", func(r *benchmarkRecord) { r.Fences[names[1]] = benchmarkFenceReceipt{PolicyUID: "pv-policy"} }},
		{"borrowed scratch UID", func(r *benchmarkRecord) { r.Fences[names[1]] = benchmarkFenceReceipt{PolicyUID: r.NamespaceUID} }},
		{"intent over existing policy", func(r *benchmarkRecord) { r.FencePending = names[0] + "/policy" }},
		{"binding intent without policy receipt", func(r *benchmarkRecord) { r.FencePending = names[1] + "/binding" }},
		{"cleaned despite intent", func(r *benchmarkRecord) { r.Cleaned = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := record
			changed.Fences = maps.Clone(record.Fences)
			test.mutate(&changed)
			if err := validateBenchmarkFenceReceipts(changed); err == nil {
				t.Fatal("ambiguous benchmark fence ownership was accepted")
			}
		})
	}
}
