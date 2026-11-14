package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkProviderReplyLimit = 128 << 10

// Verify the independently owned Hetzner address before any production API
// credential can be used against an address advertised by the Kubernetes Node.
func verifyBenchmarkProviderEndpoints(ctx context.Context, token string, roster []benchmarkAPIServer) error {
	unverified := errors.New("direct serving API endpoints do not match the provider")
	if token == "" || len(roster) != benchmarkControlPlaneCount {
		return unverified
	}
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, server := range roster {
		if !benchmarkControlPlaneProvider.MatchString(server.ProviderID) {
			return unverified
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/servers/"+strings.TrimPrefix(server.ProviderID, "hcloud://"), nil)
		if err != nil {
			return unverified
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := client.Do(req)
		if err != nil {
			return unverified
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, benchmarkProviderReplyLimit+1))
		closeErr := res.Body.Close()
		if res.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || !benchmarkProviderServerMatches(raw, server) {
			return unverified
		}
	}
	return nil
}

func benchmarkProviderServerMatches(raw []byte, endpoint benchmarkAPIServer) bool {
	if len(raw) > benchmarkProviderReplyLimit || strictjson.Validate(raw, map[string][]string{
		"":                       {"server"},
		"server":                 {"id", "name", "status", "labels", "public_net", "private_net"},
		"server.public_net":      {"ipv4"},
		"server.public_net.ipv4": {"ip", "blocked"},
		"server.private_net[]":   {"ip"},
	}) != nil {
		return false
	}
	var response struct {
		Server struct {
			ID     int64             `json:"id"`
			Name   string            `json:"name"`
			Status string            `json:"status"`
			Labels map[string]string `json:"labels"`
			Public struct {
				IPv4 struct {
					IP      string `json:"ip"`
					Blocked bool   `json:"blocked"`
				} `json:"ipv4"`
			} `json:"public_net"`
			Private []struct {
				IP string `json:"ip"`
			} `json:"private_net"`
		} `json:"server"`
	}
	if json.Unmarshal(raw, &response) != nil || !endpoint.InternalIP.Is4() || !endpoint.ExternalIP.Is4() ||
		!strings.HasPrefix(endpoint.Node, "prod-control-plane-") || !benchmarkControlPlaneProvider.MatchString(endpoint.ProviderID) {
		return false
	}
	s := response.Server
	return "hcloud://"+strconv.FormatInt(s.ID, 10) == endpoint.ProviderID && s.Name == endpoint.Node &&
		s.Status == "running" && s.Labels["ksail.owned"] == "true" && s.Labels["ksail.cluster.name"] == "prod" &&
		s.Labels["ksail.node.type"] == "control-plane" && s.Labels["ksail.node.index"] == strings.TrimPrefix(endpoint.Node, "prod-control-plane-") &&
		s.Labels["hcloud/node-group"] == "" && !s.Public.IPv4.Blocked && s.Public.IPv4.IP == endpoint.ExternalIP.String() &&
		len(s.Private) == 1 && s.Private[0].IP == endpoint.InternalIP.String()
}
