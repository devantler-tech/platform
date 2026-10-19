package main

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func workflow(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	return w
}

func TestWorkflowKeepsExpansionManualProtectedAndScoped(t *testing.T) {
	w := workflow(t, "expand-openbao-audit.yaml")
	if len(obj(w["on"])) != 1 || obj(w["on"])["workflow_dispatch"] == nil || len(obj(w["permissions"])) != 0 || field(w, "concurrency", "group") != "prod-deploy" || field(w, "concurrency", "cancel-in-progress") != false || field(w, "concurrency", "queue") != "max" {
		t.Fatal("manual or serialization boundary changed")
	}
	jobs := obj(w["jobs"])
	if len(jobs) != 1 {
		t.Fatal("unexpected execution lane")
	}
	j := obj(jobs["expand"])
	if !reflect.DeepEqual(j["environment"], map[string]any{"name": "prod", "deployment": false}) || !reflect.DeepEqual(j["permissions"], map[string]any{"contents": "read", "actions": "read", "deployments": "read"}) {
		t.Fatal("protection or GitHub authority changed")
	}
	mode := obj(field(w, "on", "workflow_dispatch", "inputs", "mode"))
	if mode["default"] != "preflight" || mode["type"] != "choice" || mode["required"] != true || !reflect.DeepEqual(mode["options"], []any{"preflight", "expand"}) {
		t.Fatal("read-only default or bounded mode choices changed")
	}
	tested, source, restored := false, false, false
	for _, v := range array(j["steps"]) {
		s := obj(v)
		body := str(s["run"])
		if strings.Contains(body, "verify-prod-recovery-source.sh") {
			source = true
		}
		if strings.Contains(body, "go test -race -count=1 ./scripts/expand-openbao-audit") {
			if field(s, "env", "AUDIT_REQUIRE_NATIVE_CONTROL") != "1" || restored {
				t.Fatal("native control optional or after credentials")
			}
			tested = true
		}
		if field(s, "env", "KUBE_CONFIG") != nil {
			if !tested || !source {
				t.Fatal("credentials before exact-source and tests")
			}
			restored = true
		}
		if strings.Contains(body, "go run ./scripts/expand-openbao-audit --execute") && (!tested || !source || !restored) {
			t.Fatal("unprotected execution")
		}
	}
	if !tested || !source || !restored {
		t.Fatal("missing protected controls")
	}
}

func TestCIRequiresRealTransportControl(t *testing.T) {
	w := workflow(t, "ci.yaml")
	for _, v := range array(field(w, "jobs", "validate-repository-guards", "steps")) {
		s := obj(v)
		if strings.Contains(str(s["run"]), "go test -race -count=1 ./scripts/expand-openbao-audit") && field(s, "env", "AUDIT_REQUIRE_NATIVE_CONTROL") == "1" {
			return
		}
	}
	t.Fatal("CI can skip the real kubectl patch control")
}

func TestActualDispatchModeMappingRejectsSwappedOrUnknownModes(t *testing.T) {
	w := workflow(t, "expand-openbao-audit.yaml")
	var body string
	for _, value := range array(field(w, "jobs", "expand", "steps")) {
		candidate := str(field(obj(value), "run"))
		if strings.Contains(candidate, "go run ./scripts/expand-openbao-audit --execute") {
			body = candidate
		}
	}
	if body == "" {
		t.Fatal("missing dispatch step")
	}
	validMapping := func(script string) bool {
		for _, tc := range []struct{ mode, want string }{
			{"preflight", "run ./scripts/expand-openbao-audit --preflight\n"},
			{"expand", "run ./scripts/expand-openbao-audit --execute\n"},
			{"unknown", ""},
		} {
			cmd := exec.Command("bash", "-c", "go() { printf '%s\\n' \"$*\"; }\n"+script)
			cmd.Env = []string{"AUDIT_CAPACITY_MODE=" + tc.mode}
			out, err := cmd.Output()
			if string(out) != tc.want || (err != nil) != (tc.mode == "unknown") {
				return false
			}
		}
		return true
	}
	if !validMapping(body) {
		t.Fatal("actual workflow does not bind modes to safe commands")
	}
	swapped := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(body, "--preflight", "--SWAP"), "--execute", "--preflight"), "--SWAP", "--execute")
	unknownExpands := strings.ReplaceAll(body, "*) exit 2", "*) go run ./scripts/expand-openbao-audit --execute")
	if validMapping(swapped) || validMapping(unknownExpands) {
		t.Fatal("adversarial workflow mapping accepted")
	}
}
