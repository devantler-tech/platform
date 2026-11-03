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

// The preflight must stay manual, protected, serialized with production
// deploys and unable to change anything, until a reviewed change adds the
// replacement step (#4970).
func TestPreflightStaysManualProtectedAndReadOnly(t *testing.T) {
	w := workflow(t)
	if len(obj(w["on"])) != 1 || obj(w["on"])["workflow_dispatch"] == nil || len(obj(w["permissions"])) != 0 ||
		field(w, "concurrency", "group") != "prod-deploy" || field(w, "concurrency", "cancel-in-progress") != false || field(w, "concurrency", "queue") != "max" {
		t.Fatal("manual or serialization boundary changed")
	}
	mode := obj(field(w, "on", "workflow_dispatch", "inputs", "mode"))
	if mode["default"] != "preflight" || !reflect.DeepEqual(mode["options"], []any{"preflight"}) {
		t.Fatal("a mode other than the read-only preflight appeared")
	}
	jobs := obj(w["jobs"])
	if len(jobs) != 1 {
		t.Fatal("unexpected execution lane")
	}
	j := obj(jobs["preflight"])
	if !reflect.DeepEqual(j["environment"], map[string]any{"name": "prod", "deployment": false}) || !reflect.DeepEqual(j["permissions"], map[string]any{"contents": "read"}) {
		t.Fatal("protection or GitHub authority changed")
	}
	mutating := regexp.MustCompile(`\b(kubectl|bao|vault)\b[^\n]*\b(delete|patch|apply|scale|rollout|edit|replace|annotate|label|drain|cordon|write)\b`)
	source, tested, restored, captured := false, false, false, false
	for _, v := range array(j["steps"]) {
		s := obj(v)
		body := str(s["run"])
		if mutating.MatchString(body) {
			t.Fatalf("a step can change production: %q", s["name"])
		}
		if strings.Contains(body, "verify-prod-recovery-source.sh") {
			source = true
		}
		if strings.Contains(body, "go test -race -count=1 ./scripts/capture-openbao-replacement ./scripts/judge-openbao-health ./scripts/judge-openbao-quorum") {
			if restored {
				t.Fatal("regressions after credentials")
			}
			tested = true
		}
		if field(s, "env", "KUBE_CONFIG") != nil {
			if !tested || !source {
				t.Fatal("credentials before exact-source and regressions")
			}
			restored = true
		}
		if strings.Contains(body, "capture-openbao-replacement\" --expect-servers") {
			if !restored || !strings.Contains(body, "judge-openbao-health") || !strings.Contains(body, "judge-openbao-quorum") || !strings.Contains(body, "--replace \"$REPLACEMENT_SERVER\"") {
				t.Fatal("capture is not judged by both judges for the named server")
			}
			captured = true
		}
	}
	if !source || !tested || !restored || !captured {
		t.Fatal("missing protected controls")
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
