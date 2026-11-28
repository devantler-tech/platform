package main

import "reflect"

type openBaoSurvivingMember struct {
	Pod, Node benchmarkResource
}

// Track the same two voters through the disruption. The Raft judge remains
// responsible for checking voter roles and leadership.
func bindOpenBaoSurvivors(original openBaoProviderStorage, stateful object, pods, nodes []object) ([2]openBaoSurvivingMember, error) {
	var peers [2]openBaoSurvivingMember
	if !knownOpenBaoMember(original.Member.Name) || benchmarkIdentity(stateful) != original.StatefulSet ||
		!recoveredNativeObject(stateful, "apps/v1", "StatefulSet", openBaoNamespace) {
		return peers, errOpenBaoProviderStorage
	}
	i := 0
	for _, name := range [3]string{"openbao-0", "openbao-1", "openbao-2"} {
		if name == original.Member.Name {
			continue
		}
		pod := uniqueOpenBaoObject(pods, name)
		node := uniqueOpenBaoObject(nodes, text(pod, "spec", "nodeName"))
		if !openBaoOriginalStorageMember(pod, stateful) || !recoveredNativeObject(node, "v1", "Node", "") ||
			!ready(valueObject(node, "status")) || text(node, "metadata", "name") == original.Node ||
			value(node, "spec", "unschedulable") == true {
			return peers, errOpenBaoProviderStorage
		}
		peers[i] = openBaoSurvivingMember{Pod: benchmarkIdentity(pod), Node: benchmarkIdentity(node)}
		i++
	}
	if peers[0].Node == peers[1].Node || peers[0].Pod.UID == peers[1].Pod.UID {
		return peers, errOpenBaoProviderStorage
	}
	return peers, nil
}

func verifyOpenBaoSurvivors(original openBaoProviderStorage, stateful object, peers [2]openBaoSurvivingMember,
	pods, nodes []object, service object, slices []object) error {
	current, err := bindOpenBaoSurvivors(original, stateful, pods, nodes)
	if err != nil || current != peers || !openBaoActiveRoute(service, slices, pods, peers) {
		return errOpenBaoProviderStorage
	}
	return nil
}

func openBaoActiveRoute(service object, slices, pods []object, peers [2]openBaoSurvivingMember) bool {
	const name = "openbao-active"
	if !recoveredNativeObject(service, "v1", "Service", openBaoNamespace) || text(service, "metadata", "name") != name ||
		text(service, "spec", "selector", name) != "true" || len(slices) != 1 {
		return false
	}
	slice := slices[0]
	if !recoveredNativeObject(slice, "discovery.k8s.io/v1", "EndpointSlice", openBaoNamespace) ||
		text(slice, "metadata", "labels", "kubernetes.io/service-name") != name ||
		text(slice, "metadata", "labels", "endpointslice.kubernetes.io/managed-by") != "endpointslice-controller.k8s.io" ||
		text(slice, "addressType") != "IPv4" {
		return false
	}
	refs, _ := value(slice, "metadata", "ownerReferences").([]any)
	endpoints, _ := value(slice, "endpoints").([]any)
	if len(refs) != 1 || len(endpoints) != 1 {
		return false
	}
	owner, _ := refs[0].(object)
	if text(owner, "apiVersion") != "v1" || text(owner, "kind") != "Service" || text(owner, "name") != name ||
		text(owner, "uid") != benchmarkIdentity(service).UID || value(owner, "controller") != true {
		return false
	}
	endpoint, _ := endpoints[0].(object)
	ref := valueObject(endpoint, "targetRef")
	if value(endpoint, "conditions", "ready") != true || value(endpoint, "conditions", "serving") != true ||
		value(endpoint, "conditions", "terminating") != false || text(ref, "kind") != "Pod" || text(ref, "namespace") != openBaoNamespace {
		return false
	}
	for _, peer := range peers {
		if text(ref, "name") != peer.Pod.Name || text(ref, "uid") != peer.Pod.UID || text(endpoint, "nodeName") != peer.Node.Name {
			continue
		}
		pod := uniqueOpenBaoObject(pods, peer.Pod.Name)
		address := text(pod, "status", "podIP")
		return address != "" && reflect.DeepEqual(value(endpoint, "addresses"), []any{address})
	}
	return false
}
