package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const maxInventoryBytes = 2 * 1024 * 1024
const maxHeadroomServers = 9
const maxAccountServers = 10

var serverFields = map[string][]string{
	"":                              {"servers", "meta"},
	"meta":                          {"pagination"},
	"meta.pagination":               {"page", "per_page", "last_page", "next_page", "total_entries"},
	"servers[]":                     {"id", "name", "status", "labels", "private_net", "server_type", "datacenter"},
	"servers[].private_net[]":       {"network", "ip"},
	"servers[].server_type":         {"name"},
	"servers[].datacenter":          {"location"},
	"servers[].datacenter.location": {"name"},
}

var nodeFields = map[string][]string{
	"":                            {"apiVersion", "kind", "metadata", "items"},
	"metadata":                    {"resourceVersion", "continue", "remainingItemCount"},
	"items[]":                     {"metadata", "spec", "status"},
	"items[].metadata":            {"name", "uid", "labels", "deletionTimestamp"},
	"items[].spec":                {"providerID", "unschedulable"},
	"items[].status":              {"conditions", "addresses"},
	"items[].status.conditions[]": {"type", "status"},
	"items[].status.addresses[]":  {"type", "address"},
}

func collectServers(ctx context.Context, client *http.Client, token string) ([]server, error) {
	return collectServerInventory(ctx, client, token, maxHeadroomServers)
}

func collectServerInventory(ctx context.Context, client *http.Client, token string, limit int) ([]server, error) {
	if token == "" {
		return nil, errors.New("provider read credential is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/servers?per_page=50&page=1", nil)
	if err != nil {
		return nil, errors.New("provider inventory request failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider inventory is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxInventoryBytes+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || len(raw) > maxInventoryBytes {
		return nil, errors.New("provider inventory exceeded its read bound")
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("provider inventory request was refused")
	}
	var page struct {
		Servers []server `json:"servers"`
		Meta    struct {
			Pagination struct {
				Page     *int            `json:"page"`
				PerPage  *int            `json:"per_page"`
				LastPage *int            `json:"last_page"`
				NextPage json.RawMessage `json:"next_page"`
				Total    *int            `json:"total_entries"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	if strictjson.Validate(raw, serverFields) != nil || json.Unmarshal(raw, &page) != nil {
		return nil, errors.New("provider inventory is malformed")
	}
	p := page.Meta.Pagination
	// The account limit is ten, below this fifty-entry request. Anything that
	// needs another page cannot qualify for a free slot; never treat it as empty.
	if page.Servers == nil || len(page.Servers) > limit || p.Page == nil || *p.Page != 1 || p.PerPage == nil || *p.PerPage != 50 || p.LastPage == nil || *p.LastPage != 1 || !bytes.Equal(bytes.TrimSpace(p.NextPage), []byte("null")) || p.Total == nil || *p.Total != len(page.Servers) {
		// Report only the bounded count and guard outcomes, never provider
		// identities, response values, credentials or a raw payload.
		return nil, fmt.Errorf("provider inventory is incomplete or exceeds migration capacity: servers=%d servers_present=%t within_capacity=%t page_valid=%t per_page_valid=%t last_page_valid=%t next_page_terminal=%t total_present=%t total_matches=%t",
			len(page.Servers), page.Servers != nil, len(page.Servers) <= limit,
			p.Page != nil && *p.Page == 1, p.PerPage != nil && *p.PerPage == 50,
			p.LastPage != nil && *p.LastPage == 1, bytes.Equal(bytes.TrimSpace(p.NextPage), []byte("null")),
			p.Total != nil, p.Total != nil && *p.Total == len(page.Servers))
	}
	return page.Servers, nil
}

func decodeNodes(raw []byte) ([]node, error) {
	var list struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
			Continue        string `json:"continue"`
			Remaining       *int64 `json:"remainingItemCount"`
		} `json:"metadata"`
		Items []node `json:"items"`
	}
	if len(raw) > maxInventoryBytes || strictjson.Validate(raw, nodeFields) != nil || json.Unmarshal(raw, &list) != nil || list.APIVersion != "v1" || list.Kind != "NodeList" || list.Metadata.ResourceVersion == "" || list.Metadata.Continue != "" || (list.Metadata.Remaining != nil && *list.Metadata.Remaining != 0) || list.Items == nil || len(list.Items) > 9 {
		return nil, errors.New("native node inventory is incomplete")
	}
	return list.Items, nil
}
