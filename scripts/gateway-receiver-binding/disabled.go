package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
)

const receiverOwner = "platform.devantler.tech/gateway-receiver-owner"
const listenerType = "type.googleapis.com/envoy.config.listener.v3.Listener"

type receiverConfig struct {
	APIVersion, Kind string
	Metadata         struct {
		metadata
		DeletionTimestamp *string
	}
	Spec struct{ Resources []map[string]any }
}

// disabledPatch removes only fields installed by this receiver's declared policy.
// The native main recovery path uses it after removing the exact policy bindings.
// It never replaces the controller-generated resource or its other route fields.
func disabledPatch(raw []byte, b binding) ([]byte, error) {
	bad := func() ([]byte, error) { return nil, errors.New("disabled-receiver-state") }
	var o receiverConfig
	if strictJSON(raw, &o) != nil || o.APIVersion != "cilium.io/v2" || o.Kind != "CiliumEnvoyConfig" || o.Metadata.Name != "cilium-gateway-platform" || o.Metadata.Namespace != "kube-system" || !uuid.MatchString(o.Metadata.UID) || o.Metadata.ResourceVersion == "" || o.Metadata.DeletionTimestamp != nil {
		return bad()
	}
	owners := o.Metadata.OwnerReferences
	if len(owners) != 1 || owners[0].Kind != "Gateway" || owners[0].Name != "platform" || owners[0].APIVersion != "gateway.networking.k8s.io/v1" || owners[0].UID != b.GatewayUID || !owners[0].Controller {
		return bad()
	}
	prefix, err := netip.ParsePrefix(b.CIDR)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 {
		return bad()
	}
	marker, present := o.Metadata.Annotations[receiverOwner]
	if present && marker != "v1" {
		return bad()
	}
	ops := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": o.Metadata.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": o.Metadata.ResourceVersion},
	}
	listeners, tls := 0, 0
	for ri, r := range o.Spec.Resources {
		if r["@type"] != listenerType {
			continue
		}
		listeners++
		if r["name"] != "listener" {
			return bad()
		}
		for _, alias := range []string{"default_filter_chain", "filter_chain_matcher", "filter_chains", "listener_filters"} {
			if _, ok := r[alias]; ok {
				return bad()
			}
		}
		filters, err := json.Marshal(r["listenerFilters"])
		if err != nil || bytes.Contains(filters, []byte("envoy.filters.listener.proxy_protocol")) || bytes.Contains(filters, []byte("envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol")) {
			return bad()
		}
		if _, ok := r["defaultFilterChain"]; ok {
			return bad()
		}
		if _, ok := r["filterChainMatcher"]; ok {
			return bad()
		}
		chains, ok := r["filterChains"].([]any)
		if !ok || len(chains) == 0 {
			return bad()
		}
		for ci, item := range chains {
			chain, ok := item.(map[string]any)
			if !ok {
				return bad()
			}
			for _, alias := range []string{"filter_chain_match", "transport_socket"} {
				if _, ok := chain[alias]; ok {
					return bad()
				}
			}
			match, ok := chain["filterChainMatch"].(map[string]any)
			if !ok {
				return bad()
			}
			for _, alias := range []string{"transport_protocol", "direct_source_prefix_ranges"} {
				if _, ok := match[alias]; ok {
					return bad()
				}
			}
			value, hasPeer := match["directSourcePrefixRanges"]
			switch match["transportProtocol"] {
			case "raw_buffer":
				if hasPeer {
					return bad()
				}
				if _, hasTLS := chain["transportSocket"]; hasTLS {
					return bad()
				}
			case "tls":
				tls++
				if !hasPeer {
					continue
				}
				expected := []any{map[string]any{"addressPrefix": prefix.Addr().String(), "prefixLen": float64(32)}}
				if !present || !reflect.DeepEqual(value, expected) {
					return bad()
				}
				ops = append(ops, map[string]any{"op": "remove", "path": fmt.Sprintf("/spec/resources/%d/filterChains/%d/filterChainMatch/directSourcePrefixRanges", ri, ci)})
			default:
				return bad()
			}
		}
	}
	if listeners != 1 || tls == 0 {
		return bad()
	}
	if present {
		ops = append(ops, map[string]any{"op": "remove", "path": "/metadata/annotations/platform.devantler.tech~1gateway-receiver-owner"})
	}
	if len(ops) == 2 {
		return nil, nil
	}
	return json.Marshal(ops)
}
