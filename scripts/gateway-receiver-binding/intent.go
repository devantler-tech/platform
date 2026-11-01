package main

import "errors"

type transportIntent struct{ Proxy, StatusProxy, Ready bool }

func deriveObserved(service, gateway []byte) (binding, transportIntent, error) {
	var observed serviceObject
	if strictJSON(service, &observed) != nil {
		return binding{}, transportIntent{}, errors.New("unbound-provider")
	}
	want := observed.Metadata.Annotations["load-balancer.hetzner.cloud/uses-proxyprotocol"]
	if want != "false" && want != "true" {
		return binding{}, transportIntent{}, errors.New("unbound-provider")
	}
	b, err := deriveExpectedTransport(service, gateway, want == "true", true)
	if err != nil {
		return binding{}, transportIntent{}, err
	}
	var actual gatewayObject
	if strictJSON(gateway, &actual) != nil {
		return binding{}, transportIntent{}, errors.New("unbound-provider")
	}
	ready, err := gatewayReadiness(actual)
	if err != nil {
		return binding{}, transportIntent{}, err
	}
	return b, transportIntent{Proxy: want == "true", StatusProxy: observed.Status.LoadBalancer.Ingress[0].IPMode == "Proxy", Ready: ready}, nil
}
