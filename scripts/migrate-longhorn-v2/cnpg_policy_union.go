package main

import (
	_ "embed"
	"encoding/json"
	"errors"
)

// Native Cilium 1.20.5 output for the reviewed namespace default-deny and DNS
// generation rules. These fixed effects grant only the existing resolver path;
// empty-peer rules grant nothing. See the fixture provenance document.
//
//go:embed testdata/cnpg-baseline-entries-cilium-1.20.5.json
var recoveryBaselinePolicies []byte

// verifyRecoveryPolicyAllowances checks every potentially applicable allowance,
// including rules without the owned policy's labels. Effects must be identical
// to an approved imported allowance; other namespaces and restrictive denies do
// not expand this namespace's access. Unknown shapes or tiers fail closed.
func verifyRecoveryPolicyAllowances(entries, expected []object, namespace string) error {
	refused := errors.New("another native policy may widen the recovery namespace")
	effect := func(entry object) string {
		body := object{}
		for key, value := range entry {
			if key != "Labels" && key != "Subject" {
				body[key] = value
			}
		}
		// Kubernetes NetworkPolicy has nil peers and ports where a Cilium
		// NetworkPolicy default-deny has empty lists. Both grant no traffic.
		// Retain the explicit field presence and match all other fields.
		peers, hasPeers := body["L3"]
		ports, hasPorts := body["L4"]
		if body["DefaultDeny"] == true && hasPeers && peers == nil && hasPorts && ports == nil {
			body["L3"], body["L4"] = []any{}, []any{}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return ""
		}
		return string(raw)
	}
	approved := map[string]bool{}
	var baseline []object
	if len(expected) == 0 || decodeIsolationJSON(recoveryBaselinePolicies, &baseline) != nil || len(baseline) != 3 {
		return refused
	}
	for _, rules := range [][]object{expected, baseline} {
		for _, entry := range rules {
			if verdict, valid := nativeObservationUint(entry["Verdict"]); valid && verdict == 0 {
				approved[effect(entry)] = true
			}
		}
	}
	delete(approved, "")
	for _, entry := range entries {
		node, validNode := entry["Node"].(bool)
		verdict, validVerdict := nativeObservationUint(entry["Verdict"])
		if !validNode || !validVerdict || verdict > 2 {
			return refused
		}
		if node || verdict == 1 {
			continue
		}
		subject, valid := entry["Subject"].(object)
		if !valid || !recoverySubjectExcludesInit(subject) {
			return refused
		}
		if !recoverySubjectMaySelectNamespace(subject, namespace) {
			continue
		}
		if verdict != 0 || !approved[effect(entry)] {
			return refused
		}
	}
	return nil
}

// The caller has already validated the native conjunctive selector and its
// positive namespace constraint. Treat other Pod labels as potentially matching.
func recoverySubjectMaySelectNamespace(subject object, namespace string) bool {
	labels, _ := subject["matchLabels"].(object)
	for key, raw := range labels {
		if recoveryNamespaceSelectorKey(key) && raw != namespace {
			return false
		}
	}
	expressions, _ := subject["matchExpressions"].([]any)
	for _, raw := range expressions {
		expression, _ := raw.(object)
		if !recoveryNamespaceSelectorKey(text(expression, "key")) {
			continue
		}
		values, _ := expression["values"].([]any)
		contains := false
		for _, value := range values {
			contains = contains || value == namespace
		}
		switch text(expression, "operator") {
		case "In":
			if !contains {
				return false
			}
		case "NotIn":
			if contains {
				return false
			}
		case "DoesNotExist":
			return false
		}
	}
	return true
}
