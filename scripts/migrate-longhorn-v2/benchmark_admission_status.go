package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A generic Forbidden is insufficient: the native admission dispatcher
// identifies both the policy and its binding in a structured Status cause.
func benchmarkDenialMatches(code int, raw []byte, group, resource, name, policy, binding, message string) bool {
	if code != http.StatusForbidden || len(raw) > 64<<10 || strictjson.Validate(raw, map[string][]string{
		"":                 {"apiVersion", "kind", "status", "reason", "code", "message", "details"},
		"details":          {"name", "group", "kind", "causes"},
		"details.causes[]": {"message", "reason", "field"},
	}) != nil {
		return false
	}
	var status metav1.Status
	if json.Unmarshal(raw, &status) != nil || status.APIVersion != "v1" || status.Kind != "Status" ||
		status.Status != metav1.StatusFailure || status.Reason != metav1.StatusReasonForbidden ||
		status.Code != http.StatusForbidden || status.Details == nil ||
		status.Details.Group != group || status.Details.Kind != resource || status.Details.Name != name ||
		len(status.Details.Causes) != 1 {
		return false
	}
	cause := status.Details.Causes[0]
	if cause.Type != "" || cause.Field != "" {
		return false
	}
	attribution, hasPrefix := strings.CutPrefix(cause.Message, "ValidatingAdmissionPolicy '")
	denyingPolicy, attribution, hasBinding := strings.Cut(attribution, "' with binding '")
	denyingBinding, denial, hasDenial := strings.Cut(attribution, "' denied request: ")
	if !hasPrefix || !hasBinding || !hasDenial || denyingPolicy != policy || denyingBinding != binding || denial != message {
		return false
	}
	qualified := schema.GroupResource{Group: group, Resource: resource}
	return status.Message == fmt.Sprintf("%s %q is forbidden: %s", qualified.String(), name, cause.Message)
}
