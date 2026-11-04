package trivyruntimedispositions

import (
	"fmt"
	"strings"
)

// These grants have exact namespaced consumers, not general controller access.
var rbacRoles = map[string]bool{
	"prune-protected-orphan-alert":                               true,
	"kyverno:background-controller:mutate-csi-sidecars":          true,
	"kyverno:background-controller:mutate-longhorn-csi-plugin":   true,
	"kyverno:background-controller:mutate-longhorn-engine-image": true,
}

func checkRBACOwner(path string, obj map[string]any) error {
	if _, known := sourcePins[path]; known {
		return nil
	} // complete object already checked
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if (kind == "Role" || kind == "ClusterRole") && rbacRoles[name] {
		return fmt.Errorf("%s: new reviewed-role definition", path)
	}
	if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
		ref, _ := obj["roleRef"].(map[string]any)
		refName, _ := ref["name"].(string)
		if rbacRoles[refName] {
			return fmt.Errorf("%s: new reviewed-role binding", path)
		}
		subjects, _ := obj["subjects"].([]any)
		for _, raw := range subjects {
			subject, _ := raw.(map[string]any)
			if subject["kind"] == "ServiceAccount" && (subject["name"] == "kyverno-background-controller" || subject["name"] == "prune-protected-orphan-alert") ||
				subject["kind"] == "Group" && subject["name"] == "system:serviceaccounts:kyverno" ||
				subject["kind"] == "User" && (subject["name"] == "system:serviceaccount:kyverno:kyverno-background-controller" || subject["name"] == "system:serviceaccount:observability:prune-protected-orphan-alert") {
				return fmt.Errorf("%s: new reviewed-controller binding", path)
			}
		}
	}
	if kind == "Pod" || kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "Job" || kind == "CronJob" {
		spec, _ := obj["spec"].(map[string]any)
		if kind == "CronJob" {
			job, _ := spec["jobTemplate"].(map[string]any)
			spec, _ = job["spec"].(map[string]any)
		}
		if kind != "Pod" {
			template, _ := spec["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		}
		if spec["serviceAccountName"] == "prune-protected-orphan-alert" || spec["serviceAccountName"] == "kyverno-background-controller" {
			return fmt.Errorf("%s: new state-writer identity consumer", path)
		}
	}
	if kind == "ClusterPolicy" || kind == "Policy" {
		spec, _ := obj["spec"].(map[string]any)
		rules, _ := spec["rules"].([]any)
		for _, raw := range rules {
			rule, _ := raw.(map[string]any)
			mutate, _ := rule["mutate"].(map[string]any)
			targets, _ := mutate["targets"].([]any)
			for _, rawTarget := range targets {
				target, _ := rawTarget.(map[string]any)
				targetKind, _ := target["kind"].(string)
				namespace, _ := target["namespace"].(string)
				// Static different namespaces/kinds do not use these grants. A wildcard,
				// omitted or computed target that could reach Longhorn must be reviewed.
				namespaceCanMatch := namespace == "" || namespace == "longhorn-system" || strings.ContainsAny(namespace, "*{")
				kindCanMatch := targetKind == "Deployment" || targetKind == "DaemonSet" || targetKind == "" || strings.ContainsAny(targetKind, "*{")
				if namespaceCanMatch && kindCanMatch {
					return fmt.Errorf("%s: new Longhorn mutation-policy consumer", path)
				}
			}
		}
	}
	return nil
}
