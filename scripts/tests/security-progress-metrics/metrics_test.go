package securityprogress_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, input map[string]any) (string, error) {
	t.Helper()
	path, err := filepath.Abs("../../../k8s/bases/infrastructure/coroot/components/security-progress/metrics.jq")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("jq", "-er", "-f", path)
	c.Stdin = strings.NewReader(string(b))
	o, err := c.CombinedOutput()
	return string(o), err
}

func policy() map[string]any {
	return map[string]any{"mode": "policy", "policy": map[string]any{"items": []any{map[string]any{
		"scope":   map[string]any{"kind": "Deployment", "namespace": "demo", "name": "web"},
		"results": []any{map[string]any{"source": "kyverno", "policy": "safe-pods", "rule": "nonroot", "result": "fail", "timestamp": map[string]any{"seconds": 1}}},
	}}}, "cluster_policy": map[string]any{"items": []any{}}}
}

func TestPolicyDeduplicatesReportsAndKeepsRemainingResource(t *testing.T) {
	p := policy()
	p["cluster_policy"] = p["policy"]
	out, err := run(t, p)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, `platform_security_policy_results{policy="safe-pods",rule="nonroot",namespace="demo",status="fail",severity="unknown"} 1`) {
		t.Fatal(out)
	}
	if !strings.Contains(out, `kind="Deployment",name="web"`) {
		t.Fatal(out)
	}
	if strings.Contains(out, `} 2`) {
		t.Fatalf("duplicate counted twice: %s", out)
	}
}

func TestPolicyEmptyAndUnknownStatusFailClosed(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["policy"] = map[string]any{"items": []any{}} },
		func(p map[string]any) {
			p["policy"].(map[string]any)["items"].([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)["result"] = "unexpected"
		},
	} {
		p := policy()
		mutate(p)
		if out, err := run(t, p); err == nil {
			t.Fatalf("unknown data reported success: %s", out)
		}
	}
}

func kubescape() map[string]any {
	digest := "sha256:" + strings.Repeat("a", 64)
	labels := map[string]any{"kubescape.io/workload-kind": "Deployment", "kubescape.io/workload-namespace": "demo", "kubescape.io/workload-name": "web"}
	meta := map[string]any{"namespace": "demo", "name": "web-image", "resourceVersion": "7", "annotations": map[string]any{"kubescape.io/image-id": "registry.test/web@" + digest, "kubescape.io/timestamp": "1790000000"}}
	v := map[string]any{"metadata": meta, "spec": map[string]any{"severities": map[string]any{"critical": map[string]any{"all": 3, "relevant": 1}}}}
	list := func(items ...any) map[string]any { return map[string]any{"items": items} }
	return map[string]any{
		"mode": "kubescape", "now": 1790000100, "unscanned": []string{"kube-system"},
		"coverage_report": "COVERAGE posture expected=1 current=1 missing=0 empty=0 orphan=0\nCOVERAGE vulnerability expected=1 current=1 missing=0 stale=0 split=0\nCOVERAGE runtime expected=1 current=0 failing=0 missing=0 partial=0 stale=0 pending=2\n",
		"workloads":       list(map[string]any{"kind": "Deployment", "metadata": map[string]any{"namespace": "demo", "name": "web"}}),
		"pods":            list(map[string]any{"metadata": map[string]any{"namespace": "demo"}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"imageID": "registry.test/web@" + digest}}}}),
		"posture":         list(map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{"controls": map[string]any{"C-0001": map[string]any{"severity": map[string]any{"severity": "High"}, "status": map[string]any{"status": "failed"}}}}}),
		"vulnerability":   list(v), "vulnerability_stored": list(v),
	}
}

func TestKubescapeShowsCoverageUnknownFreshnessAndFullImageOccurrences(t *testing.T) {
	p := kubescape()
	out, err := run(t, p)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{
		`platform_security_coverage{surface="runtime",state="current"} 0`,
		`platform_security_coverage{surface="runtime",state="pending"} 2`,
		`platform_security_posture_freshness_observable 0`,
		`platform_security_runtime_delivery_verified 0`,
		`platform_security_posture_finding{control="C-0001",severity="high",status="failed",namespace="demo",kind="Deployment",name="web"} 1`,
		`platform_security_vulnerabilities{severity="critical",context="full"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `context="relevant"`) {
		t.Fatalf("unproved relevancy exported: %s", out)
	}
}

func TestKubescapeOrphansAndDuplicateImagesDoNotInflateProgress(t *testing.T) {
	p := kubescape()
	p["posture"].(map[string]any)["items"] = append(p["posture"].(map[string]any)["items"].([]any), map[string]any{"metadata": map[string]any{"labels": map[string]any{"kubescape.io/workload-kind": "Deployment", "kubescape.io/workload-namespace": "gone", "kubescape.io/workload-name": "old"}}, "spec": map[string]any{"controls": map[string]any{"C-9999": map[string]any{}}}})
	v := p["vulnerability_stored"].(map[string]any)["items"].([]any)[0]
	p["vulnerability_stored"] = map[string]any{"items": []any{v, v}}
	out, err := run(t, p)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(out, "C-9999") || !strings.Contains(out, `context="full"} 3`) {
		t.Fatal(out)
	}
}

func TestKubescapeMalformedAndSplitDataCannotProduceCleanCounts(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["coverage_report"] = "COVERAGE posture expected=0 current=0\n" },
		func(p map[string]any) {
			p["vulnerability_stored"].(map[string]any)["items"].([]any)[0].(map[string]any)["spec"] = map[string]any{}
		},
		func(p map[string]any) {
			p["vulnerability_stored"].(map[string]any)["items"].([]any)[0].(map[string]any)["spec"] = map[string]any{"severities": map[string]any{"critical": map[string]any{"relevant": 1}}}
		},
		func(p map[string]any) { p["vulnerability"] = map[string]any{"items": []any{}} },
	} {
		p := kubescape()
		mutate(p)
		if out, err := run(t, p); err == nil {
			t.Fatalf("unknown data reported success: %s", out)
		}
	}
}

func TestAcceptedRuleCannotHideAFailedControl(t *testing.T) {
	p := kubescape()
	control := p["posture"].(map[string]any)["items"].([]any)[0].(map[string]any)["spec"].(map[string]any)["controls"].(map[string]any)["C-0001"].(map[string]any)
	control["rules"] = []any{map[string]any{"appliedIgnoreRules": []any{map[string]any{"name": "reviewed-exception"}}}}
	out, err := run(t, p)
	if err != nil || !strings.Contains(out, `platform_security_posture_total{status="failed"} 1`) {
		t.Fatalf("failed control hidden: %v %s", err, out)
	}
}

func TestCollectorReplacesStaleCountsWhenBothSourcesFail(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"kubectl": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_COMMANDS\"\nexit 1\n", "coverage": "#!/bin/sh\nexit 2\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics"), []byte("platform_security_policy_total{status=\"pass\"} 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../../k8s/bases/infrastructure/coroot/components/security-progress/collector.sh")
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", script, "--once")
	c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "METRICS_DIR="+dir, "COVERAGE_CHECK="+filepath.Join(dir, "coverage"), "TEST_COMMANDS="+filepath.Join(dir, "commands"))
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), " 99") || !strings.Contains(string(b), `platform_security_collection_success{surface="kubescape"} 0`) || !strings.Contains(string(b), `platform_security_collection_success{surface="policy"} 0`) {
		t.Fatalf("stale clean counts survived: %s", b)
	}
	commands, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "--cache-dir=/tmp/security-progress-kubectl") {
		t.Fatalf("read-only container has no writable discovery cache: %s", commands)
	}
}

func TestEveryPresentSeverityBucketMustHaveAFullCount(t *testing.T) {
	p := kubescape()
	p["vulnerability_stored"].(map[string]any)["items"].([]any)[0].(map[string]any)["spec"].(map[string]any)["severities"].(map[string]any)["high"] = map[string]any{"relevant": 2}
	if out, err := run(t, p); err == nil {
		t.Fatalf("missing full count appeared clean: %s", out)
	}
}

func TestConflictingLatestSummariesFailRegardlessOfListOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		p := kubescape()
		first := p["vulnerability_stored"].(map[string]any)["items"].([]any)[0]
		b, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		var second map[string]any
		if err := json.Unmarshal(b, &second); err != nil {
			t.Fatal(err)
		}
		second["metadata"].(map[string]any)["name"] = "second-image-summary"
		second["spec"].(map[string]any)["severities"].(map[string]any)["critical"].(map[string]any)["all"] = 0
		items := []any{first, second}
		if reverse {
			items = []any{second, first}
		}
		p["vulnerability"] = map[string]any{"items": items}
		p["vulnerability_stored"] = map[string]any{"items": items}
		if out, err := run(t, p); err == nil {
			t.Fatalf("conflicting latest counts succeeded reverse=%v: %s", reverse, out)
		}
	}
}

func TestEmptyPosturePreservesItsCoverageGap(t *testing.T) {
	p := kubescape()
	p["posture"].(map[string]any)["items"].([]any)[0].(map[string]any)["spec"].(map[string]any)["controls"] = nil
	p["coverage_report"] = strings.Replace(p["coverage_report"].(string), "posture expected=1 current=1 missing=0 empty=0", "posture expected=1 current=0 missing=0 empty=1", 1)
	out, err := run(t, p)
	if err != nil || !strings.Contains(out, `platform_security_coverage{surface="posture",state="empty"} 1`) || strings.Contains(out, "platform_security_posture_total{") {
		t.Fatalf("empty posture became hidden or a clean count: %v %s", err, out)
	}
}

func TestAllStaleVulnerabilitiesPreserveCoverageWithoutCleanCounts(t *testing.T) {
	p := kubescape()
	p["now"] = 1791000000
	p["coverage_report"] = strings.Replace(p["coverage_report"].(string), "vulnerability expected=1 current=1 missing=0 stale=0", "vulnerability expected=1 current=0 missing=0 stale=1", 1)
	out, err := run(t, p)
	if err != nil || !strings.Contains(out, `platform_security_coverage{surface="vulnerability",state="stale"} 1`) || strings.Contains(out, "platform_security_vulnerabilities{") {
		t.Fatalf("stale findings became hidden or zero: %v %s", err, out)
	}
}

func TestKyvernoTopLevelSeverityIsPreserved(t *testing.T) {
	p := policy()
	p["policy"].(map[string]any)["items"].([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)["severity"] = "high"
	out, err := run(t, p)
	if err != nil || !strings.Contains(out, `severity="high"`) {
		t.Fatalf("severity lost: %v %s", err, out)
	}
}
