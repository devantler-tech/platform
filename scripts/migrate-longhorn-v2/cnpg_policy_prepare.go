package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

type recoveryPolicyPreparation struct {
	Agent                recoveryCiliumAgent
	Runtime              recoveryCiliumConfig
	PolicyUID            string
	Generation, Revision uint64
}

// observeRecoveryPolicyPreparation admits no data Pod before native policy import and safe initialization.
func (c client) observeRecoveryPolicyPreparation(ctx context.Context, input recoveryCiliumObservation) (result recoveryPolicyPreparation, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = recoveryPolicyPreparation{}
			err = fmt.Errorf("recovery policy preparation held: phase=%s", phase)
		}
	}()
	refused := errors.New("pre-creation policy or its native process changed")
	if c.context != "admin@prod" || !commandContextActive(ctx) {
		return result, refused
	}
	baseline, err := bindRecoveryCiliumAgent(input.Agent, input.DaemonSet, input.Node)
	generation, ok := nativeObservationUint(value(input.Policy, "metadata", "generation"))
	ns, name := text(input.Policy, "metadata", "namespace"), text(input.Policy, "metadata", "name")
	if err != nil || !ok || generation == 0 || generation > 2147483647 || !isolationNamespace.MatchString(ns) || !isolationUID.MatchString(text(input.Policy, "metadata", "uid")) || !recoveryEndpoint.MatchString(input.ArchiveEndpoint) {
		return result, refused
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	readBindings := func() error {
		agent, e := c.get(ctx, "pod", "kube-system", baseline.Pod, false)
		if e != nil {
			return e
		}
		ds, e := c.get(ctx, "daemonset", "kube-system", "cilium", false)
		if e != nil {
			return e
		}
		node, e := c.get(ctx, "node", "", baseline.Node, false)
		if e != nil {
			return e
		}
		current, e := bindRecoveryCiliumAgent(agent, ds, node)
		if e != nil || current != baseline {
			return refused
		}
		policy, e := c.get(ctx, "ciliumnetworkpolicies.cilium.io", ns, name, false)
		if e != nil {
			return e
		}
		if text(policy, "metadata", "namespace") != ns || text(policy, "metadata", "name") != name || text(policy, "metadata", "uid") != text(input.Policy, "metadata", "uid") || value(policy, "metadata", "generation") != value(input.Policy, "metadata", "generation") || value(policy, "metadata", "deletionTimestamp") != nil || !reflect.DeepEqual(value(policy, "spec"), value(input.Policy, "spec")) {
			return refused
		}
		return nil
	}
	phase = "bindings-before"
	if err = readBindings(); err != nil {
		return result, err
	}
	native := func(args ...string) ([]byte, error) {
		prefix := []string{"exec", baseline.Pod, "--namespace=kube-system", "--container=cilium-agent", "--pod-running-timeout=20s", "--", "cilium-dbg"}
		return c.kubectl(ctx, nil, append(prefix, args...)...)
	}
	var first recoveryPolicyPreparation
	for pass := range 2 {
		phase = "runtime"
		configuration, e := native("config", "--all", "-o", "json")
		if e != nil {
			return result, e
		}
		version, e := native("version", "-o", "json")
		if e != nil {
			return result, e
		}
		hive, e := native("shell", "hive")
		if e != nil {
			return result, e
		}
		runtime, e := readRecoveryCiliumRuntime(configuration, version, hive)
		if e != nil {
			return result, e
		}
		phase = "policy-import"
		raw, e := native("policy", "get", "-o", "json")
		if e != nil {
			return result, e
		}
		revision, e := verifyCNPGIsolationImport(raw, input.Policy, input.ArchiveEndpoint, runtime)
		if e != nil {
			return result, e
		}
		phase = "initialization"
		initRevision, e := verifyRecoveryInitPolicy(raw)
		if e != nil || initRevision != revision {
			return result, refused
		}
		bound := recoveryPolicyPreparation{Agent: baseline, Runtime: runtime, PolicyUID: text(input.Policy, "metadata", "uid"), Generation: generation, Revision: revision}
		if pass == 1 && bound != first {
			return result, refused
		}
		first = bound
	}
	phase = "bindings-after"
	if err = readBindings(); err != nil {
		return result, err
	}
	if !commandContextActive(ctx) {
		return result, refused
	}
	return first, nil
}
