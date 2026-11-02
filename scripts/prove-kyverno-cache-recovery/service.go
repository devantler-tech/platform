package main

import (
	"errors"
	"net/netip"
	"sort"
)

func qualifyService(serviceRaw, endpointsRaw []byte, controllers []object) ([]object, error) {
	fail := errors.New("webhook service routing differs from the qualified admission processes")
	service, err := nativeObject(serviceRaw, "v1", "Service", "kyverno-svc", "kyverno")
	if err != nil {
		return nil, err
	}
	if text(service, "spec", "type") != "ClusterIP" || text(service, "spec", "externalName") != "" {
		return nil, fail
	}
	selector, ok := at(service, "spec", "selector").(map[string]any)
	if !ok || len(selector) == 0 {
		return nil, fail
	}
	pods := map[string]object{}
	for _, binding := range controllers {
		if controllerOwner(binding, "ReplicaSet") == "" {
			continue
		}
		for key, value := range selector {
			wanted, ok := value.(string)
			if !ok || text(binding, "metadata", "labels", key) != wanted {
				return nil, fail
			}
		}
		pods[text(binding, "metadata", "uid")] = binding
	}
	if len(pods) != 2 {
		return nil, fail
	}
	ports, ok := at(service, "spec", "ports").([]any)
	if !ok {
		return nil, fail
	}
	https := 0
	for _, port := range ports {
		if text(port, "name") == "https" {
			if text(port, "protocol") != "TCP" || at(port, "port") != float64(443) || text(port, "targetPort") != "https" {
				return nil, fail
			}
			https++
		}
	}
	if https != 1 {
		return nil, fail
	}
	slices, err := completeList(endpointsRaw, "discovery.k8s.io/v1", "EndpointSliceList")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	bindings := []object{service}
	for _, slice := range slices {
		if text(slice, "metadata", "namespace") != "kyverno" || controllerOwner(slice, "Service") != text(service, "metadata", "uid") || text(slice, "metadata", "labels", "kubernetes.io/service-name") != "kyverno-svc" || text(slice, "addressType") != "IPv4" {
			return nil, fail
		}
		ports, ok := slice["ports"].([]any)
		if !ok {
			return nil, fail
		}
		https := 0
		for _, port := range ports {
			if text(port, "name") == "https" {
				if text(port, "protocol") != "TCP" || at(port, "port") != float64(9443) {
					return nil, fail
				}
				https++
			}
		}
		if https != 1 {
			return nil, fail
		}
		endpoints, ok := slice["endpoints"].([]any)
		if !ok {
			return nil, fail
		}
		for _, endpoint := range endpoints {
			uid := text(endpoint, "targetRef", "uid")
			pod, owned := pods[uid]
			if !owned || seen[uid] || text(endpoint, "targetRef", "kind") != "Pod" || text(endpoint, "targetRef", "namespace") != "kyverno" || text(endpoint, "targetRef", "name") != text(pod, "metadata", "name") || at(endpoint, "conditions", "ready") != true || at(endpoint, "conditions", "serving") != true || at(endpoint, "conditions", "terminating") != false {
				return nil, fail
			}
			addresses, ok := at(endpoint, "addresses").([]any)
			if !ok || len(addresses) != 1 {
				return nil, fail
			}
			address, ok := addresses[0].(string)
			if !ok {
				return nil, fail
			}
			endpointIP, err := netip.ParseAddr(address)
			podIP, podErr := netip.ParseAddr(text(pod, "status", "podIP"))
			if err != nil || podErr != nil || !endpointIP.Is4() || endpointIP != podIP {
				return nil, fail
			}
			seen[uid] = true
		}
		bindings = append(bindings, slice)
	}
	if len(seen) != 2 {
		return nil, fail
	}
	sort.Slice(bindings, func(i, j int) bool {
		return text(bindings[i], "metadata", "uid") < text(bindings[j], "metadata", "uid")
	})
	return bindings, nil
}
