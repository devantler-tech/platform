package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"slices"
)

// Native Parse and RulesToPolicyEntries output for the scoped deny policy.
//
//go:embed testdata/storage-copy-deny-cilium-1.20.5.json
var storageCopyNativeDenyEntries []byte

type storageCopyIsolationScope struct {
	Namespace, Label, Prefix string
}

func (s storageCopyIsolationScope) denyPolicy(run, source, ownerAnnotation string) object {
	return object{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": object{
			"name": s.Prefix + run, "namespace": s.Namespace,
			"annotations": object{ownerAnnotation: run + ":" + source},
		},
		"spec": object{
			"endpointSelector": object{"matchLabels": object{s.Label: run}},
			"ingressDeny":      []any{object{"fromEntities": []any{"all"}}},
			"egressDeny":       []any{object{"toEntities": []any{"all"}}},
		},
	}
}

func (s storageCopyIsolationScope) verifyNativeDeny(raw []byte, policy object, run string) (uint64, error) {
	refused := errors.New("the copy Pod's ingress and egress denial is unverified")
	revision, repo, err := decodeRecoveryPolicyRepository(raw)
	if err != nil {
		return 0, err
	}
	var fixture []any
	if err := json.Unmarshal(storageCopyNativeDenyEntries, &fixture); err != nil {
		return 0, err
	}
	name, uid := text(policy, "metadata", "name"), text(policy, "metadata", "uid")
	if !isolationUID.MatchString(uid) || name != s.Prefix+run || text(policy, "metadata", "namespace") != s.Namespace {
		return 0, refused
	}
	expected := replaceIsolationFixture(fixture, map[string]string{
		"11111111-1111-4111-8111-111111111111": uid,
		"storage-copy-isolation-1234":          name,
		"storage-copy-fixture":                 s.Namespace,
		"1234":                                 run,
	}).([]any)
	for _, rule := range expected {
		labels := valueObject(rule.(object), "Subject", "matchLabels")
		if labels == nil {
			return 0, refused
		}
		const label = "any:storage.platform.devantler.tech/copy-run-fixture"
		if replacement := "any:" + s.Label; replacement != label {
			labels[replacement] = labels[label]
			delete(labels, label)
		}
	}
	var seen []int
	for _, entry := range repo {
		labels, _ := value(entry, "Labels").([]any)
		owned := slices.ContainsFunc(labels, func(raw any) bool {
			label, _ := raw.(object)
			return text(label, "source") == "k8s" &&
				(text(label, "key") == "io.cilium.k8s.policy.uid" && text(label, "value") == uid ||
					text(label, "key") == "io.cilium.k8s.policy.name" && text(label, "value") == name)
		})
		if !owned {
			continue
		}
		index := slices.IndexFunc(expected, func(e any) bool { return reflect.DeepEqual(entry, e) })
		if index < 0 || slices.Contains(seen, index) {
			return 0, refused
		}
		seen = append(seen, index)
	}
	if len(seen) != len(expected) {
		return 0, refused
	}
	return revision, nil
}

func storageCopyCiliumAgent(ctx context.Context, c client, nodeName string) (recoveryCiliumAgent, error) {
	node, err := c.get(ctx, "node", "", nodeName, false)
	if err != nil {
		return recoveryCiliumAgent{}, err
	}
	ds, err := c.get(ctx, "daemonset", "kube-system", "cilium", false)
	if err != nil {
		return recoveryCiliumAgent{}, err
	}
	pods, err := c.list(ctx, "pods", "kube-system")
	if err != nil {
		return recoveryCiliumAgent{}, err
	}
	var agent object
	for _, pod := range pods {
		if text(pod, "spec", "nodeName") == nodeName && text(pod, "metadata", "labels", "k8s-app") == "cilium" {
			if agent != nil {
				return recoveryCiliumAgent{}, errors.New("more than one Cilium agent is present on the copy node")
			}
			agent = pod
		}
	}
	return bindRecoveryCiliumAgent(agent, ds, node)
}

func (s storageCopyIsolationScope) observe(ctx context.Context, c client, pod, policy object, run, allowedNode string) (recoveryCiliumDatapath, error) {
	var proof recoveryCiliumDatapath
	refused := errors.New("the copy Pod's realized Cilium policy is unverified")
	node := text(pod, "spec", "nodeName")
	if node == "" || node != allowedNode || text(pod, "metadata", "namespace") != s.Namespace ||
		text(pod, "metadata", "labels", s.Label) != run {
		return proof, refused
	}
	agent, err := storageCopyCiliumAgent(ctx, c, node)
	if err != nil {
		return proof, err
	}
	native := func(args ...string) ([]byte, error) {
		prefix := []string{"exec", agent.Pod, "--namespace=kube-system", "--container=cilium-agent", "--pod-running-timeout=20s", "--", "cilium-dbg"}
		return c.kubectl(ctx, nil, append(prefix, args...)...)
	}
	config, err := native("config", "--all", "-o", "json")
	if err != nil {
		return proof, err
	}
	version, err := native("version", "-o", "json")
	if err != nil {
		return proof, err
	}
	runtime, err := readRecoveryCiliumConfiguration(config, version)
	if err != nil {
		return proof, err
	}
	repository, err := native("policy", "get", "-o", "json")
	if err != nil {
		return proof, err
	}
	revision, err := s.verifyNativeDeny(repository, policy, run)
	if err != nil {
		return proof, err
	}
	address, err := recoveryAddress(text(pod, "status", "podIP"))
	if err != nil {
		return proof, err
	}
	identifier := "ipv6:" + address
	if netip.MustParseAddr(address).Is4() {
		identifier = "ipv4:" + address
	}
	endpoint, err := native("endpoint", "get", identifier, "-o", "json")
	if err != nil {
		return proof, err
	}
	cep, err := c.get(ctx, "ciliumendpoints.cilium.io", s.Namespace, text(pod, "metadata", "name"), false)
	if err != nil {
		return proof, err
	}
	binding, err := bindCiliumEndpoint(endpoint, cep, pod, revision, s.Namespace, text(pod, "status", "phase"))
	if err != nil || !s.copyIdentity(endpoint, cep, run) {
		return proof, refused
	}
	sameAgent, err := storageCopyCiliumAgent(ctx, c, node)
	if err != nil || sameAgent != agent {
		return proof, refused
	}
	name := text(policy, "metadata", "name")
	seen, err := c.get(ctx, "ciliumnetworkpolicies.cilium.io", s.Namespace, name, false)
	generation, valid := nativeObservationUint(value(seen, "metadata", "generation"))
	if err != nil || !valid || generation == 0 || !recoveredNativeObject(seen, "cilium.io/v2", "CiliumNetworkPolicy", s.Namespace) ||
		benchmarkIdentity(seen) != benchmarkIdentity(policy) || value(seen, "metadata", "generation") != value(policy, "metadata", "generation") ||
		!reflect.DeepEqual(value(seen, "spec"), value(policy, "spec")) {
		return proof, refused
	}
	return recoveryCiliumDatapath{Agent: agent, Runtime: runtime, Endpoint: binding, PolicyUID: text(seen, "metadata", "uid"),
		PolicyGeneration: generation, PolicyRevision: revision}, nil
}

func (s storageCopyIsolationScope) copyIdentity(raw []byte, cep object, run string) bool {
	var endpoints []object
	if decodeIsolationJSON(raw, &endpoints) != nil || len(endpoints) != 1 {
		return false
	}
	prefix := "k8s:" + s.Label + "="
	for _, raw := range []any{value(endpoints[0], "status", "identity", "labels"), value(cep, "status", "identity", "labels")} {
		labels, _ := raw.([]any)
		if !slices.Contains(labels, any(prefix+run)) {
			return false
		}
	}
	return true
}

func sameStorageCopyIsolation(before, after recoveryCiliumDatapath) bool {
	return before.Agent == after.Agent && before.Runtime == after.Runtime && before.PolicyUID == after.PolicyUID &&
		before.PolicyGeneration == after.PolicyGeneration && after.PolicyRevision >= before.PolicyRevision &&
		after.Endpoint.PolicyRevision >= before.Endpoint.PolicyRevision && before.Endpoint.PodUID == after.Endpoint.PodUID &&
		before.Endpoint.CEPUID == after.Endpoint.CEPUID && before.Endpoint.Node == after.Endpoint.Node &&
		before.Endpoint.Addresses == after.Endpoint.Addresses && before.Endpoint.Attachment == after.Endpoint.Attachment &&
		before.Endpoint.EndpointID == after.Endpoint.EndpointID && before.Endpoint.SecurityID == after.Endpoint.SecurityID
}
