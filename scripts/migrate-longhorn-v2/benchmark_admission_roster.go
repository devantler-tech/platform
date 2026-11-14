package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkControlPlaneCount = 3

var benchmarkControlPlaneProvider = regexp.MustCompile(`^hcloud://[1-9][0-9]{0,18}$`)

type benchmarkAPIServer struct {
	Node, NodeUID, ProviderID, BootID, PodUID, ContainerID string
	InternalIP, ExternalIP                                 netip.Addr
	StartedAt                                              time.Time
}

func observeBenchmarkAPIServers(ctx context.Context, c client) ([]benchmarkAPIServer, error) {
	if c.context != "admin@prod" {
		return nil, errors.New("the protected production API roster is required")
	}
	lists := [3][]object{}
	for index, query := range []struct {
		path, selector, apiVersion, kind string
	}{
		{"/api/v1/nodes", "node-role.kubernetes.io/control-plane", "v1", "NodeList"},
		{"/api/v1/namespaces/kube-system/pods", "component=kube-apiserver,tier=control-plane", "v1", "PodList"},
		{"/apis/discovery.k8s.io/v1/namespaces/default/endpointslices", "kubernetes.io/service-name=kubernetes", "discovery.k8s.io/v1", "EndpointSliceList"},
	} {
		path := query.path + "?" + url.Values{"labelSelector": {query.selector}}.Encode()
		raw, err := c.kubectl(ctx, nil, "get", "--raw="+path)
		if err != nil {
			return nil, errors.New("serving API server inventory is unavailable")
		}
		lists[index], err = decodeBenchmarkAdmissionList(raw, query.apiVersion, query.kind)
		if err != nil {
			return nil, errors.New("serving API server inventory is incomplete")
		}
	}
	return qualifyBenchmarkAPIServers(lists[0], lists[1], lists[2])
}

func decodeBenchmarkAdmissionList(raw []byte, apiVersion, kind string) ([]object, error) {
	var envelope struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Version   string `json:"resourceVersion"`
			Remaining int64  `json:"remainingItemCount"`
		} `json:"metadata"`
	}
	if strictjson.Validate(raw, map[string][]string{
		"":         {"apiVersion", "kind", "metadata", "items"},
		"metadata": {"resourceVersion", "remainingItemCount", "continue"},
	}) != nil || json.Unmarshal(raw, &envelope) != nil || envelope.APIVersion != apiVersion ||
		envelope.Kind != kind || envelope.Metadata.Version == "" || envelope.Metadata.Remaining != 0 {
		return nil, errors.New("native API server inventory did not identify a complete revision")
	}
	return decodeList(raw)
}

// The service publishes internal addresses. Each address must match the mirror
// Pod on a Ready control-plane host, whose direct external route can be verified
// separately against the provider. A service or floating IP alone cannot prove
// that a policy is active on each API server.
func qualifyBenchmarkAPIServers(nodes, pods, slices []object) ([]benchmarkAPIServer, error) {
	incomplete := errors.New("complete native serving API server roster is required")
	if len(nodes) != benchmarkControlPlaneCount || len(pods) != len(nodes) || len(slices) != 1 {
		return nil, incomplete
	}
	nodeByName, err := indexed(nodes)
	if err != nil {
		return nil, incomplete
	}
	podByName, err := indexed(pods)
	if err != nil {
		return nil, incomplete
	}
	served, err := benchmarkServiceAddresses(slices[0])
	if err != nil {
		return nil, err
	}

	roster := make([]benchmarkAPIServer, 0, len(nodes))
	direct := map[netip.Addr]bool{}
	providers := map[string]bool{}
	for name, node := range nodeByName {
		pod := podByName["kube-apiserver-"+name]
		endpoint, err := benchmarkControlPlaneEndpoint(node, pod)
		if err != nil || !served[endpoint.InternalIP] || direct[endpoint.ExternalIP] || providers[endpoint.ProviderID] {
			return nil, incomplete
		}
		delete(served, endpoint.InternalIP)
		direct[endpoint.ExternalIP], providers[endpoint.ProviderID] = true, true
		roster = append(roster, endpoint)
	}
	sort.Slice(roster, func(i, j int) bool { return roster[i].Node < roster[j].Node })
	return roster, nil
}

func benchmarkServiceAddresses(slice object) (map[netip.Addr]bool, error) {
	incomplete := errors.New("default Kubernetes API service endpoints are unverified")
	ports, _ := value(slice, "ports").([]any)
	endpoints, _ := value(slice, "endpoints").([]any)
	if text(slice, "metadata", "namespace") != "default" || text(slice, "metadata", "uid") == "" ||
		text(slice, "metadata", "labels", "kubernetes.io/service-name") != "kubernetes" ||
		text(slice, "addressType") != "IPv4" || len(ports) != 1 || len(endpoints) != benchmarkControlPlaneCount {
		return nil, incomplete
	}
	port, _ := ports[0].(object)
	if text(port, "name") != "https" || text(port, "protocol") != "TCP" || value(port, "port") != float64(6443) {
		return nil, incomplete
	}
	served := make(map[netip.Addr]bool, len(endpoints))
	for _, item := range endpoints {
		endpoint, _ := item.(object)
		addresses, _ := value(endpoint, "addresses").([]any)
		if len(addresses) != 1 || value(endpoint, "conditions", "ready") != true ||
			value(endpoint, "conditions", "terminating") == true {
			return nil, incomplete
		}
		raw, _ := addresses[0].(string)
		ip, err := netip.ParseAddr(raw)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || served[ip] {
			return nil, incomplete
		}
		served[ip] = true
	}
	return served, nil
}

func benchmarkControlPlaneEndpoint(node, pod object) (benchmarkAPIServer, error) {
	incomplete := errors.New("control-plane Pod is not bound to its observed host")
	endpoint := benchmarkAPIServer{
		Node: text(node, "metadata", "name"), NodeUID: text(node, "metadata", "uid"),
		ProviderID: text(node, "spec", "providerID"), BootID: text(node, "status", "nodeInfo", "bootID"),
		PodUID: text(pod, "metadata", "uid"),
	}
	_, controlPlane := valueObject(node, "metadata", "labels")["node-role.kubernetes.io/control-plane"]
	if !controlPlane || !kubeNamespace.MatchString(endpoint.Node) ||
		!benchmarkControlPlaneProvider.MatchString(endpoint.ProviderID) || endpoint.BootID == "" ||
		!ready(valueObject(node, "status")) || endpoint.PodUID == "" ||
		text(pod, "metadata", "namespace") != "kube-system" || text(pod, "spec", "nodeName") != endpoint.Node ||
		text(pod, "metadata", "annotations", "kubernetes.io/config.mirror") == "" ||
		value(pod, "spec", "hostNetwork") != true || text(pod, "status", "phase") != "Running" ||
		!ready(valueObject(pod, "status")) {
		return benchmarkAPIServer{}, incomplete
	}
	owners, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 {
		return benchmarkAPIServer{}, incomplete
	}
	owner, _ := owners[0].(object)
	if text(owner, "kind") != "Node" || text(owner, "apiVersion") != "v1" || value(owner, "controller") != true ||
		text(owner, "name") != endpoint.Node || text(owner, "uid") != endpoint.NodeUID {
		return benchmarkAPIServer{}, incomplete
	}
	var err error
	endpoint.InternalIP, endpoint.ExternalIP, err = benchmarkControlPlaneAddresses(node)
	if err != nil || text(pod, "status", "hostIP") != endpoint.InternalIP.String() ||
		text(pod, "status", "podIP") != endpoint.InternalIP.String() {
		return benchmarkAPIServer{}, incomplete
	}
	containers, _ := value(pod, "status", "containerStatuses").([]any)
	if len(containers) != 1 {
		return benchmarkAPIServer{}, incomplete
	}
	running, _ := containers[0].(object)
	endpoint.ContainerID = text(running, "containerID")
	endpoint.StartedAt, err = time.Parse(time.RFC3339Nano, text(running, "state", "running", "startedAt"))
	if text(running, "name") != "kube-apiserver" || value(running, "ready") != true ||
		value(running, "started") != true || !benchmarkContainerID.MatchString(endpoint.ContainerID) ||
		err != nil || endpoint.StartedAt.IsZero() {
		return benchmarkAPIServer{}, incomplete
	}
	return endpoint, nil
}

func benchmarkControlPlaneAddresses(node object) (internal, external netip.Addr, err error) {
	incomplete := errors.New("control-plane address pairs are incomplete")
	addresses, _ := value(node, "status", "addresses").([]any)
	for _, item := range addresses {
		address, _ := item.(object)
		var target *netip.Addr
		switch text(address, "type") {
		case "InternalIP":
			target = &internal
		case "ExternalIP":
			target = &external
		default:
			continue
		}
		ip, parseErr := netip.ParseAddr(text(address, "address"))
		if parseErr != nil || !ip.Is4() || !ip.IsGlobalUnicast() || target.IsValid() {
			return netip.Addr{}, netip.Addr{}, incomplete
		}
		*target = ip
	}
	if !internal.IsPrivate() || !external.IsValid() || external.IsPrivate() {
		return netip.Addr{}, netip.Addr{}, incomplete
	}
	return internal, external, nil
}
