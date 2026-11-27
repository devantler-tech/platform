package main

import "errors"

func recoveryNamespaceSelectorKey(key string) bool {
	return key == "k8s:io.kubernetes.pod.namespace" || key == "any:io.kubernetes.pod.namespace"
}

// Native LabelSelector requirements are conjunctive. Requiring a namespace
// excludes reserved:init, which does not yet have that label. Unknown selector
// shapes fail closed. This deliberately recognizes a small safe subset.
func recoverySubjectExcludesInit(subject object) bool {
	for key := range subject {
		if key != "matchLabels" && key != "matchExpressions" {
			return false
		}
	}
	positive := false
	if raw := subject["matchLabels"]; raw != nil {
		labels, valid := raw.(object)
		if !valid {
			return false
		}
		for key, raw := range labels {
			label, valid := raw.(string)
			if key == "" || !valid {
				return false
			}
			positive = positive || recoveryNamespaceSelectorKey(key) && label != ""
		}
	}
	if raw := subject["matchExpressions"]; raw != nil {
		expressions, valid := raw.([]any)
		if !valid {
			return false
		}
		for _, raw := range expressions {
			expression, valid := raw.(object)
			if !valid || text(expression, "key") == "" {
				return false
			}
			for key := range expression {
				if key != "key" && key != "operator" && key != "values" {
					return false
				}
			}
			var values []any
			if raw := expression["values"]; raw != nil {
				values, valid = raw.([]any)
				if !valid {
					return false
				}
				for _, raw := range values {
					if _, valid := raw.(string); !valid {
						return false
					}
				}
			}
			operator := text(expression, "operator")
			switch operator {
			case "In", "NotIn":
				if len(values) == 0 {
					return false
				}
			case "Exists", "DoesNotExist":
				if len(values) != 0 {
					return false
				}
			default:
				return false
			}
			positive = positive || recoveryNamespaceSelectorKey(text(expression, "key")) && (operator == "In" || operator == "Exists")
		}
	}
	return positive
}

// Before creating a recovery Cluster, use the same fresh full repository
// readback that proves its CNP import. Every potentially init-selecting egress
// allowance holds, even a harmless DNS-only rule. No peer/port/tier inference
// substitutes for this conservative check. Native runtime RestoreState=true,
// a fresh namespace and a stable selected agent remain separate obligations.
func verifyRecoveryInitPolicy(raw []byte) (uint64, error) {
	refused := errors.New("native policy repository may allow initialization egress")
	revision, entries, err := decodeRecoveryPolicyRepository(raw)
	if err != nil {
		return 0, refused
	}
	if err := verifyRecoveryInitEntries(entries); err != nil {
		return 0, err
	}
	return revision, nil
}

func verifyRecoveryInitEntries(entries []object) error {
	refused := errors.New("native policy repository may allow initialization egress")
	for _, entry := range entries {
		node, nodeOK := entry["Node"].(bool)
		ingress, ingressOK := entry["Ingress"].(bool)
		verdict, verdictOK := nativeObservationUint(entry["Verdict"])
		if !nodeOK || !ingressOK || !verdictOK || verdict > 2 {
			return refused
		}
		if !node && !ingress && verdict == 0 {
			subject, valid := entry["Subject"].(object)
			if !valid || !recoverySubjectExcludesInit(subject) {
				return refused
			}
		}
	}
	return nil
}
