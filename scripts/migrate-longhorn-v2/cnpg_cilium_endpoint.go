package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type recoveryCiliumEndpoint struct {
	PodUID, CEPUID, Node, Addresses, Attachment string
	EndpointID, SecurityID, PolicyRevision      uint64
	Monitor                                     recoveryCiliumMonitorConfig
}

func nativeObservationUint(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9007199254740991 || math.Trunc(n) != n {
			return 0, false
		}
		return uint64(n), true
	case json.Number:
		value, err := strconv.ParseUint(string(n), 10, 64)
		return value, err == nil
	default:
		return 0, false
	}
}

func recoveryAddress(raw string) (string, error) {
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
		return "", errors.New("recovery endpoint address is missing or invalid")
	}
	return ip.Unmap().String(), nil
}

func nativeCiliumAddresses(raw any) (map[string]bool, error) {
	pairs, ok := raw.([]any)
	if !ok || len(pairs) == 0 || len(pairs) > 2 {
		return nil, errors.New("native endpoint addressing is incomplete")
	}
	result := map[string]bool{}
	for _, pair := range pairs {
		p, ok := pair.(object)
		if !ok {
			return nil, errors.New("native endpoint address pair is invalid")
		}
		count := 0
		for _, key := range []string{"ipv4", "ipv6"} {
			if p[key] == nil || p[key] == "" {
				continue
			}
			address, err := recoveryAddress(text(p, key))
			if err != nil || result[address] || (key == "ipv4") != netip.MustParseAddr(address).Is4() {
				return nil, errors.New("native endpoint address is invalid or duplicated")
			}
			result[address] = true
			count++
		}
		if count == 0 {
			return nil, errors.New("native endpoint address pair is empty")
		}
	}
	return result, nil
}

func recoveryNamespaceIdentity(raw any, ns string) bool {
	labels, ok := raw.([]any)
	if !ok {
		return false
	}
	count := 0
	for _, label := range labels {
		s, ok := label.(string)
		if !ok {
			return false
		}
		if strings.HasPrefix(s, "k8s:io.kubernetes.pod.namespace=") {
			if s != "k8s:io.kubernetes.pod.namespace="+ns {
				return false
			}
			count++
		}
	}
	return count == 1
}

func verifyCiliumPodCEPOwnership(cep, pod object) error {
	ns, name := text(pod, "metadata", "namespace"), text(pod, "metadata", "name")
	if ns == "" || name == "" || !isolationUID.MatchString(text(pod, "metadata", "uid")) {
		return errors.New("pod identity for the CiliumEndpoint is incomplete")
	}
	if text(cep, "apiVersion") != "cilium.io/v2" || text(cep, "kind") != "CiliumEndpoint" || text(cep, "metadata", "namespace") != ns || text(cep, "metadata", "name") != name || !isolationUID.MatchString(text(cep, "metadata", "uid")) || value(cep, "metadata", "deletionTimestamp") != nil {
		return errors.New("recovery CiliumEndpoint identity is incomplete")
	}
	owners, ok := value(cep, "metadata", "ownerReferences").([]any)
	if !ok || len(owners) != 1 {
		return errors.New("recovery CiliumEndpoint ownership is ambiguous")
	}
	owner, ok := owners[0].(object)
	if !ok || text(owner, "apiVersion") != "v1" || text(owner, "kind") != "Pod" || text(owner, "name") != name || text(owner, "uid") != text(pod, "metadata", "uid") {
		return errors.New("recovery CiliumEndpoint does not belong to the exact Pod")
	}
	return nil
}

func ciliumPodAddresses(cep, pod object) (map[string]bool, error) {
	primary, err := recoveryAddress(text(pod, "status", "podIP"))
	if err != nil {
		return nil, err
	}
	podIPs, ok := value(pod, "status", "podIPs").([]any)
	if !ok || len(podIPs) == 0 || len(podIPs) > 2 {
		return nil, errors.New("recovery Pod IP inventory is incomplete")
	}
	addresses := map[string]bool{}
	for _, rawIP := range podIPs {
		item, ok := rawIP.(object)
		if !ok {
			return nil, errors.New("recovery Pod IP is malformed")
		}
		ip, err := recoveryAddress(text(item, "ip"))
		if err != nil || addresses[ip] {
			return nil, errors.New("recovery Pod IP is invalid or duplicated")
		}
		addresses[ip] = true
	}
	cepIPs, err := nativeCiliumAddresses(value(cep, "status", "networking", "addressing"))
	if !addresses[primary] || err != nil || !reflect.DeepEqual(addresses, cepIPs) {
		return nil, errors.New("cilium endpoint does not identify the exact Pod IPs")
	}
	return addresses, nil
}

// Payload binding only. The caller must validate the owned CNPG Pod/Job graph,
// query the bound agent on this Node, prove its process continuity and exact CNP
// import, then re-read Pod/CEP/endpoint identities around live network probes.
// A CEP's compressed ready state never substitutes for native datapath state.
func bindRecoveryCiliumEndpoint(raw []byte, cep, pod object, importedRevision uint64) (recoveryCiliumEndpoint, error) {
	ns := text(pod, "metadata", "namespace")
	if !isolationNamespace.MatchString(ns) {
		return recoveryCiliumEndpoint{}, errors.New("recovery endpoint does not belong to the reviewed recovery namespace")
	}
	return bindCiliumEndpoint(raw, cep, pod, importedRevision, ns, "Running")
}

func bindCiliumEndpoint(raw []byte, cep, pod object, importedRevision uint64, namespace, phase string) (recoveryCiliumEndpoint, error) {
	var proof recoveryCiliumEndpoint
	ns, name := text(pod, "metadata", "namespace"), text(pod, "metadata", "name")
	if importedRevision == 0 || importedRevision > 1<<63-1 || len(raw) == 0 || len(raw) > 1<<20 || ns != namespace || !kubeNamespace.MatchString(namespace) || name == "" || !isolationUID.MatchString(text(pod, "metadata", "uid")) || text(pod, "status", "phase") != phase || text(pod, "spec", "nodeName") == "" || value(pod, "spec", "hostNetwork") == true || value(pod, "metadata", "deletionTimestamp") != nil {
		return proof, errors.New("recovery endpoint lacks an import revision or exact running Pod")
	}
	if err := verifyCiliumPodCEPOwnership(cep, pod); err != nil {
		return proof, err
	}
	var endpoints []object
	if decodeIsolationJSON(raw, &endpoints) != nil || len(endpoints) != 1 {
		return proof, errors.New("native endpoint lookup is malformed or ambiguous")
	}
	endpoint := endpoints[0]
	endpointID, idOK := nativeObservationUint(value(endpoint, "id"))
	cepID, cepOK := nativeObservationUint(value(cep, "status", "id"))
	identity, identityOK := nativeObservationUint(value(endpoint, "status", "identity", "id"))
	cepIdentity, cepIdentityOK := nativeObservationUint(value(cep, "status", "identity", "id"))
	realizedIdentity, realizedOK := nativeObservationUint(value(endpoint, "status", "policy", "realized", "id"))
	revision, revisionOK := nativeObservationUint(value(endpoint, "status", "policy", "realized", "policy-revision"))
	if !idOK || !cepOK || endpointID == 0 || endpointID > 65535 || endpointID != cepID || !identityOK || !cepIdentityOK || !realizedOK || identity == 0 || identity > 1<<32-1 || identity != cepIdentity || identity != realizedIdentity || !revisionOK || revision < importedRevision || revision > 1<<63-1 || text(endpoint, "status", "state") != "ready" || text(endpoint, "status", "policy", "realized", "policy-enabled") != "both" || !recoveryNamespaceIdentity(value(endpoint, "status", "identity", "labels"), ns) || !recoveryNamespaceIdentity(value(cep, "status", "identity", "labels"), ns) {
		return proof, errors.New("native recovery endpoint identity or realized enforcement is not current")
	}
	status, _ := value(endpoint, "status").(object)
	monitor, err := bindRecoveryCiliumMonitorConfig(status)
	if err != nil {
		return proof, err
	}
	attachment := text(endpoint, "status", "external-identifiers", "cni-attachment-id")
	if attachment == "" || len(attachment) > 512 || strings.ContainsAny(attachment, "\r\n\x00") {
		return proof, errors.New("native recovery CNI attachment identity is incomplete")
	}
	if rawIdentifiers, present := value(cep, "status").(object)["external-identifiers"]; present {
		identifiers, valid := rawIdentifiers.(object)
		if !valid {
			return proof, errors.New("recovery CEP external identifiers are malformed")
		}
		if rawAttachment, present := identifiers["cni-attachment-id"]; present {
			cepAttachment, valid := rawAttachment.(string)
			if !valid || cepAttachment != attachment {
				return proof, errors.New("recovery CEP and native CNI attachment identities disagree")
			}
		}
	}
	addresses, err := ciliumPodAddresses(cep, pod)
	if err != nil {
		return proof, err
	}
	nativeIPs, nativeErr := nativeCiliumAddresses(value(endpoint, "status", "networking", "addressing"))
	if nativeErr != nil || !reflect.DeepEqual(addresses, nativeIPs) {
		return proof, errors.New("native endpoint does not identify the exact Pod IPs")
	}
	sorted := make([]string, 0, len(addresses))
	for ip := range addresses {
		sorted = append(sorted, ip)
	}
	sort.Strings(sorted)
	proof = recoveryCiliumEndpoint{PodUID: text(pod, "metadata", "uid"), CEPUID: text(cep, "metadata", "uid"), Node: text(pod, "spec", "nodeName"), Addresses: strings.Join(sorted, ","), Attachment: attachment, EndpointID: endpointID, SecurityID: identity, PolicyRevision: revision, Monitor: monitor}
	return proof, nil
}
