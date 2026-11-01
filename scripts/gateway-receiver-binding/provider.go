package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type providerIdentity struct{ ID, NetworkID int64 }

var errProviderMode = errors.New("provider-mode")

func readProvider(ctx context.Context, b binding, wantProxy bool) (providerIdentity, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	return observeProvider(ctx, &http.Client{Transport: transport}, os.Getenv("HCLOUD_TOKEN"), b, wantProxy)
}

type providerBalancer struct {
	ID        int64
	Name      string
	Labels    map[string]string
	PublicNet struct {
		Enabled *bool
		IPv4    struct {
			IP      string
			Blocked *bool
		}
	} `json:"public_net"`
	PrivateNet []struct {
		Network int64
		IP      string
	} `json:"private_net"`
	Services []struct {
		Protocol        string
		ListenPort      int   `json:"listen_port"`
		DestinationPort int   `json:"destination_port"`
		ProxyProtocol   *bool `json:"proxyprotocol"`
	}
	Targets []struct {
		Type          string
		Server        *struct{ ID int64 }
		UsePrivateIP  *bool `json:"use_private_ip"`
		IP            any
		LabelSelector any `json:"label_selector"`
		Targets       []json.RawMessage
	}
}

// providerMatches binds effective provider configuration to the already verified
// Kubernetes identity. An absent boolean cannot prove that a mode is disabled.
func providerMatches(lb providerBalancer, b binding, wantProxy bool) (providerIdentity, error) {
	bad := func() (providerIdentity, error) { return providerIdentity{}, errors.New("provider-state") }
	if lb.ID <= 0 || lb.Name != "platform-gateway" || lb.Labels["hcloud-ccm/service-uid"] != b.ServiceUID || lb.PublicNet.Enabled == nil || !*lb.PublicNet.Enabled || lb.PublicNet.IPv4.IP != b.PublicIPv4 || lb.PublicNet.IPv4.Blocked == nil || *lb.PublicNet.IPv4.Blocked || len(lb.PrivateNet) != 1 || len(lb.Services) != 2 || len(lb.Targets) == 0 {
		return bad()
	}
	private, err := netip.ParsePrefix(b.CIDR)
	if err != nil || private.Bits() != 32 || !private.Addr().Is4() || !private.Addr().IsPrivate() || lb.PrivateNet[0].IP != private.Addr().String() || lb.PrivateNet[0].Network <= 0 {
		return bad()
	}
	ports := map[int]bool{}
	modeMismatch := false
	for _, service := range lb.Services {
		destination := b.HTTPNodePort
		if service.ListenPort == 443 {
			destination = b.HTTPSNodePort
		}
		if (service.ListenPort != 80 && service.ListenPort != 443) || ports[service.ListenPort] || service.Protocol != "tcp" || service.DestinationPort != destination || service.ProxyProtocol == nil {
			return bad()
		}
		if *service.ProxyProtocol != wantProxy {
			modeMismatch = true
		}
		ports[service.ListenPort] = true
	}
	servers := map[int64]bool{}
	for _, target := range lb.Targets {
		if target.Type != "server" || target.Server == nil || target.Server.ID <= 0 || servers[target.Server.ID] || target.UsePrivateIP == nil || !*target.UsePrivateIP || target.IP != nil || target.LabelSelector != nil || len(target.Targets) != 0 {
			return bad()
		}
		servers[target.Server.ID] = true
	}
	if modeMismatch {
		return providerIdentity{ID: lb.ID, NetworkID: lb.PrivateNet[0].Network}, errProviderMode
	}
	return providerIdentity{ID: lb.ID, NetworkID: lb.PrivateNet[0].Network}, nil
}

// Provider schema keys use lowercase ASCII, digits and underscores; labels are
// the sole arbitrary-key map. An allowlist also rejects Unicode simple-fold
// aliases accepted by Go's struct decoder, rather than enumerating exceptions.
// Validate that spelling before Go's case-insensitive struct decoder can allow
// an alias to stand in for, or override, an authoritative mode field.
func providerKeys(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		for key, item := range value {
			invalid := strings.IndexFunc(key, func(r rune) bool {
				return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
			})
			if key == "" || invalid != -1 || (key != "labels" && !providerKeys(item)) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !providerKeys(item) {
				return false
			}
		}
	}
	return true
}

func providerJSON(raw []byte, target any) error {
	var keys any
	if strictJSON(raw, &keys) != nil || !providerKeys(keys) || json.Unmarshal(raw, target) != nil {
		return errors.New("provider-document")
	}
	return nil
}

// observeProvider performs two bounded GETs to a fixed API origin. Discovery is
// scoped by Service UID, then re-read by the returned resource ID. It makes no
// provider changes and returns only categorical errors, never response bodies.
func observeProvider(ctx context.Context, client *http.Client, token string, b binding, wantProxy bool) (providerIdentity, error) {
	if token == "" || strings.ContainsAny(token, " \r\n\t") || client == nil || !uuid.MatchString(b.ServiceUID) || b.HTTPNodePort < 30000 || b.HTTPNodePort > 32767 || b.HTTPSNodePort < 30000 || b.HTTPSNodePort > 32767 || b.HTTPNodePort == b.HTTPSNodePort {
		return providerIdentity{}, errors.New("provider-input")
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 20*time.Second {
		bounded.Timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	get := func(path string, query url.Values, target any) error {
		u := url.URL{Scheme: "https", Host: "api.hetzner.cloud", Path: path, RawQuery: query.Encode()}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return errors.New("provider-request")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := bounded.Do(request)
		if err != nil {
			return errors.New("provider-transport")
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (512<<10)+1))
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("provider-response")
		}
		if readErr != nil || closeErr != nil || providerJSON(raw, target) != nil {
			return errors.New("provider-document")
		}
		return nil
	}
	var response struct {
		Balancers []providerBalancer `json:"load_balancers"`
		Meta      struct {
			Pagination struct {
				Page         *int            `json:"page"`
				PerPage      *int            `json:"per_page"`
				LastPage     *int            `json:"last_page"`
				TotalEntries *int            `json:"total_entries"`
				NextPage     json.RawMessage `json:"next_page"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	if err := get("/v1/load_balancers", url.Values{"label_selector": {"hcloud-ccm/service-uid=" + b.ServiceUID}, "page": {"1"}, "per_page": {"50"}}, &response); err != nil {
		return providerIdentity{}, err
	}
	p := response.Meta.Pagination
	// A unique filtered result fits one page. Any indication of another page,
	// omitted pagination, or a different count is unverified, never truncated.
	if p.Page == nil || *p.Page != 1 || p.PerPage == nil || *p.PerPage != 50 || p.LastPage == nil || *p.LastPage != 1 || p.TotalEntries == nil || *p.TotalEntries != 1 || !bytes.Equal(bytes.TrimSpace(p.NextPage), []byte("null")) || len(response.Balancers) != 1 {
		return providerIdentity{}, errors.New("provider-inventory")
	}
	before, beforeErr := providerMatches(response.Balancers[0], b, wantProxy)
	if beforeErr != nil && !errors.Is(beforeErr, errProviderMode) {
		return providerIdentity{}, beforeErr
	}
	var detail struct {
		Balancer providerBalancer `json:"load_balancer"`
	}
	if err := get("/v1/load_balancers/"+strconv.FormatInt(before.ID, 10), nil, &detail); err != nil {
		return providerIdentity{}, err
	}
	after, afterErr := providerMatches(detail.Balancer, b, wantProxy)
	if (afterErr != nil && !errors.Is(afterErr, errProviderMode)) || before != after {
		return providerIdentity{}, errors.New("provider-changed")
	}
	if beforeErr != nil || afterErr != nil {
		return after, errProviderMode
	}
	return after, nil
}
