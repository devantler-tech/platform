package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func array(v any) []any        { a, _ := v.([]any); return a }

func field(m map[string]any, keys ...string) any {
	var v any = m
	for _, key := range keys {
		v = obj(v)[key]
	}
	return v
}

func workflow(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "replace-openbao-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	return w
}

// The procedure must stay manual, protected and serialized with production
// deploys. Preflight changes nothing; production changes only in the
// replace-mode step, only through the replacement command, and only after the
// same step's capture was judged PROVEN for the named server (#4970).
func TestProcedureStaysManualProtectedAndGated(t *testing.T) {
	w := workflow(t)
	if len(obj(w["on"])) != 1 || obj(w["on"])["workflow_dispatch"] == nil || len(obj(w["permissions"])) != 0 ||
		field(w, "concurrency", "group") != "prod-deploy" || field(w, "concurrency", "cancel-in-progress") != false || field(w, "concurrency", "queue") != "max" {
		t.Fatal("manual or serialization boundary changed")
	}
	mode := obj(field(w, "on", "workflow_dispatch", "inputs", "mode"))
	if mode["default"] != "preflight" || !reflect.DeepEqual(mode["options"], []any{"preflight", "replace"}) {
		t.Fatal("the modes changed or replace became the default")
	}
	jobs := obj(w["jobs"])
	if len(jobs) != 1 {
		t.Fatal("unexpected execution lane")
	}
	j := obj(jobs["preflight"])
	if !reflect.DeepEqual(j["environment"], map[string]any{"name": "prod", "deployment": false}) || !reflect.DeepEqual(j["permissions"], map[string]any{"contents": "read", "packages": "read"}) {
		t.Fatal("protection or GitHub authority changed")
	}
	mutating := regexp.MustCompile(`\b(kubectl|bao|vault)\b[^\n]*\b(delete|patch|apply|scale|rollout|edit|replace|annotate|label|drain|cordon|write)\b`)
	source, tested, restored, judged, replaced, confirmed, proven := false, false, false, false, false, false, false
	for _, v := range array(j["steps"]) {
		s := obj(v)
		body := str(s["run"])
		if mutating.MatchString(body) {
			t.Fatalf("a step changes production outside the replacement command: %q", s["name"])
		}
		if strings.Contains(body, `[[ "$REPLACEMENT_CONFIRM" == "REPLACE $REPLACEMENT_SERVER" ]]`) {
			confirmed = true
		}
		if strings.Contains(body, "verify-prod-recovery-source.sh") {
			source = true
		}
		if strings.Contains(body, "go test -race -count=1 ./scripts/capture-openbao-replacement ./scripts/judge-openbao-health ./scripts/judge-openbao-quorum ./scripts/replace-openbao-pod") {
			if restored {
				t.Fatal("regressions after credentials")
			}
			tested = true
		}
		if field(s, "env", "KUBE_CONFIG") != nil {
			if !tested || !source || !confirmed {
				t.Fatal("credentials before exact-source, input checks and regressions")
			}
			restored = true
		}
		// Production must run this checkout's revision before anything is judged
		// or replaced (#6219): the applied digest's provenance is resolved
		// against the checkout, and only a CONVERGED verdict exports the digest.
		if strings.Contains(body, `replace-openbao-pod" applied`) {
			resolve := strings.Index(body, `resolve-prod-convergence" --digest "$applied" --main-ref "$GITHUB_SHA"`)
			export := strings.Index(body, `echo "OPENBAO_APPLIED_DIGEST=$applied" >> "$GITHUB_ENV"`)
			if !restored || judged || s["if"] != nil || resolve < 0 || export < resolve || !strings.Contains(body, `0) ;;`) {
				t.Fatal("the platform revision proof is skipped, misordered or not bound to this checkout")
			}
			proven = true
		}
		if strings.Contains(body, `"$bin/replace-openbao-pod"`) {
			if s["if"] != "inputs.mode == 'replace'" || !judged || !proven {
				t.Fatal("the replacement can run outside replace mode or before the preflight")
			}
			if !strings.Contains(body, `--expect-applied "$OPENBAO_APPLIED_DIGEST"`) {
				t.Fatal("the replacement is not bound to the proven platform revision")
			}
			gate := strings.Index(body, `judge_openbao_replacement "$private" "$ALREADY_REPLACED" || status=$?`)
			stop := strings.Index(body, `exit "$status"`)
			change := strings.Index(body, `replace-openbao-pod" delete`)
			if gate < 0 || stop < gate || change < stop || !strings.Contains(body, `--expect-uid "$judged_uid"`) {
				t.Fatal("the replacement is not bound to this step's PROVEN capture")
			}
			if !strings.Contains(body, `replace-openbao-pod" deliver`) {
				t.Fatal("secret delivery is not proven after the replacement")
			}
			replaced = true
		} else if strings.Contains(body, "judge_openbao_replacement") {
			if !restored || s["if"] != nil {
				t.Fatal("the preflight judgement is skipped or runs before credentials")
			}
			judged = true
		}
	}
	if !source || !tested || !restored || !judged || !replaced || !confirmed || !proven {
		t.Fatal("missing protected controls")
	}
	lib, err := os.ReadFile(filepath.Join("..", "judge-openbao-replacement.lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"capture-openbao-replacement\" --expect-servers", "judge-openbao-health\" --input", "judge-openbao-quorum\" --input", `--replace "$REPLACEMENT_SERVER"`} {
		if !strings.Contains(string(lib), want) {
			t.Fatalf("the shared judgement lost %s", want)
		}
	}
}

// The collector may read pods, mint the observer's token and port-forward;
// nothing else reaches kubectl.
func TestCollectorUsesOnlyReadingKubectlVerbs(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`k\.(?:output|args)\((?:ctx, )?"([a-z-]+)"`).FindAllStringSubmatch(string(b), -1)
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c[1]] = true
	}
	if !reflect.DeepEqual(seen, map[string]bool{"get": true, "create": true, "port-forward": true}) {
		t.Fatalf("kubectl verbs changed: %v", seen)
	}
	if !strings.Contains(string(b), `"create", "token", observerAccount`) {
		t.Fatal("create is used for something other than the observer token")
	}
}
