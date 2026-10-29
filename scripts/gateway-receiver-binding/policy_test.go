package main

import (
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReceiverPolicyPreservesOnlyTheSyntheticHTTPDiagnostic(t *testing.T) {
	path := filepath.Join("..", "..", "k8s", "providers", "hetzner", "infrastructure", "cilium", "gateway-receiver", "cilium-clusterwide-network-policy-gateway-receiver.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Spec struct {
			EndpointSelector struct {
				MatchExpressions []struct{ Key, Operator string } `yaml:"matchExpressions"`
			} `yaml:"endpointSelector"`
			Ingress []struct {
				FromCIDRSet  []struct{ CIDR string } `yaml:"fromCIDRSet"`
				FromEntities []string                `yaml:"fromEntities"`
				ToPorts      []struct {
					Ports []struct{ Port, Protocol string }
					Rules struct {
						HTTP []struct{ Host, Method, Path string }
					}
				} `yaml:"toPorts"`
			}
		}
	}
	if yaml.Unmarshal(raw, &policy) != nil {
		t.Fatal("invalid policy")
	}
	p := policy.Spec
	if len(p.EndpointSelector.MatchExpressions) != 1 || p.EndpointSelector.MatchExpressions[0].Key != "reserved:ingress" || p.EndpointSelector.MatchExpressions[0].Operator != "Exists" || len(p.Ingress) != 2 {
		t.Fatal("policy selection changed")
	}
	if len(p.Ingress[0].FromCIDRSet) != 1 || p.Ingress[0].FromCIDRSet[0].CIDR != "${gateway_receiver_source_cidr}" || len(p.Ingress[0].FromEntities) != 0 || len(p.Ingress[0].ToPorts) != 0 {
		t.Fatal("provider path has a fallback, broader identity or limited port")
	}
	diag := p.Ingress[1]
	if len(diag.FromEntities) != 1 || diag.FromEntities[0] != "cluster" || len(diag.ToPorts) != 1 || len(diag.ToPorts[0].Ports) != 1 || diag.ToPorts[0].Ports[0].Port != "80" || diag.ToPorts[0].Ports[0].Protocol != "TCP" || len(diag.ToPorts[0].Rules.HTTP) != 3 {
		t.Fatal("diagnostic exception is not HTTP-only and bounded")
	}
	for _, rule := range diag.ToPorts[0].Rules.HTTP {
		if rule.Method != "GET" || rule.Path != "^/$" {
			t.Fatal("diagnostic gained another operation")
		}
		r, err := regexp.Compile(rule.Host)
		if err != nil {
			t.Fatal(err)
		}
		matched := 0
		for _, host := range []string{"control.externalauth-probe.invalid", "authz.externalauth-probe.invalid", "unrouted.externalauth-probe.invalid"} {
			if r.MatchString(host) {
				matched++
			}
		}
		if matched != 1 || r.MatchString("mail.devantler.tech") || r.MatchString("control.externalauth-probe.invalid.evil") || r.MatchString("other.externalauth-probe.invalid") {
			t.Fatal("diagnostic rule can select an application or another host")
		}
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
