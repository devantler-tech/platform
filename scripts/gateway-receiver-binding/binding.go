package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"regexp"
)

type binding struct {
	CIDR, ServiceUID, GatewayUID, ServiceVersion, GatewayVersion, PublicIPv4 string
	HTTPNodePort, HTTPSNodePort                                              int
}
type metadata struct {
	Name, Namespace, UID, ResourceVersion string
	Generation                            int64
	Labels, Annotations                   map[string]string
	OwnerReferences                       []struct {
		APIVersion, Kind, Name, UID string
		Controller                  bool
	}
}
type gatewayObject struct {
	Metadata metadata
	Spec     struct{ GatewayClassName string }
	Status   struct {
		Conditions []struct {
			Type, Status       string
			ObservedGeneration int64
		}
	}
}
type serviceObject struct {
	Metadata metadata
	Spec     struct {
		Type, ExternalTrafficPolicy string
		IPFamilies                  []string
		Ports                       []struct {
			Port, NodePort int
			Protocol       string
		}
	}
	Status struct {
		LoadBalancer struct{ Ingress []struct{ IP, IPMode string } }
	}
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func strictJSON(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > 512<<10 {
		return errors.New("input-size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 32 {
			return errors.New("input-depth")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("input-json")
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				s, ok := key.(string)
				if err != nil || !ok || seen[s] {
					return errors.New("input-keys")
				}
				seen[s] = true
				if err = visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("input-json")
		}
		_, err = d.Token()
		return err
	}
	if visit(0) != nil {
		return errors.New("input-json")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("input-trailing")
	}
	if json.Unmarshal(raw, v) != nil {
		return errors.New("input-type")
	}
	return nil
}
func derive(service, gateway []byte) (binding, error) {
	return deriveTransport(service, gateway, false)
}

func deriveRecovery(service, gateway []byte) (binding, error) {
	return deriveTransport(service, gateway, true)
}

func deriveTransport(service, gateway []byte, recovering bool) (binding, error) {
	return deriveExpectedTransport(service, gateway, false, recovering)
}

func deriveExpectedTransport(service, gateway []byte, wantProxy, recovering bool) (binding, error) {
	refuse := func() (binding, error) { return binding{}, errors.New("unbound-provider") }
	var s serviceObject
	var g gatewayObject
	if strictJSON(service, &s) != nil || strictJSON(gateway, &g) != nil {
		return refuse()
	}
	if s.Metadata.Name != "cilium-gateway-platform" || s.Metadata.Namespace != "kube-system" || !uuid.MatchString(s.Metadata.UID) || s.Metadata.ResourceVersion == "" || s.Metadata.Labels["io.cilium.gateway/owning-gateway"] != "platform" {
		return refuse()
	}
	if g.Metadata.Name != "platform" || g.Metadata.Namespace != "kube-system" || !uuid.MatchString(g.Metadata.UID) || g.Metadata.ResourceVersion == "" || g.Metadata.Generation < 1 || g.Spec.GatewayClassName != "cilium" {
		return refuse()
	}
	for _, kind := range []string{"Accepted", "Programmed"} {
		found := false
		for _, c := range g.Status.Conditions {
			if c.Type == kind {
				if found || c.Status != "True" || c.ObservedGeneration != g.Metadata.Generation {
					return refuse()
				}
				found = true
			}
		}
		if !found {
			return refuse()
		}
	}
	refs := s.Metadata.OwnerReferences
	if len(refs) != 1 || refs[0].APIVersion != "gateway.networking.k8s.io/v1" || refs[0].Kind != "Gateway" || refs[0].Name != "platform" || refs[0].UID != g.Metadata.UID || !refs[0].Controller {
		return refuse()
	}
	proxy, expectedMode := "false", "VIP"
	if wantProxy {
		proxy, expectedMode = "true", "Proxy"
	}
	for key, want := range map[string]string{"load-balancer.hetzner.cloud/name": "platform-gateway", "load-balancer.hetzner.cloud/use-private-ip": "true", "load-balancer.hetzner.cloud/uses-proxyprotocol": proxy} {
		if s.Metadata.Annotations[key] != want {
			return refuse()
		}
	}
	if s.Spec.Type != "LoadBalancer" || s.Spec.ExternalTrafficPolicy != "Cluster" || len(s.Spec.IPFamilies) != 1 || s.Spec.IPFamilies[0] != "IPv4" || len(s.Spec.Ports) != 2 {
		return refuse()
	}
	ports := map[int]bool{}
	nodePorts := map[int]bool{}
	var httpNodePort, httpsNodePort int
	for _, p := range s.Spec.Ports {
		if (p.Port != 80 && p.Port != 443) || p.Protocol != "TCP" || p.NodePort < 30000 || p.NodePort > 32767 || ports[p.Port] || nodePorts[p.NodePort] {
			return refuse()
		}
		ports[p.Port] = true
		nodePorts[p.NodePort] = true
		if p.Port == 80 {
			httpNodePort = p.NodePort
		} else {
			httpsNodePort = p.NodePort
		}
	}
	if len(s.Status.LoadBalancer.Ingress) < 2 || len(s.Status.LoadBalancer.Ingress) > 3 {
		return refuse()
	}
	seen := map[netip.Addr]bool{}
	var private netip.Addr
	var public netip.Addr
	mode := ""
	for _, entry := range s.Status.LoadBalancer.Ingress {
		ip, err := netip.ParseAddr(entry.IP)
		if err != nil || !ip.IsGlobalUnicast() || ip.Is4In6() || ip.Zone() != "" || seen[ip] ||
			(entry.IPMode != expectedMode && (!recovering || (entry.IPMode != "VIP" && entry.IPMode != "Proxy"))) || (mode != "" && mode != entry.IPMode) {
			return refuse()
		}
		mode = entry.IPMode
		seen[ip] = true
		if ip.IsPrivate() {
			if !ip.Is4() || private.IsValid() {
				return refuse()
			}
			private = ip
		} else if ip.Is4() {
			if public.IsValid() {
				return refuse()
			}
			public = ip
		}
	}
	if !private.IsValid() || !public.IsValid() {
		return refuse()
	}
	return binding{CIDR: netip.PrefixFrom(private, 32).String(), ServiceUID: s.Metadata.UID, GatewayUID: g.Metadata.UID, ServiceVersion: s.Metadata.ResourceVersion, GatewayVersion: g.Metadata.ResourceVersion, PublicIPv4: public.String(), HTTPNodePort: httpNodePort, HTTPSNodePort: httpsNodePort}, nil
}
