package securityprogress_test

import (
	"fmt"
	"strings"
	"testing"
)

func policyFamilies(results ...map[string]any) map[string]any {
	items := make([]any, 0, len(results))
	for _, result := range results {
		items = append(items, map[string]any{
			"scope":   map[string]any{"kind": "Deployment", "namespace": "synthetic-demo", "name": "synthetic-web"},
			"results": []any{result},
		})
	}
	return map[string]any{"mode": "policy", "policy": map[string]any{"items": items}, "cluster_policy": map[string]any{"items": []any{}}}
}

func familyResult(source string) map[string]any {
	return map[string]any{"source": source, "policy": "synthetic-policy", "result": "fail", "severity": "high", "timestamp": map[string]any{"seconds": 1}}
}

func TestPolicyFamiliesAllRemainVisible(t *testing.T) {
	families := []string{"kyverno", "KyvernoGeneratingPolicy", "KyvernoMutatingPolicy", "KyvernoValidatingPolicy"}
	results := make([]map[string]any, 0, len(families))
	for _, source := range families {
		result := familyResult(source)
		result["rule"] = "synthetic-rule"
		results = append(results, result)
	}
	out, err := run(t, policyFamilies(results...))
	if err != nil {
		t.Fatalf("family collection failed: %v %s", err, out)
	}
	if !strings.Contains(out, `platform_security_policy_total{status="fail"} 4`) {
		t.Fatalf("policy families omitted or collapsed: %s", out)
	}
	for _, source := range families {
		if !strings.Contains(out, fmt.Sprintf(`source="%s"`, source)) {
			t.Fatalf("source %s is absent: %s", source, out)
		}
	}
}

func TestCELPolicyFamiliesUsePolicyRuleWhenRuleIsAbsent(t *testing.T) {
	for _, source := range []string{"KyvernoGeneratingPolicy", "KyvernoMutatingPolicy", "KyvernoValidatingPolicy"} {
		t.Run(source, func(t *testing.T) {
			out, err := run(t, policyFamilies(familyResult(source)))
			if err != nil || !strings.Contains(out, `rule="(policy)"`) || !strings.Contains(out, `platform_security_policy_total{status="fail"} 1`) {
				t.Fatalf("missing CEL rule hid its policy-level finding: %v %s", err, out)
			}
		})
	}
}

func TestPolicyDeduplicationIncludesSourceFamily(t *testing.T) {
	legacy := familyResult("kyverno")
	legacy["rule"] = "same-rule"
	generating := familyResult("KyvernoGeneratingPolicy")
	generating["rule"] = "same-rule"
	out, err := run(t, policyFamilies(legacy, generating, legacy, generating))
	if err != nil || !strings.Contains(out, `platform_security_policy_total{status="fail"} 2`) {
		t.Fatalf("distinct families were collapsed or duplicate reports inflated counts: %v %s", err, out)
	}
}

func TestMalformedLegacyAndUnknownKyvernoFamiliesFailClosed(t *testing.T) {
	for _, source := range []string{"kyverno", "KyvernoUnknownPolicy", "KyvernoValidatingPolciy"} {
		t.Run(source, func(t *testing.T) {
			invalid := familyResult(source)
			// Keep ordinary legacy evidence present so silently ignoring an
			// unknown family cannot pass merely as an empty-collection error.
			valid := familyResult("kyverno")
			valid["rule"] = "known-good-rule"
			if out, err := run(t, policyFamilies(valid, invalid)); err == nil {
				t.Fatalf("malformed or unknown Kyverno family looked complete: %s", out)
			}
		})
	}
}
