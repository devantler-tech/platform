package main

import (
	"context"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"
)

func policyMatches(raw []byte, b binding) bool {
	var live map[string]any
	if strictJSON(raw, &live) != nil {
		return false
	}
	meta, _ := live["metadata"].(map[string]any)
	if meta["name"] != "gateway-receiver" {
		return false
	}
	source, err := os.ReadFile("k8s/providers/hetzner/infrastructure/cilium/gateway-receiver/cilium-clusterwide-network-policy-gateway-receiver.yaml")
	if err != nil {
		return false
	}
	var expected map[string]any
	if yaml.Unmarshal([]byte(strings.ReplaceAll(string(source), "${gateway_receiver_source_cidr}", b.CIDR)), &expected) != nil {
		return false
	}
	actual, _ := live["spec"].(map[string]any)
	// These API defaults preserve the declared ingress-only enforcement.
	if defaults, present := actual["enableDefaultDeny"]; present {
		d, ok := defaults.(map[string]any)
		if !ok || (d["ingress"] != nil && d["ingress"] != true) {
			return false
		}
		for key, value := range d {
			if key != "ingress" && key != "egress" {
				return false
			}
			if _, ok := value.(bool); !ok {
				return false
			}
		}
		delete(actual, "enableDefaultDeny")
	}
	return reflect.DeepEqual(actual, expected["spec"])
}
func secretMatches(raw []byte, b binding) bool {
	var s struct {
		Metadata metadata
		Data     map[string][]byte
	}
	if strictJSON(raw, &s) != nil || s.Metadata.Name != "variables-gateway-receiver" || s.Metadata.Namespace != "flux-system" || !uuid.MatchString(s.Metadata.UID) || len(s.Data) != 3 {
		return false
	}
	return string(s.Data["gateway_receiver_source_cidr"]) == b.CIDR && string(s.Data["gateway_receiver_service_uid"]) == b.ServiceUID && string(s.Data["gateway_receiver_gateway_uid"]) == b.GatewayUID
}
func probePublic(ctx context.Context, ip string) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || (host != "platform.devantler.tech" && host != "vault.platform.devantler.tech") {
			return nil, errors.New("public-target")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
	}
	transport.TLSHandshakeTimeout = 5 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return probePublicWithClient(ctx, client)
}
func probePublicWithClient(ctx context.Context, client *http.Client) error {
	for _, target := range []string{"https://platform.devantler.tech/", "https://vault.platform.devantler.tech/ui/"} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		response, err := client.Do(request)
		if err != nil {
			return errors.New("public-route")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
		_ = response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 302 && response.StatusCode != 303 {
			return errors.New("public-route")
		}
	}
	return nil
}
func verifyDeployed(ctx context.Context, kubeContext string) error {
	before, err := collect(ctx, kubeContext)
	if err != nil {
		return err
	}
	for sample := 0; sample < 2; sample++ {
		secret, err := readNamespace(ctx, kubeContext, "flux-system", "secret", "variables-gateway-receiver")
		if err != nil || !secretMatches(secret, before) {
			return errors.New("source-secret")
		}
		policy, err := readNamespace(ctx, kubeContext, "", "ciliumclusterwidenetworkpolicy.cilium.io", "gateway-receiver")
		if err != nil || !policyMatches(policy, before) {
			return errors.New("source-policy")
		}
		if err = probePublic(ctx, before.PublicIPv4); err != nil {
			return err
		}
		if sample == 0 {
			select {
			case <-ctx.Done():
				return errors.New("cancelled")
			case <-time.After(time.Second):
			}
		}
	}
	after, err := collect(ctx, kubeContext)
	if err != nil || after != before {
		return errors.New("source-changed")
	}
	return nil
}
