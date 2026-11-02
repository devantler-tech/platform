package main

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type object = map[string]any

func emptySelector(value any) bool {
	if value == nil {
		return true
	}
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range m {
		switch key {
		case "matchLabels":
			labels, ok := value.(map[string]any)
			if !ok || len(labels) != 0 {
				return false
			}
		case "matchExpressions":
			expressions, ok := value.([]any)
			if !ok || len(expressions) != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func auditedNamespaceSelector(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if labels, present := m["matchLabels"]; present && !emptySelector(map[string]any{"matchLabels": labels}) {
		return false
	}
	for key := range m {
		if key != "matchLabels" && key != "matchExpressions" {
			return false
		}
	}
	expressions, ok := m["matchExpressions"].([]any)
	if !ok || len(expressions) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, expression := range expressions {
		values, ok := at(expression, "values").([]any)
		if text(expression, "key") != "kubernetes.io/metadata.name" || text(expression, "operator") != "NotIn" || !ok || len(values) != 1 {
			return false
		}
		name, ok := values[0].(string)
		if !ok || (name != "kube-system" && name != "kyverno") || seen[name] {
			return false
		}
		seen[name] = true
	}
	return seen["kube-system"] && seen["kyverno"]
}

func includes(value any, want string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == want || item == "*" {
			return true
		}
	}
	return false
}

func supportsAdmissionV1(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == "v1" {
			return true
		}
	}
	return false
}

func qualifyWebhook(raw []byte) (object, error) {
	v, err := nativeObject(raw, "admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "kyverno-resource-validating-webhook-cfg", "")
	if err != nil {
		return nil, err
	}
	webhooks, ok := v["webhooks"].([]any)
	if !ok {
		return nil, errors.New("native webhook inventory is missing")
	}
	count := 0
	for _, webhook := range webhooks {
		if text(webhook, "name") != validatingWebhook {
			continue
		}
		count++
		if text(webhook, "failurePolicy") != "Fail" || text(webhook, "sideEffects") != "NoneOnDryRun" || at(webhook, "timeoutSeconds") != float64(30) || !supportsAdmissionV1(at(webhook, "admissionReviewVersions")) || text(webhook, "clientConfig", "service", "name") != "kyverno-svc" || text(webhook, "clientConfig", "service", "namespace") != "kyverno" || text(webhook, "clientConfig", "service", "path") != "/ivpol/validate/verify-app-images" || at(webhook, "clientConfig", "service", "port") != float64(443) || text(webhook, "clientConfig", "caBundle") == "" || at(webhook, "clientConfig", "url") != nil || !auditedNamespaceSelector(at(webhook, "namespaceSelector")) || !emptySelector(at(webhook, "objectSelector")) {
			return nil, errors.New("native image webhook scope or service differs from the audited configuration")
		}
		if conditions := at(webhook, "matchConditions"); conditions != nil {
			actual, ok := conditions.([]any)
			if !ok || len(actual) > 0 {
				return nil, errors.New("native webhook has unqualified match conditions")
			}
		}
		rules, ok := at(webhook, "rules").([]any)
		if !ok {
			return nil, errors.New("native webhook rules are missing")
		}
		covered := false
		for _, rule := range rules {
			scope := text(rule, "scope")
			if (scope == "" || scope == "*" || scope == "Namespaced") && includes(at(rule, "operations"), "CREATE") && includes(at(rule, "apiGroups"), "") && includes(at(rule, "apiVersions"), "v1") && includes(at(rule, "resources"), "pods") {
				covered = true
			}
		}
		if !covered {
			return nil, errors.New("native webhook does not cover namespaced Pod creation")
		}
	}
	if count != 1 {
		return nil, errors.New("native image webhook is missing or duplicated")
	}
	return v, nil
}

func at(value any, path ...string) any {
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}
func text(value any, path ...string) string { v, _ := at(value, path...).(string); return v }

func decodeObject(raw []byte) (object, error) {
	var value object
	if len(raw) > 2*1024*1024 || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("native response is incomplete or ambiguous")
	}
	return value, nil
}

func identified(value object) bool {
	return text(value, "metadata", "name") != "" && text(value, "metadata", "uid") != "" && text(value, "metadata", "resourceVersion") != "" && at(value, "metadata", "deletionTimestamp") == nil
}

func nativeObject(raw []byte, version, kind, name, namespace string) (object, error) {
	v, err := decodeObject(raw)
	if err != nil || text(v, "apiVersion") != version || text(v, "kind") != kind || text(v, "metadata", "name") != name || text(v, "metadata", "namespace") != namespace || !identified(v) {
		return nil, errors.New("native object binding is incomplete")
	}
	return v, nil
}

func qualifyPolicy(raw, desired []byte) (object, error) {
	v, err := nativeObject(raw, "policies.kyverno.io/v1", "ImageValidatingPolicy", "verify-app-images", "")
	if err != nil {
		return nil, err
	}
	var spec object
	if strictjson.Validate(desired, nil) != nil || json.Unmarshal(desired, &spec) != nil || spec == nil || !reflect.DeepEqual(v["spec"], spec) || text(v, "spec", "failurePolicy") != "Fail" || at(v, "spec", "evaluation", "admission", "enabled") != true || at(v, "status", "conditionStatus", "ready") != true {
		return nil, errors.New("admission policy differs from reviewed source or is unready")
	}
	conditions, ok := at(v, "status", "conditionStatus", "conditions").([]any)
	if !ok {
		return nil, errors.New("native policy readiness conditions are missing")
	}
	for _, kind := range []string{"WebhookConfigured", "RBACPermissionsGranted"} {
		count := 0
		for _, condition := range conditions {
			if text(condition, "type") == kind {
				if text(condition, "status") != "True" {
					return nil, errors.New("native policy readiness condition is not true")
				}
				count++
			}
		}
		if count != 1 {
			return nil, errors.New("native policy readiness is incomplete or ambiguous")
		}
	}
	return v, nil
}

func completeList(raw []byte, version, kind string) ([]object, error) {
	v, err := decodeObject(raw)
	if err != nil || text(v, "apiVersion") != version || text(v, "kind") != kind || text(v, "metadata", "resourceVersion") == "" {
		return nil, errors.New("native inventory envelope is incomplete")
	}
	metadata, ok := v["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("native inventory metadata is missing")
	}
	if continuation, present := metadata["continue"]; present && continuation != "" {
		return nil, errors.New("native inventory has another page")
	}
	if remaining, present := metadata["remainingItemCount"]; present && remaining != float64(0) {
		return nil, errors.New("native inventory is not complete")
	}
	items, ok := v["items"].([]any)
	if !ok || len(items) > 1000 {
		return nil, errors.New("native inventory items are missing or exceed the bound")
	}
	result := make([]object, 0, len(items))
	names, uids := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		o, ok := item.(map[string]any)
		if !ok || !identified(o) {
			return nil, errors.New("native inventory member identity is incomplete")
		}
		key := text(o, "metadata", "namespace") + "/" + text(o, "metadata", "name")
		uid := text(o, "metadata", "uid")
		if names[key] || uids[uid] {
			return nil, errors.New("native inventory contains duplicate members")
		}
		names[key], uids[uid] = true, true
		result = append(result, o)
	}
	return result, nil
}
