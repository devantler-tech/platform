package main

import (
	"errors"
	"strings"
)

// verifyV2Runtime requires ready SPDK clients on every worker and three
// independent raw disks on the temporary workers. The original workers retain
// the V1 replicas throughout the disposable recovery proof.
func verifyV2Runtime(s inventory, managers []object, pools map[string]migrationPool, expectedManagerImage string) error {
	if expectedManagerImage == "" || strings.TrimSpace(expectedManagerImage) != expectedManagerImage {
		return errors.New("the reviewed instance-manager image is unresolved")
	}
	if err := verifyMigrationCapacity(s.nodes, pools); err != nil {
		return err
	}
	if err := verifyRedundancy(s); err != nil {
		return err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return err
	}
	if _, err = indexed(managers); err != nil {
		return err
	}
	workers, raw := map[string]bool{}, map[string]bool{}
	for name, n := range nodes {
		if strings.HasPrefix(name, "prod-control-plane-") {
			continue
		}
		if value(n, "spec", "unschedulable") == true || text(n, "status", "allocatable", "cpu") != "8" {
			return errors.New("V2 migration workers must be schedulable with eight allocatable CPUs")
		}
		workers[name] = true
		if _, temporary := pools[text(n, "metadata", "labels", migrationPoolLabel)]; temporary {
			raw[name] = true
		}
	}
	if len(workers) != 6 || len(raw) != 3 {
		return errors.New("V2 canary storage and client roster is incomplete")
	}
	diskIDs := map[string]bool{}
	for name := range raw {
		lh := storage[name]
		status, _ := value(lh, "status").(object)
		diskStatus, _ := value(lh, "status", "diskStatus").(object)
		disk, _ := diskStatus["longhorn-v2"].(object)
		id := text(disk, "diskUUID")
		if !reviewedBlockDisks(value(lh, "spec", "disks")) || value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") != false ||
			!ready(status) || len(diskStatus) != 1 || !ready(disk) || id == "" || diskIDs[id] {
			return errors.New("three distinct ready canonical raw disks are not proven")
		}
		if override := value(lh, "spec", "instanceManagerCPURequest"); override != nil && override != float64(0) {
			return errors.New("a node CPU override supersedes the reviewed V2 reservation")
		}
		diskIDs[id] = true
	}
	return verifyV2ClientPods(s, managers, workers, expectedManagerImage)
}

// verifyV2Clients qualifies the effective clients before any raw disk is
// registered, independently of the later three-disk readiness requirement.
func verifyV2Clients(s inventory, managers []object, pools map[string]migrationPool, expectedManagerImage string) error {
	if err := verifyMigrationCapacity(s.nodes, pools); err != nil {
		return err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return err
	}
	workers := map[string]bool{}
	for name, node := range nodes {
		if strings.HasPrefix(name, "prod-control-plane-") {
			continue
		}
		if value(node, "spec", "unschedulable") == true || text(node, "status", "allocatable", "cpu") != "8" {
			return errors.New("V2 migration workers must be schedulable with eight allocatable CPUs")
		}
		workers[name] = true
	}
	if len(workers) != 6 {
		return errors.New("V2 client roster is incomplete")
	}
	return verifyV2ClientPods(s, managers, workers, expectedManagerImage)
}

// verifyV2ClientPods binds each client to its controller and reviewed image,
// reservations, reactor arguments and isolation on the already checked roster.
func verifyV2ClientPods(s inventory, managers []object, workers map[string]bool, expectedManagerImage string) error {
	if expectedManagerImage == "" || strings.TrimSpace(expectedManagerImage) != expectedManagerImage {
		return errors.New("the reviewed instance-manager image is unresolved")
	}
	if _, err := indexed(managers); err != nil {
		return err
	}
	pods := map[string]object{}
	for _, p := range s.pods {
		if text(p, "metadata", "namespace") != "longhorn-system" {
			continue
		}
		name := text(p, "metadata", "name")
		if pods[name] != nil {
			return errors.New("instance-manager pod inventory is duplicated")
		}
		pods[name] = p
	}
	seen := map[string]bool{}
	for _, im := range managers {
		if text(im, "spec", "dataEngine") == "v1" {
			continue
		}
		if text(im, "spec", "dataEngine") != "v2" {
			return errors.New("an instance manager has an unrecognized data engine")
		}
		node := text(im, "spec", "nodeID")
		if !workers[node] || seen[node] || text(im, "spec", "type") != "aio" || value(im, "metadata", "deletionTimestamp") != nil ||
			text(im, "status", "currentState") != "running" || text(im, "metadata", "namespace") != "longhorn-system" {
			return errors.New("V2 instance managers do not match the ready worker roster")
		}
		p := pods[text(im, "metadata", "name")]
		status, _ := value(p, "status").(object)
		if text(p, "metadata", "uid") == "" || value(p, "metadata", "deletionTimestamp") != nil || text(p, "spec", "nodeName") != node || text(p, "status", "phase") != "Running" || !ready(status) {
			return errors.New("V2 instance-manager pod is missing, misplaced or unready")
		}
		owned := false
		refs, _ := value(p, "metadata", "ownerReferences").([]any)
		for _, ref := range refs {
			r, _ := ref.(object)
			if text(r, "apiVersion") == "longhorn.io/v1beta2" && text(r, "kind") == "InstanceManager" && text(r, "name") == text(im, "metadata", "name") && text(r, "uid") == text(im, "metadata", "uid") && value(r, "controller") == true {
				owned = true
			}
		}
		containers, _ := value(p, "spec", "containers").([]any)
		if !owned || len(containers) != 1 {
			return errors.New("V2 instance-manager workload ownership is unproven")
		}
		container, _ := containers[0].(object)
		if command := value(container, "command"); command != nil {
			items, ok := command.([]any)
			if !ok || len(items) != 0 {
				return errors.New("V2 workload overrides the reviewed image entrypoint")
			}
		}
		cpu := text(container, "resources", "requests", "cpu")
		cpuLimit := text(container, "resources", "limits", "cpu")
		hugepages := text(container, "resources", "limits", "hugepages-2Mi")
		if text(container, "name") != "instance-manager" || text(container, "image") != expectedManagerImage || text(container, "image") != text(im, "spec", "image") || (cpu != "2" && cpu != "2000m") || (cpuLimit != "4" && cpuLimit != "4000m") || (hugepages != "2Gi" && hugepages != "2048Mi") {
			return errors.New("V2 workload lacks its effective two-core and two-GiB huge-page reservation")
		}
		args, _ := value(container, "args").([]any)
		if len(args) == 0 || args[0] != "start-spdk-tgt" {
			return errors.New("V2 workload does not launch the reviewed SPDK target wrapper")
		}
		flags := map[string]int{}
		positions := map[string]int{}
		for i, arg := range args {
			a, ok := arg.(string)
			if !ok {
				return errors.New("V2 workload arguments are malformed")
			}
			for _, flag := range []string{"--spdk-cpumask", "--spdk-core-number", "--spdk-memory-size", "--enable-spdk", "--spdk-enabled", "--enable-irq-affinity", "--enable-workqueue-affinity", "--enable-rps", "--spdk-no-hugepage", "--spdk-interrupt-mode"} {
				if strings.HasPrefix(a, flag+"=") {
					return errors.New("V2 workload overrides a reviewed flag using an assignment")
				}
			}
			flags[a]++
			positions[a] = i
		}
		paired := func(flag, expected string) bool {
			count := 0
			for i, arg := range args {
				if arg == flag {
					if i+1 >= len(args) || args[i+1] != expected {
						return false
					}
					count++
				}
			}
			return count == 1
		}
		if !paired("--spdk-cpumask", "0x3") || !paired("--spdk-core-number", "0") || !paired("--spdk-memory-size", "2048") ||
			flags["start-spdk-tgt"] != 1 || flags["daemon"] != 1 || flags["--enable-spdk"] != 1 || flags["--spdk-enabled"] != 1 ||
			positions["--enable-spdk"] >= positions["daemon"] || positions["--spdk-enabled"] <= positions["daemon"] ||
			flags["--enable-irq-affinity"] != 1 || flags["--enable-workqueue-affinity"] != 1 || flags["--enable-rps"] != 1 || flags["--spdk-no-hugepage"] != 0 || flags["--spdk-interrupt-mode"] != 0 {
			return errors.New("V2 workload has not adopted the reviewed polling reactors, huge pages and CPU isolation")
		}
		seen[node] = true
	}
	if len(seen) != len(workers) {
		return errors.New("V2 volume clients are not ready on every worker")
	}
	return nil
}
