package main

import (
	"encoding/json"
	"errors"
	"reflect"
)

type receiverMode string

const (
	receiverDisabled       receiverMode = "disabled"
	receiverFencedDisabled receiverMode = "fenced-disabled"
	receiverFencedActive   receiverMode = "fenced-active"
)

type receiverState struct {
	Mode         receiverMode
	UID, Version string
}

// inspectReceiver classifies an owned receiver without changing it. In
// particular, observing known active state never authorizes fence cleanup.
func inspectReceiver(raw []byte, b binding) (receiverState, error) {
	bad := func() (receiverState, error) { return receiverState{}, errors.New("receiver-state-unverified") }
	var body map[string]any
	if strictJSON(raw, &body) != nil {
		return bad()
	}
	meta, ok := body["metadata"].(map[string]any)
	if !ok || meta["name"] != "cilium-gateway-platform" || meta["namespace"] != "kube-system" {
		return bad()
	}
	uid, uidOK := meta["uid"].(string)
	version, versionOK := meta["resourceVersion"].(string)
	owners, ownersOK := meta["ownerReferences"].([]any)
	if !uidOK || !versionOK || !uuid.MatchString(uid) || version == "" || !ownersOK || len(owners) != 1 {
		return bad()
	}
	owner, ok := owners[0].(map[string]any)
	if !ok || owner["apiVersion"] != "gateway.networking.k8s.io/v1" || owner["kind"] != "Gateway" || owner["name"] != "platform" || owner["uid"] != b.GatewayUID || owner["controller"] != true {
		return bad()
	}
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		return bad()
	}
	resources, ok := spec["resources"].([]any)
	if !ok {
		return bad()
	}
	proxy, tls, fenced := false, 0, 0
	for _, item := range resources {
		r, ok := item.(map[string]any)
		if !ok {
			return bad()
		}
		if r["@type"] != listenerType {
			continue
		}
		if filters, present := r["listenerFilters"]; present {
			list, ok := filters.([]any)
			if !ok {
				return bad()
			}
			for index, entry := range list {
				filter, ok := entry.(map[string]any)
				if !ok {
					return bad()
				}
				if filter["name"] != "envoy.filters.listener.proxy_protocol" {
					continue
				}
				config, ok := filter["typedConfig"].(map[string]any)
				if !ok || proxy || index != 0 || len(filter) != 2 || len(config) < 1 || len(config) > 2 || config["@type"] != "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol" {
					return bad()
				}
				if optional, present := config["allowRequestsWithoutProxyProtocol"]; present {
					if optional != false {
						return bad()
					}
				} else if len(config) != 1 {
					return bad()
				}
				expectedTLS := map[string]any{"name": "envoy.filters.listener.tls_inspector", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector"}}
				if len(list) != 2 || !reflect.DeepEqual(list[1], expectedTLS) {
					return bad()
				}
				proxy = true
				// Remove only from this private inspection copy. The original
				// bytes still fail disabledPatch's active-listener guard.
				r["listenerFilters"] = list[1:]
			}
		}
		chains, ok := r["filterChains"].([]any)
		if !ok {
			return bad()
		}
		for _, item := range chains {
			chain, ok := item.(map[string]any)
			if !ok {
				return bad()
			}
			match, ok := chain["filterChainMatch"].(map[string]any)
			if !ok {
				return bad()
			}
			if match["transportProtocol"] == "tls" {
				tls++
				if _, present := match["directSourcePrefixRanges"]; present {
					fenced++
				}
			}
		}
	}
	copy, err := json.Marshal(body)
	if err != nil {
		return bad()
	}
	if _, err = disabledPatch(copy, b); err != nil {
		return bad()
	}
	annotations, _ := meta["annotations"].(map[string]any)
	owned := annotations[receiverOwner] == "v1"
	mode := receiverDisabled
	switch {
	case tls > 0 && fenced == tls && owned:
		mode = receiverFencedDisabled
		if proxy {
			mode = receiverFencedActive
		}
	case fenced != 0 || owned || proxy:
		return bad()
	}
	return receiverState{Mode: mode, UID: uid, Version: version}, nil
}
