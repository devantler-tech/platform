package main

import (
	"encoding/json"
	"testing"
)

func webhookFixture(t *testing.T, change func(object)) []byte {
	t.Helper()
	w := object{"name": validatingWebhook, "failurePolicy": "Fail", "sideEffects": "NoneOnDryRun", "timeoutSeconds": 30, "admissionReviewVersions": []any{"v1"}, "clientConfig": object{"caBundle": "c3ludGhldGljLWNh", "service": object{"name": "kyverno-svc", "namespace": "kyverno", "port": 443, "path": "/ivpol/validate/verify-app-images"}}, "namespaceSelector": object{"matchExpressions": []any{object{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []any{"kube-system"}}, object{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []any{"kyverno"}}}}, "rules": []any{object{"operations": []any{"CREATE"}, "apiGroups": []any{""}, "apiVersions": []any{"v1"}, "resources": []any{"pods"}, "scope": "Namespaced"}}}
	if change != nil {
		change(w)
	}
	v := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration", "metadata": object{"name": "kyverno-resource-validating-webhook-cfg", "uid": "webhook-1", "resourceVersion": "31"}, "webhooks": []any{w}}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWebhookActuallyCoversTheDryRunPod(t *testing.T) {
	if _, err := qualifyWebhook(webhookFixture(t, nil)); err != nil {
		t.Fatalf("qualified native webhook rejected: %v", err)
	}
	for name, change := range map[string]func(object){
		"fail open":                     func(v object) { v["failurePolicy"] = "Ignore" },
		"unsupported admission version": func(v object) { v["admissionReviewVersions"] = []any{"*"} },
		"external target":               func(v object) { v["clientConfig"].(object)["url"] = "https://foreign.invalid" },
		"wrong service":                 func(v object) { v["clientConfig"].(object)["service"].(object)["name"] = "other" },
		"wrong path":                    func(v object) { v["clientConfig"].(object)["service"].(object)["path"] = "/other" },
		"default excluded": func(v object) {
			v["namespaceSelector"].(object)["matchExpressions"].([]any)[0].(object)["values"] = []any{"default"}
		},
		"object filtered":    func(v object) { v["objectSelector"] = object{"matchLabels": object{"skip": "true"}} },
		"pods not covered":   func(v object) { v["rules"].([]any)[0].(object)["resources"] = []any{"configmaps"} },
		"create not covered": func(v object) { v["rules"].([]any)[0].(object)["operations"] = []any{"UPDATE"} },
		"unknown condition":  func(v object) { v["matchConditions"] = []any{object{"name": "skip-positive", "expression": "false"}} },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := qualifyWebhook(webhookFixture(t, change)); err == nil {
				t.Fatal("unqualified webhook accepted")
			}
		})
	}
}
