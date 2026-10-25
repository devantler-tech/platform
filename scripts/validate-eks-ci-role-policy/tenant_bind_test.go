package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type tenantBindRole struct {
	Rules []struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		Verbs         []string `yaml:"verbs"`
		ResourceNames []string `yaml:"resourceNames"`
	} `yaml:"rules"`
}

func tenantPullSecretCIContract(wf workflow) error {
	j, ok := wf.Jobs["validate-cluster-policies"]
	needs, valid := j.needsOf()
	if !ok || !valid || !slices.Equal(needs, []string{"changes"}) || errorIsTolerated(j.ContinueOnError) ||
		strings.Join(strings.Fields(j.If), " ") != "github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'" {
		return fmt.Errorf("lost selected policy job enforcement")
	}
	count := 0
	for _, s := range j.Steps {
		shell := s.Shell
		if shell == "" {
			shell = j.Defaults.Run.Shell
		}
		if shell == "" {
			shell = wf.Defaults.Run.Shell
		}
		if s.Uses == "" && s.enforces() && shellKeepsErrexit(shell) {
			count += canonicalCommandCount(s.Run, "bash scripts/tests/test-tenant-kro-pull-secret.sh")
		}
	}
	if count != 1 {
		return fmt.Errorf("lost native pull-secret admission execution")
	}
	return nil
}

func TestTenantPullSecretAdmissionRemainsInCI(t *testing.T) {
	wf := canonicalCIWorkflow(t)
	if err := tenantPullSecretCIContract(wf); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing", "skip", "tolerate", "comment", "mask", "unsafe-shell", "job-skip", "job-tolerate"} {
		t.Run(mutation, func(t *testing.T) {
			w := canonicalCIWorkflow(t)
			j := w.Jobs["validate-cluster-policies"]
			found := 0
			for i := range j.Steps {
				s := &j.Steps[i]
				if !strings.Contains(s.Run, "bash scripts/tests/test-tenant-kro-pull-secret.sh") {
					continue
				}
				found++
				switch mutation {
				case "missing":
					s.Run = ""
				case "skip":
					s.If = "false"
				case "tolerate":
					s.ContinueOnError = true
				case "comment":
					s.Run = "# bash scripts/tests/test-tenant-kro-pull-secret.sh"
				case "mask":
					s.Run += " || true"
				case "unsafe-shell":
					s.Shell = "bash {0}"
				case "job-skip":
					j.If = "false"
				case "job-tolerate":
					j.ContinueOnError = true
				}
			}
			if found != 1 {
				t.Fatal("native admission mutation did not select one actual step")
			}
			w.Jobs["validate-cluster-policies"] = j
			if tenantPullSecretCIContract(w) == nil {
				t.Fatal("admitted lost or neutralized native admission controls")
			}
		})
	}
}

func tenantBindContract(graphBody, roleBody []byte) error {
	var graph struct {
		Spec struct {
			Resources []struct {
				Template struct {
					Kind    string
					RoleRef struct{ Kind, Name string } `yaml:"roleRef"`
				} `yaml:"template"`
			} `yaml:"resources"`
		} `yaml:"spec"`
	}
	var role tenantBindRole
	if err := yaml.Unmarshal(graphBody, &graph); err != nil {
		return err
	}
	if err := yaml.Unmarshal(roleBody, &role); err != nil {
		return err
	}
	want := []string{"tenant-edit", "tenant-external-dns", "tenant-external-dns-global", "tenant-webapp-edit"}
	referenced := map[string]bool{}
	for _, resource := range graph.Spec.Resources {
		template := resource.Template
		if template.Kind != "RoleBinding" && template.Kind != "ClusterRoleBinding" {
			continue
		}
		if template.RoleRef.Kind != "ClusterRole" || !slices.Contains(want, template.RoleRef.Name) {
			return fmt.Errorf("unreviewed Tenant role reference")
		}
		referenced[template.RoleRef.Name] = true
	}
	granted := map[string]bool{}
	for _, rule := range role.Rules {
		if !slices.Contains(rule.Verbs, "bind") && !slices.Contains(rule.Verbs, "*") {
			continue
		}
		if !slices.Equal(rule.Verbs, []string{"bind"}) || !slices.Equal(rule.APIGroups, []string{"rbac.authorization.k8s.io"}) || !slices.Equal(rule.Resources, []string{"clusterroles"}) || len(rule.ResourceNames) == 0 {
			return fmt.Errorf("unbounded Tenant bind grant")
		}
		for _, name := range rule.ResourceNames {
			if !slices.Contains(want, name) || granted[name] {
				return fmt.Errorf("extra Tenant bind grant: %s", name)
			}
			granted[name] = true
		}
	}
	for _, name := range want {
		if !referenced[name] || !granted[name] {
			return fmt.Errorf("Tenant constructor and scoped bind grant differ for %s", name)
		}
	}
	return nil
}

func TestTenantBindGrantMatchesActualConstructor(t *testing.T) {
	graph, err := os.ReadFile("../../k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role, err := os.ReadFile("../../k8s/bases/infrastructure/resource-graph-definitions/tenant/cluster-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := tenantBindContract(graph, role); err != nil {
		t.Fatal(err)
	}
	var parsed tenantBindRole
	if err := yaml.Unmarshal(role, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing", "wildcard", "edit", "admin", "wrong-group", "wrong-kind", "all-verbs"} {
		t.Run(mutation, func(t *testing.T) {
			var changed tenantBindRole
			if err := yaml.Unmarshal(role, &changed); err != nil {
				t.Fatal(err)
			}
			found := 0
			for i := range changed.Rules {
				r := &changed.Rules[i]
				if !slices.Contains(r.Verbs, "bind") {
					continue
				}
				found++
				switch mutation {
				case "missing":
					r.ResourceNames = slices.DeleteFunc(r.ResourceNames, func(v string) bool { return v == "tenant-edit" })
				case "wildcard":
					r.ResourceNames = []string{"*"}
				case "edit", "admin":
					r.ResourceNames = append(r.ResourceNames, mutation)
				case "wrong-group":
					r.APIGroups = []string{""}
				case "wrong-kind":
					r.Resources = []string{"roles"}
				case "all-verbs":
					r.Verbs = []string{"*"}
				}
			}
			if found != 1 {
				t.Fatal("no unique real bind rule mutated")
			}
			body, err := yaml.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := tenantBindContract(graph, body); err == nil {
				t.Fatal("accepted missing or excessive Tenant binding authority")
			}
		})
	}
}
