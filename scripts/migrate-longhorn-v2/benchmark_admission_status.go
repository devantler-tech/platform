package main

import (
	"encoding/json"
	"fmt"
	"net/http"

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
	attributed := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", policy, binding, message)
	qualified := schema.GroupResource{Group: group, Resource: resource}
	return status.Details.Causes[0] == (metav1.StatusCause{Message: attributed}) &&
		status.Message == fmt.Sprintf("%s %q is forbidden: %s", qualified.String(), name, attributed)
}
