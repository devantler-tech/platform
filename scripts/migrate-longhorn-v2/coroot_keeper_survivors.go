package main

import (
	"context"
	"strconv"
)

func probeCorootKeeperMember(ctx context.Context, c client, member corootKeeperMember, stateful object, ordinal, followers int) (bool, error) {
	out, err := c.kubectl(ctx, nil, "exec", member.Pod, "--namespace="+corootKeeperNamespace,
		"--container="+corootKeeperContainer, "--pod-running-timeout=10s", "--", "bash", "-ec", corootKeeperProbe)
	if err != nil {
		return false, err
	}
	latest, err := c.get(ctx, "pod", corootKeeperNamespace, member.Pod, false)
	if err != nil {
		return false, err
	}
	checked, err := bindCorootKeeperMember(latest, stateful, ordinal)
	member.Leader = false
	if err != nil || checked != member {
		return false, errCorootKeeperQuorum
	}
	return observedKeeperLeader(out, followers)
}

func verifyCorootKeeperSurvivors(ctx context.Context, c client, input corootKeeperQuorumReceipt, target int) error {
	if target < 0 || target >= len(input.Members) || input.Members[target].Leader {
		return errCorootKeeperQuorum
	}
	stateful, err := c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil {
		return err
	}
	if benchmarkIdentity(stateful) != input.Controller ||
		text(stateful, "status", "currentRevision") == "" ||
		text(stateful, "status", "currentRevision") != text(stateful, "status", "updateRevision") ||
		value(stateful, "spec", "replicas") != float64(3) || value(stateful, "status", "readyReplicas") != float64(2) ||
		value(stateful, "metadata", "generation") != value(stateful, "status", "observedGeneration") {
		return errCorootKeeperQuorum
	}
	budget, err := c.get(ctx, "poddisruptionbudget", corootKeeperNamespace, corootKeeperName, false)
	if err != nil {
		return err
	}
	if benchmarkIdentity(budget) != input.Budget ||
		value(budget, "metadata", "generation") != value(budget, "status", "observedGeneration") ||
		value(budget, "spec", "maxUnavailable") != float64(1) ||
		value(budget, "status", "currentHealthy") != float64(2) ||
		value(budget, "status", "expectedPods") != float64(3) ||
		value(budget, "status", "desiredHealthy") != float64(2) ||
		value(budget, "status", "disruptionsAllowed") != float64(0) {
		return errCorootKeeperQuorum
	}
	pods, err := c.list(ctx, "pods", corootKeeperNamespace)
	if err != nil {
		return err
	}
	members := map[string]object{}
	for _, pod := range pods {
		if corootKeeperSelected(pod) {
			name := text(pod, "metadata", "name")
			if members[name] != nil {
				return errCorootKeeperQuorum
			}
			members[name] = pod
		}
	}
	if len(members) != 2 || members[corootKeeperName+"-"+strconv.Itoa(target)] != nil {
		return errCorootKeeperQuorum
	}
	for ordinal, expected := range input.Members {
		if ordinal == target {
			continue
		}
		member, err := bindCorootKeeperMember(members[expected.Pod], stateful, ordinal)
		if err != nil || member.Pod != expected.Pod || member.UID != expected.UID ||
			member.ContainerID != expected.ContainerID || member.Node != expected.Node {
			return errCorootKeeperQuorum
		}
		leader, err := probeCorootKeeperMember(ctx, c, member, stateful, ordinal, 1)
		if err != nil || leader != expected.Leader {
			return errCorootKeeperQuorum
		}
	}
	return nil
}
