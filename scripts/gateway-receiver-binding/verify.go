package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

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
func verifyTransportProfile(ctx context.Context, kubeContext string, profile transportProfile) error {
	if profile.Proxy && !profile.Fence {
		return errors.New("transport-profile")
	}
	before, err := collectExpected(ctx, kubeContext, profile.Proxy)
	if err != nil {
		return err
	}
	return withProviderMode(ctx, before, profile.Proxy, readProvider, func() error {
		for sample := 0; sample < 2; sample++ {
			secret, err := readNamespace(ctx, kubeContext, "flux-system", "secret", "variables-gateway-receiver")
			if err != nil || !secretMatches(secret, before) {
				return errors.New("source-secret")
			}
			c := receiverClient{context: kubeContext, command: executeReceiver}
			if err := c.policies(ctx, before); err != nil {
				return err
			}
			if err := c.receiverProfile(ctx, before, profile); err != nil {
				return err
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
		after, err := collectExpected(ctx, kubeContext, profile.Proxy)
		if err != nil || after != before {
			return errors.New("source-changed")
		}
		return nil
	})
}

func (c receiverClient) receiverProfile(ctx context.Context, b binding, profile transportProfile) error {
	if !profile.Proxy {
		return c.receiverState(ctx, b, profile.Fence)
	}
	if !profile.Fence {
		return errors.New("transport-profile")
	}
	if err := c.bindingsSelected(ctx, b); err != nil {
		return err
	}
	raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
	if err != nil {
		return err
	}
	if err = c.receiverObserved(raw, b); err != nil {
		return err
	}
	state, err := inspectReceiver(raw, b)
	if err != nil || state.Mode != receiverFencedActive {
		return errors.New("receiver-not-active")
	}
	return nil
}
