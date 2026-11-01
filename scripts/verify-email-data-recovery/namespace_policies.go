package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"

	"gopkg.in/yaml.v3"
)

const namespacePolicySource = "k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml"

type namespacePolicy struct {
	api, rule string
	spec      object
}

// Only the reviewed namespace policy generators may leave untracked resources
// in the disposable namespace. Their specs remain owned by declarative source.
func readNamespacePolicies(input io.Reader) (map[string]namespacePolicy, error) {
	data, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("namespace-policy-source")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var source object
	if decoder.Decode(&source) != nil || text(source, "apiVersion") != "kyverno.io/v1" || text(source, "kind") != "ClusterPolicy" || text(child(source, "metadata"), "name") != "add-default-deny" {
		return nil, errors.New("namespace-policy-source")
	}
	var extra yaml.Node
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, errors.New("namespace-policy-source")
	}
	wanted := map[string]string{"CiliumNetworkPolicy/default-deny": "generate-default-deny", "CiliumNetworkPolicy/allow-dns": "generate-allow-dns", "CiliumNetworkPolicy/allow-cnpg-operator": "generate-allow-cnpg-operator", "NetworkPolicy/default-deny": "generate-default-deny-networkpolicy"}
	policies := map[string]namespacePolicy{}
	for _, rule := range items(child(source, "spec"), "rules") {
		generate := child(rule, "generate")
		key := text(generate, "kind") + "/" + text(generate, "name")
		ruleName, ok := wanted[key]
		api := "cilium.io/v2"
		if text(generate, "kind") == "NetworkPolicy" {
			api = "networking.k8s.io/v1"
		}
		_, duplicate := policies[key]
		if !ok || duplicate || text(rule, "name") != ruleName || text(generate, "apiVersion") != api || text(generate, "namespace") != "{{request.object.metadata.name}}" || generate["synchronize"] != true || generate["generateExisting"] != true || len(child(generate, "data")) != 1 {
			return nil, errors.New("namespace-policy-source")
		}
		encoded, err := json.Marshal(child(child(generate, "data"), "spec"))
		var spec object
		if err != nil || json.Unmarshal(encoded, &spec) != nil || len(spec) == 0 {
			return nil, errors.New("namespace-policy-source")
		}
		policies[key] = namespacePolicy{api: api, rule: ruleName, spec: spec}
	}
	if len(policies) != len(wanted) {
		return nil, errors.New("namespace-policy-source")
	}
	return policies, nil
}

func loadNamespacePolicies() (map[string]namespacePolicy, error) {
	source, err := os.Open(namespacePolicySource)
	if err != nil {
		return nil, errors.New("namespace-policy-source")
	}
	defer func() { _ = source.Close() }()
	return readNamespacePolicies(source)
}

func (p namespacePolicy) matches(o object, namespaceUID string) bool {
	m := child(o, "metadata")
	if namespaceUID == "" || text(m, "uid") == "" || text(o, "apiVersion") != p.api || m["deletionTimestamp"] != nil || len(items(m, "ownerReferences")) != 0 || !reflect.DeepEqual(child(o, "spec"), p.spec) {
		return false
	}
	for key := range o {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "spec" && key != "status" {
			return false
		}
	}
	labels := child(m, "labels")
	for key, expected := range map[string]string{"app.kubernetes.io/managed-by": "kyverno", "generate.kyverno.io/policy-name": "add-default-deny", "generate.kyverno.io/policy-namespace": "", "generate.kyverno.io/rule-name": p.rule, "generate.kyverno.io/trigger-group": "", "generate.kyverno.io/trigger-kind": "Namespace", "generate.kyverno.io/trigger-namespace": "", "generate.kyverno.io/trigger-uid": namespaceUID, "generate.kyverno.io/trigger-version": "v1"} {
		actual, ok := labels[key].(string)
		if !ok || actual != expected {
			return false
		}
	}
	return true
}
