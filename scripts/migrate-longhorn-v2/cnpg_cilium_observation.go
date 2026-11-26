package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"time"
)

type recoveryCiliumObservation struct {
	Agent, DaemonSet, Node, Pod, Policy object
	ArchiveEndpoint                     string
}

type recoveryCiliumDatapath struct {
	Agent                            recoveryCiliumAgent
	Runtime                          recoveryCiliumConfig
	Endpoint                         recoveryCiliumEndpoint
	PolicyUID                        string
	PolicyGeneration, PolicyRevision uint64
}

// A bounded read-only collection, intended only for the reviewed-main recovery
// workflow. The caller must first prove the owned CNPG Pod/Job graph and bind
// the DaemonSet to reviewed deployment facts. Run again around fixed live
// network probes and compare the complete result. This is not network proof,
// namespace setup, capacity reservation, or permission for production cutover.
func (c client) observeRecoveryCilium(ctx context.Context, input recoveryCiliumObservation) (result recoveryCiliumDatapath, err error) {
	stage := "input-binding"
	defer func() {
		if err != nil {
			result = recoveryCiliumDatapath{}
			err = fmt.Errorf("native recovery datapath observation failed: phase=%s", stage)
		}
	}()
	refused := errors.New("native recovery datapath observation is incomplete or changed during collection")
	baseline, err := bindRecoveryCiliumAgent(input.Agent, input.DaemonSet, input.Node)
	ns, name := text(input.Pod, "metadata", "namespace"), text(input.Pod, "metadata", "name")
	generation, genOK := nativeObservationUint(value(input.Policy, "metadata", "generation"))
	if err != nil || !isolationNamespace.MatchString(ns) || name == "" || !isolationUID.MatchString(text(input.Pod, "metadata", "uid")) || text(input.Pod, "spec", "nodeName") != baseline.Node || !isolationUID.MatchString(text(input.Policy, "metadata", "uid")) || text(input.Policy, "metadata", "namespace") != ns || !genOK || generation == 0 || generation > 2147483647 || !recoveryEndpoint.MatchString(input.ArchiveEndpoint) {
		return result, refused
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	readBindings := func() (object, object, error) {
		agent, e := c.get(ctx, "pod", "kube-system", baseline.Pod, false)
		if e != nil {
			return nil, nil, refused
		}
		ds, e := c.get(ctx, "daemonset", "kube-system", "cilium", false)
		if e != nil {
			return nil, nil, refused
		}
		node, e := c.get(ctx, "node", "", baseline.Node, false)
		if e != nil {
			return nil, nil, refused
		}
		current, e := bindRecoveryCiliumAgent(agent, ds, node)
		if e != nil || current != baseline {
			return nil, nil, refused
		}
		pod, e := c.get(ctx, "pod", ns, name, false)
		if e != nil || text(pod, "metadata", "uid") != text(input.Pod, "metadata", "uid") || text(pod, "metadata", "namespace") != ns || text(pod, "metadata", "name") != name || text(pod, "spec", "nodeName") != baseline.Node || !reflect.DeepEqual(value(pod, "spec"), value(input.Pod, "spec")) || !reflect.DeepEqual(value(pod, "metadata", "ownerReferences"), value(input.Pod, "metadata", "ownerReferences")) {
			return nil, nil, refused
		}
		policy, e := c.get(ctx, "ciliumnetworkpolicies.cilium.io", ns, text(input.Policy, "metadata", "name"), false)
		if e != nil || value(policy, "metadata", "deletionTimestamp") != nil || text(policy, "metadata", "namespace") != ns || text(policy, "metadata", "name") != text(input.Policy, "metadata", "name") || text(policy, "metadata", "uid") != text(input.Policy, "metadata", "uid") || value(policy, "metadata", "generation") != value(input.Policy, "metadata", "generation") || !reflect.DeepEqual(value(policy, "spec"), value(input.Policy, "spec")) {
			return nil, nil, refused
		}
		return pod, policy, nil
	}
	native := func(args ...string) ([]byte, error) {
		argv := []string{"exec", baseline.Pod, "--namespace=kube-system", "--container=cilium-agent", "--pod-running-timeout=20s", "--", "cilium-dbg"}
		b, e := c.kubectl(ctx, nil, append(argv, args...)...)
		if e != nil {
			return nil, refused
		}
		return b, nil
	}
	stage = "binding-before"
	before, policy, err := readBindings()
	if err != nil {
		return result, refused
	}
	address, err := recoveryAddress(text(before, "status", "podIP"))
	if err != nil {
		return result, refused
	}
	identifier := "ipv6:" + address
	if netip.MustParseAddr(address).Is4() {
		identifier = "ipv4:" + address
	}
	var runtime recoveryCiliumConfig
	var firstEndpoint recoveryCiliumEndpoint
	var firstRevision uint64
	for phase := range 2 {
		suffix := "before"
		if phase == 1 {
			suffix = "after"
		}
		stage = "runtime-config-" + suffix
		config, e := native("config", "--all", "-o", "json")
		if e != nil {
			return result, refused
		}
		stage = "runtime-version-" + suffix
		version, e := native("version", "-o", "json")
		if e != nil {
			return result, refused
		}
		stage = "policy-repository-" + suffix
		raw, e := native("policy", "get", "-o", "json")
		if e != nil {
			return result, refused
		}
		stage = "runtime-policy-binding-" + suffix
		cfg, revision, e := readRecoveryCiliumRuntime(config, version, raw, policy, input.ArchiveEndpoint)
		if e != nil || phase == 1 && (cfg != runtime || revision != firstRevision) {
			return result, refused
		}
		runtime = cfg
		stage = "endpoint-read-" + suffix
		raw, e = native("endpoint", "get", identifier, "-o", "json")
		if e != nil {
			return result, refused
		}
		cep, e := c.get(ctx, "ciliumendpoints.cilium.io", ns, name, false)
		if e != nil {
			return result, refused
		}
		pod := before
		if phase == 1 {
			stage = "binding-after"
			pod, _, e = readBindings()
			if e != nil {
				return result, refused
			}
		}
		stage = "endpoint-binding-" + suffix
		endpoint, e := bindRecoveryCiliumEndpoint(raw, cep, pod, revision)
		if e != nil || phase == 1 && endpoint != firstEndpoint {
			return result, refused
		}
		firstEndpoint, firstRevision = endpoint, revision
	}
	result = recoveryCiliumDatapath{Agent: baseline, Runtime: runtime, Endpoint: firstEndpoint, PolicyUID: text(policy, "metadata", "uid"), PolicyGeneration: generation, PolicyRevision: firstRevision}
	return result, nil
}
