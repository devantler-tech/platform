package main

import (
	"context"
	"errors"
	"strconv"
)

const cnpgRecoveryMemory = 1 << 30
const cnpgRecoveryMilliCPU = 500

var errCNPGRecoveryHost = errors.New("no registered, scale-down-protected worker has sufficient database recovery capacity")

func cnpgRecoveryHostname(plan *cnpgRecoveryPlan) string {
	if plan == nil || len(plan.Resources) != 4 {
		return ""
	}
	return text(plan.Resources[3], "spec", "affinity", "nodeSelector", "kubernetes.io/hostname")
}

func selectCNPGRecoveryHost(live inventory, bindings map[string]temporaryV2Binding) (string, error) {
	nodes, err := indexed(live.nodes)
	if err != nil || len(bindings) != len(temporaryV2Selections) {
		return "", errCNPGRecoveryHost
	}
	host, best := "", float64(0)
	for _, selection := range temporaryV2Selections {
		binding := bindings[selection]
		node := nodes[binding.Name]
		room, fits := cnpgRecoveryHostRoom(node, live.pods, binding)
		if fits && room > best {
			host, best = binding.Name, room
		}
	}
	if host == "" {
		return "", errCNPGRecoveryHost
	}
	return host, nil
}

// Reserve room for the 256 MiB recovery container, the bootstrap and injected
// containers, while accounting for existing requests and allocated resources.
func cnpgRecoveryHostRoom(node object, pods []object, binding temporaryV2Binding) (float64, bool) {
	spec, _ := value(node, "spec").(object)
	taints, tainted := value(spec, "taints").([]any)
	if binding.Name == "" || binding.NodeUID == "" || binding.ProviderID == "" || binding.MachineUUID == "" || binding.BootID == "" || binding.OSImage == "" ||
		text(node, "metadata", "name") != binding.Name || text(node, "metadata", "labels", "kubernetes.io/hostname") != binding.Name ||
		text(node, "metadata", "uid") != binding.NodeUID || text(node, "spec", "providerID") != binding.ProviderID ||
		text(node, "status", "nodeInfo", "systemUUID") != binding.MachineUUID || text(node, "status", "nodeInfo", "bootID") != binding.BootID ||
		text(node, "status", "nodeInfo", "osImage") != binding.OSImage || value(node, "metadata", "deletionTimestamp") != nil ||
		text(node, "metadata", "annotations", scaleDownDisabled) != "true" || text(node, "metadata", "annotations", fenceAnnotation) != "" ||
		!falseOrAbsent(value(spec, "unschedulable")) || value(spec, "taints") != nil && (!tainted || len(taints) != 0) || !hugepagesReady(node) {
		return 0, false
	}
	room, err := refreshExcess(node, pods)
	availableCPU, cpuErr := runtimeMilliCPU(text(node, "status", "allocatable", "cpu"))
	slots, slotErr := strconv.Atoi(text(node, "status", "allocatable", "pods"))
	if err != nil || cpuErr != nil || slotErr != nil || -room < cnpgRecoveryMemory {
		return 0, false
	}
	for _, pod := range pods {
		phase := text(pod, "status", "phase")
		if text(pod, "spec", "nodeName") != binding.Name || phase == "Succeeded" || phase == "Failed" {
			continue
		}
		request, err := runtimePodCPURequestUpper(pod)
		if err != nil {
			return 0, false
		}
		availableCPU -= request
		slots--
	}
	return -room, availableCPU >= cnpgRecoveryMilliCPU && slots >= 1
}

func (p *cnpgProofRun) chooseRecoveryHost(ctx context.Context) (string, error) {
	_, record, err := p.storage.read(ctx)
	if err != nil || !record.Completed {
		return "", errCNPGRecoveryHost
	}
	live, err := v2Environment(ctx, p.c, p.storage.pools)
	if err != nil {
		return "", err
	}
	return selectCNPGRecoveryHost(live, record.Targets)
}

func (p *cnpgProofRun) verifyRecoveryHostCapacity(ctx context.Context, storage cnpgStorageEnvironment) error {
	hostname := cnpgRecoveryHostname(p.record.Plan)
	if hostname == "" || p.record.Preparation == nil {
		return errCNPGRecoveryHost
	}
	var binding temporaryV2Binding
	for _, candidate := range storage.Targets {
		if candidate.Name == hostname {
			binding = candidate
			break
		}
	}
	agent := p.record.Preparation.Agent
	if agent.Node != hostname || agent.NodeUID != binding.NodeUID || agent.BootID != binding.BootID {
		return errCNPGRecoveryHost
	}
	pods, err := p.c.list(ctx, "pods", "*")
	if err != nil {
		return err
	}
	node, err := p.c.get(ctx, "node", "", hostname, false)
	if err != nil {
		return err
	}
	if _, fits := cnpgRecoveryHostRoom(node, pods, binding); !fits {
		return errCNPGRecoveryHost
	}
	return nil
}
