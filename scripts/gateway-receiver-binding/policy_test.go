package main

import (
	"gopkg.in/yaml.v3"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparedReceiverRenderDoesNotActivateTheFence(t *testing.T) {
	command := exec.Command("kubectl", "kustomize", filepath.Join("..", "..", receiverSourceDir))
	raw, err := command.Output()
	if err != nil {
		t.Fatal("prepared policy render")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	seen := map[string]bool{}
	for {
		var obj map[string]any
		err := decoder.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kind, _ := obj["kind"].(string)
		if kind != "MutatingAdmissionPolicy" && kind != "ValidatingAdmissionPolicy" {
			t.Fatal("prepared revision activates or changes an unexpected resource")
		}
		seen[kind] = true
	}
	if len(seen) != 2 {
		t.Fatal("both unbound policy definitions are required")
	}
}
func TestProductionRequiresTheEncryptedBindingWithoutChangingTheLocalCluster(t *testing.T) {
	cmd := exec.Command("kubectl", "kustomize", filepath.Join("..", "..", "k8s", "clusters", "prod"))
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("production render")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	secret, required := false, false
	for {
		var o map[string]any
		err := decoder.Decode(&o)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		metadata, _ := o["metadata"].(map[string]any)
		if o["kind"] == "Secret" && metadata["name"] == "variables-gateway-receiver" {
			secret = true
			if _, ok := o["sops"]; !ok {
				t.Fatal("binding lost encryption metadata")
			}
		}
		if o["kind"] == "Kustomization" && metadata["name"] == "infrastructure" {
			spec := o["spec"].(map[string]any)
			post := spec["postBuild"].(map[string]any)
			for _, v := range post["substituteFrom"].([]any) {
				s := v.(map[string]any)
				if s["name"] == "variables-gateway-receiver" {
					required = s["kind"] == "Secret" && s["optional"] == false
				}
			}
		}
	}
	if !secret || !required {
		t.Fatal("production can apply a missing or guessed receiver source")
	}
	cmd = exec.Command("kubectl", "kustomize", filepath.Join("..", "..", "k8s", "clusters", "local"))
	raw, err = cmd.Output()
	if err != nil {
		t.Fatal("local render")
	}
	if strings.Contains(string(raw), "variables-gateway-receiver") {
		t.Fatal("production binding entered the local cluster")
	}
}
