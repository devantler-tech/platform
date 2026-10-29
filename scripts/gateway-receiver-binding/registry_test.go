package main

import (
	"context"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderedPoliciesMatchTheExactDeployedReadback(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "192.0.2.5/32"}
	raw, err := exec.Command("kubectl", "kustomize", receiverSourceDir).Output()
	if err != nil {
		t.Fatal("native policy render")
	}
	decoder := yaml.NewDecoder(strings.NewReader(strings.ReplaceAll(string(raw), "${gateway_receiver_source_cidr}", b.CIDR)))
	seen := 0
	for {
		var rendered map[string]any
		if err := decoder.Decode(&rendered); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal("rendered policy input")
		}
		for _, r := range preparedPolicies {
			if rendered["kind"] != r.Kind {
				continue
			}
			var actual map[string]any
			if json.Unmarshal(registryFixture(t, r, b), &actual) != nil {
				t.Fatal("policy metadata fixture")
			}
			actual["spec"] = rendered["spec"]
			readback, _ := json.Marshal(actual)
			if !admissionMatches(readback, r, b) {
				t.Fatal("render changed a reviewed policy's exact deployed surface")
			}
			seen++
		}
	}
	if seen != len(preparedPolicies) {
		t.Fatal("a prepared policy was not exercised")
	}
}

func registryFixture(t *testing.T, r admissionResource, b binding) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(receiverSourceDir, r.File))
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if yaml.Unmarshal([]byte(strings.ReplaceAll(string(raw), "${gateway_receiver_source_cidr}", b.CIDR)), &o) != nil {
		t.Fatal("source")
	}
	m := o["metadata"].(map[string]any)
	m["uid"] = "44444444-4444-4444-8444-444444444444"
	m["resourceVersion"] = "10"
	m["generation"] = 1
	if r.Kind == "ValidatingAdmissionPolicy" {
		o["status"] = map[string]any{"observedGeneration": 1, "typeChecking": map[string]any{"expressionWarnings": []any{}}}
	}
	out, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestDisabledBindingCleanupChecksTheWholeSetBeforeDeleting(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "192.0.2.5/32"}
	state := map[string][]byte{}
	for _, r := range preparedBindings {
		state[r.Resource] = registryFixture(t, r, b)
	}
	var foreign map[string]any
	_ = json.Unmarshal(state[preparedBindings[1].Resource], &foreign)
	foreign["metadata"].(map[string]any)["annotations"].(map[string]any)[receiverOwner] = "foreign"
	state[preparedBindings[1].Resource], _ = json.Marshal(foreign)
	writes := 0
	c := receiverClient{context: "admin@prod", command: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "get" {
				return state[args[i+1]], nil
			}
			if arg == "delete" {
				writes++
				delete(state, preparedBindings[0].Resource)
				return nil, nil
			}
		}
		return nil, errors.New("unexpected")
	}}
	if c.removeBindings(context.Background(), b) == nil || writes != 0 {
		t.Fatal("a foreign set caused partial removal")
	}
}
func TestDisabledBindingCleanupUsesDeletePreconditionsAndResolvesLostAcknowledgement(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "192.0.2.5/32"}
	state := map[string][]byte{}
	for _, r := range preparedBindings {
		state[r.Resource] = registryFixture(t, r, b)
	}
	deleted := []string{}
	c := receiverClient{context: "admin@prod", command: func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "get" {
				return state[args[i+1]], nil
			}
			if arg == "delete" {
				uri := args[i+2]
				kind := ""
				for _, r := range preparedBindings {
					if uri == "/apis/admissionregistration.k8s.io/v1/"+r.Resource+"/gateway-receiver" {
						kind = r.Resource
					}
				}
				var options struct {
					Preconditions struct{ UID, ResourceVersion string }
				}
				if kind == "" || json.Unmarshal(input, &options) != nil || options.Preconditions.UID != "44444444-4444-4444-8444-444444444444" || options.Preconditions.ResourceVersion != "10" {
					t.Fatal("delete not pinned to observed object and version")
				}
				deleted = append(deleted, kind)
				delete(state, kind)
				return nil, errors.New("sensitive fake transport error")
			}
		}
		return nil, errors.New("unexpected")
	}}
	if err := c.removeBindings(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || deleted[0] != preparedBindings[0].Resource || len(state) != 0 {
		t.Fatal("wrong cleanup order or acknowledgement substituted for readback")
	}
}
func TestPreparedPolicyRejectsUnobservedTypeCheckingAndScopeDrift(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "192.0.2.5/32"}
	r := preparedPolicies[1]
	raw := registryFixture(t, r, b)
	if !admissionMatches(raw, r, b) {
		t.Fatal("reviewed native policy refused")
	}
	for _, bad := range []string{"unobserved", "warning", "scope"} {
		t.Run(bad, func(t *testing.T) {
			var o map[string]any
			_ = json.Unmarshal(raw, &o)
			switch bad {
			case "unobserved":
				o["status"].(map[string]any)["observedGeneration"] = 0
			case "warning":
				o["status"].(map[string]any)["typeChecking"].(map[string]any)["expressionWarnings"] = []any{map[string]any{"warning": "unknown expression"}}
			case "scope":
				o["spec"].(map[string]any)["matchConditions"] = []any{}
			}
			changed, _ := json.Marshal(o)
			if admissionMatches(changed, r, b) {
				t.Fatal("unknown or changed native admission policy accepted")
			}
		})
	}
}

func TestDisabledReceiverRefusesAnActiveProxyBeforeAnyBindingWrite(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	m, b := disabledFixture()
	listener := m["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
	listener["listenerFilters"] = []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol"}}
	config, _ := json.Marshal(m)
	state := map[string][]byte{"ciliumenvoyconfig": config}
	for _, r := range append(append([]admissionResource{}, preparedPolicies...), preparedBindings...) {
		state[r.Resource] = registryFixture(t, r, b)
	}
	writes := 0
	c := receiverClient{context: "admin@prod", command: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "get" {
				return state[args[i+1]], nil
			}
			if arg == "delete" {
				writes++
				for _, r := range preparedBindings {
					if strings.Contains(args[i+2], "/"+r.Resource+"/") {
						delete(state, r.Resource)
					}
				}
				return nil, nil
			}
			if arg == "patch" {
				writes++
				return nil, errors.New("unexpected patch")
			}
		}
		return nil, errors.New("unexpected")
	}}
	if c.restoreReceiver(context.Background(), b) == nil || writes != 0 {
		t.Fatal("active PROXY listener allowed removal of a receiver fence")
	}
}
