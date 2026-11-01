package main

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
)

type benchmarkManagerCPU struct{ Node, PodUID, Image, ImageID, Request, Limit string }
type benchmarkCPU struct {
	ClientRequest, ClientLimit string
	Managers                   map[string]benchmarkManagerCPU
}

func cpuQuantity(s string) bool {
	return regexp.MustCompile(`^[1-9][0-9]*(m|\.[0-9]+)?$`).MatchString(s)
}

func validateBenchmarkCPU(cpu benchmarkCPU) error {
	if cpu.ClientRequest != "1" || cpu.ClientLimit != "2" || len(cpu.Managers) < 3 {
		return errors.New("admitted benchmark CPU context is incomplete")
	}
	nodes := map[string]bool{}
	for name, m := range cpu.Managers {
		if name == "" || m.PodUID == "" || m.Image == "" || m.ImageID == "" || !cpuQuantity(m.Request) || (m.Limit != "unbounded" && !cpuQuantity(m.Limit)) ||
			(m.Node != "prod-worker-1" && m.Node != "prod-worker-2" && m.Node != "prod-worker-3") {
			return errors.New("storage instance-manager CPU context is incomplete")
		}
		nodes[m.Node] = true
	}
	if len(nodes) != 3 {
		return errors.New("storage CPU observations must cover all three permanent workers")
	}
	return nil
}

func benchmarkCompute(s inventory, managers []object, probe object, phase string) (benchmarkCPU, error) {
	result := benchmarkCPU{Managers: map[string]benchmarkManagerCPU{}}
	containers, _ := value(probe, "spec", "containers").([]any)
	if len(containers) != 1 {
		return result, errors.New("benchmark client CPU allocation is ambiguous")
	}
	container, _ := containers[0].(object)
	result.ClientRequest, result.ClientLimit = text(container, "resources", "requests", "cpu"), text(container, "resources", "limits", "cpu")
	if result.ClientRequest == "1000m" {
		result.ClientRequest = "1"
	}
	if result.ClientLimit == "2000m" {
		result.ClientLimit = "2"
	}
	if _, err := indexed(managers); err != nil {
		return result, err
	}
	for _, im := range managers {
		if text(im, "spec", "dataEngine") != phase {
			continue
		}
		node, name := text(im, "spec", "nodeID"), text(im, "metadata", "name")
		if node != "prod-worker-1" && node != "prod-worker-2" && node != "prod-worker-3" {
			continue
		}
		if text(im, "status", "currentState") != "running" {
			return result, errors.New("benchmark storage instance manager is changing")
		}
		var pod object
		for _, p := range s.pods {
			if text(p, "metadata", "namespace") == "longhorn-system" && text(p, "metadata", "name") == name {
				if pod != nil {
					return result, errors.New("duplicate storage CPU observation")
				}
				pod = p
			}
		}
		status, _ := value(pod, "status").(object)
		if text(pod, "metadata", "uid") == "" || value(pod, "metadata", "deletionTimestamp") != nil || text(pod, "status", "phase") != "Running" || !ready(status) || text(pod, "spec", "nodeName") != node {
			return result, errors.New("benchmark storage CPU workload is unready")
		}
		owned := false
		refs, _ := value(pod, "metadata", "ownerReferences").([]any)
		for _, ref := range refs {
			r, _ := ref.(object)
			if text(r, "kind") == "InstanceManager" && text(r, "apiVersion") == "longhorn.io/v1beta2" && text(r, "uid") == text(im, "metadata", "uid") && text(r, "name") == name && value(r, "controller") == true {
				owned = true
			}
		}
		cs, _ := value(pod, "spec", "containers").([]any)
		statuses, _ := value(pod, "status", "containerStatuses").([]any)
		if !owned || len(cs) != 1 || len(statuses) != 1 {
			return result, errors.New("storage CPU workload ownership or executable is unproven")
		}
		c, _ := cs[0].(object)
		running, _ := statuses[0].(object)
		if text(c, "image") != text(im, "spec", "image") {
			return result, errors.New("storage CPU image differs from its instance manager")
		}
		limit := text(c, "resources", "limits", "cpu")
		if limit == "" {
			limit = "unbounded"
		}
		result.Managers[name] = benchmarkManagerCPU{Node: node, PodUID: text(pod, "metadata", "uid"), Image: text(c, "image"), ImageID: text(running, "imageID"), Request: text(c, "resources", "requests", "cpu"), Limit: limit}
	}
	return result, validateBenchmarkCPU(result)
}

func captureBenchmarkCPU(ctx context.Context, c client, phase string, pod object) (benchmarkCPU, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return benchmarkCPU{}, err
	}
	managers, err := c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return benchmarkCPU{}, err
	}
	return benchmarkCompute(s, managers, pod, phase)
}

func publicBenchmarkCPU(cpu benchmarkCPU) string {
	requests, limits := map[string]bool{}, map[string]bool{}
	for _, m := range cpu.Managers {
		requests[m.Request] = true
		limits[m.Limit] = true
	}
	keys := func(m map[string]bool) []string {
		result := []string{}
		for key := range m {
			result = append(result, key)
		}
		sort.Strings(result)
		return result
	}
	return "client request/limit=" + cpu.ClientRequest + "/" + cpu.ClientLimit + "; storage requests=" + strings.Join(keys(requests), ",") + "; storage limits=" + strings.Join(keys(limits), ",")
}
