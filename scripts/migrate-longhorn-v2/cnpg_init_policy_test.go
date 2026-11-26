package main

import "testing"

func TestRecoveryInitGuardAcceptsNativeNamespaceBoundEntries(t *testing.T) {
	_, entries := isolationFixture(t, "umami-db")
	if revision, err := verifyRecoveryInitPolicy(isolationResponse(t, entries, 817)); err != nil || revision != 817 {
		t.Fatal("native namespace-bound isolation entries were refused")
	}
	for _, subject := range []object{
		{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "one"}},
		{"matchLabels": object{"any:io.kubernetes.pod.namespace": "one"}},
		{"matchExpressions": []any{object{"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": []any{"one", "two"}}}},
		{"matchExpressions": []any{object{"key": "any:io.kubernetes.pod.namespace", "operator": "Exists"}}},
	} {
		entry := object{"Node": false, "Ingress": false, "Verdict": float64(0), "Subject": subject}
		if _, err := verifyRecoveryInitPolicy(isolationResponse(t, []object{entry}, 817)); err != nil {
			t.Fatal("positive namespace requirement failed to exclude reserved:init")
		}
	}
	for _, entry := range []object{
		{"Node": true, "Ingress": false, "Verdict": float64(0), "Subject": object{}},
		{"Node": false, "Ingress": true, "Verdict": float64(0), "Subject": object{}},
		{"Node": false, "Ingress": false, "Verdict": float64(1), "Subject": object{}},
		{"Node": false, "Ingress": false, "Verdict": float64(2), "Subject": object{}},
	} {
		if _, err := verifyRecoveryInitPolicy(isolationResponse(t, []object{entry}, 817)); err != nil {
			t.Fatal("host, ingress, deny or pass rule became an initialization egress allowance")
		}
	}
}

func TestRecoveryInitGuardRejectsPotentialWildcardAndNegativeNamespaceAllows(t *testing.T) {
	for name, subject := range map[string]any{
		"empty clusterwide selector": object{},
		"null selector":              nil,
		"explicit init":              object{"matchLabels": object{"reserved:init": ""}},
		"namespace absent":           object{"matchExpressions": []any{object{"key": "k8s:io.kubernetes.pod.namespace", "operator": "DoesNotExist"}}},
		"negative namespace":         object{"matchExpressions": []any{object{"key": "k8s:io.kubernetes.pod.namespace", "operator": "NotIn", "values": []any{"one"}}}},
		"namespace lookalike":        object{"matchLabels": object{"k8s:io.kubernetes.namespace": "one"}},
		"empty namespace equality":   object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": ""}},
		"empty In values":            object{"matchExpressions": []any{object{"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": []any{}}}},
		"malformed expression":       object{"matchExpressions": "private-value"},
		"unknown selector field":     object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "one"}, "unreviewed": true},
	} {
		t.Run(name, func(t *testing.T) {
			entry := object{"Node": false, "Ingress": false, "Verdict": float64(0), "Subject": subject}
			if _, err := verifyRecoveryInitPolicy(isolationResponse(t, []object{entry}, 817)); err == nil {
				t.Fatal("potential initialization allowance was accepted")
			}
		})
	}
}

func TestRecoveryInitGuardRequiresEveryNativeEntryClassification(t *testing.T) {
	for _, field := range []string{"Node", "Ingress", "Verdict"} {
		entry := object{"Node": false, "Ingress": false, "Verdict": float64(1), "Subject": object{}}
		delete(entry, field)
		if _, err := verifyRecoveryInitPolicy(isolationResponse(t, []object{entry}, 817)); err == nil {
			t.Fatal("missing native classification cleared the repository")
		}
	}
}
