package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"time"
)

const builderWorkflow = "retire-abandoned-longhorn-builder"

// Only the approved server ID, name, creation time and both Primary IP IDs
// can produce this witness. No dispatch input can select a different server.
const approvedBuilder = "fc6fbe6409ded992e52ad0069cae07ad3b73de3e70838a257d35136fd9c25a66"

var builderName = regexp.MustCompile(`^hcloud-upload-image-[0-9a-f]{8}$`)

// Hetzner labels cap values at 63 characters, so the uploader truncates the schematic digest.
var builderSchematic = regexp.MustCompile(`^[0-9a-f]{63}$`)

type builderAddress struct {
	ID int64  `json:"id"`
	IP string `json:"ip"`
}

type builderServer struct {
	server
	Created       time.Time `json:"created"`
	Locked        *bool     `json:"locked"`
	RescueEnabled *bool     `json:"rescue_enabled"`
	Volumes       []int64   `json:"volumes"`
	LoadBalancers []int64   `json:"load_balancers"`
	BackupWindow  *string   `json:"backup_window"`
	Protection    struct {
		Delete *bool `json:"delete"`
	} `json:"protection"`
	Image struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"image"`
	PublicNet struct {
		IPv4        builderAddress `json:"ipv4"`
		IPv6        builderAddress `json:"ipv6"`
		FloatingIPs []int64        `json:"floating_ips"`
	} `json:"public_net"`
}

func (b builderServer) witness() string {
	data := fmt.Sprintf("%d\n%s\n%s\n%d\n%d\n", b.ID, b.Name, b.Created.UTC().Format(time.RFC3339), b.PublicNet.IPv4.ID, b.PublicNet.IPv6.ID)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}

func (b builderServer) validate(candidate server, expected string, now time.Time) error {
	if !reflect.DeepEqual(b.server, candidate) || b.witness() != expected {
		return errors.New("builder differs from the exact approved inventory witness")
	}
	if b.ID <= 0 || !builderName.MatchString(b.Name) || b.Status != "running" ||
		b.Labels["apricote.de/created-by"] != "hcloud-upload-image" || b.Labels["ksail.io/cluster"] != "prod" ||
		!builderSchematic.MatchString(b.Labels["ksail.io/talos-schematic"]) || b.Labels["ksail.io/talos-version"] == "" ||
		b.Labels["ksail.cluster.name"] != "" || b.Labels["ksail.owned"] != "" || b.Labels["hcloud/node-group"] != "" ||
		b.Image.Type != "system" || b.Image.Description != "Ubuntu 24.04" ||
		b.Created.IsZero() || now.Sub(b.Created) < 30*24*time.Hour {
		return errors.New("server is not the abandoned production image builder")
	}
	if b.PrivateNet == nil || len(b.PrivateNet) != 0 || b.Volumes == nil || len(b.Volumes) != 0 ||
		b.LoadBalancers == nil || len(b.LoadBalancers) != 0 || b.PublicNet.FloatingIPs == nil || len(b.PublicNet.FloatingIPs) != 0 ||
		b.Locked == nil || *b.Locked || b.RescueEnabled == nil || *b.RescueEnabled ||
		b.Protection.Delete == nil || *b.Protection.Delete || b.BackupWindow != nil ||
		b.PublicNet.IPv4.ID <= 0 || b.PublicNet.IPv6.ID <= 0 || b.PublicNet.IPv4.ID == b.PublicNet.IPv6.ID ||
		b.PublicNet.IPv4.IP == "" || b.PublicNet.IPv6.IP == "" {
		return errors.New("builder has attachments, protection, enabled backups or an incomplete network record")
	}
	return nil
}

type builderAction struct {
	Command string    `json:"command"`
	Status  string    `json:"status"`
	Started time.Time `json:"started"`
}

type builderDeleteAction struct {
	Action struct {
		builderAction
		ID        int64 `json:"id"`
		Resources []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"resources"`
	} `json:"action"`
}

func (d builderDeleteAction) matches(serverID int64) bool {
	if d.Action.ID <= 0 || d.Action.Command != "delete_server" {
		return false
	}
	found := false
	for _, resource := range d.Action.Resources {
		if resource.Type != "server" {
			continue
		}
		if found || resource.ID != serverID {
			return false
		}
		found = true
	}
	return found
}

func validateBuilderActions(actions []builderAction, created time.Time) error {
	createdCount := 0
	for _, action := range actions {
		if action.Status != "success" || action.Started.Before(created) || action.Started.After(created.Add(2*time.Minute)) {
			return errors.New("builder has incomplete or later provider activity")
		}
		switch action.Command {
		case "create_server":
			createdCount++
		case "start_server", "enable_rescue":
		default:
			return errors.New("builder has activity outside its original bootstrap")
		}
	}
	if createdCount != 1 {
		return errors.New("builder creation history is missing or ambiguous")
	}
	return nil
}

type builderPrimaryIP struct {
	ID         int64  `json:"id"`
	AssigneeID int64  `json:"assignee_id"`
	Type       string `json:"type"`
	AutoDelete *bool  `json:"auto_delete"`
	Protection struct {
		Delete *bool `json:"delete"`
	} `json:"protection"`
}

type builderLoadBalancer struct {
	Targets []struct {
		Type   string              `json:"type"`
		Server struct{ ID int64 }  `json:"server"`
		IP     struct{ IP string } `json:"ip"`
	} `json:"targets"`
}

type builderImage struct {
	BoundTo     int64              `json:"bound_to"`
	CreatedFrom struct{ ID int64 } `json:"created_from"`
}

type migrationProvider struct {
	client *http.Client
	token  string
}

func (a migrationProvider) request(ctx context.Context, method, path string) ([]byte, int, error) {
	return a.requestJSON(ctx, method, path, nil)
}

func (a migrationProvider) requestJSON(ctx context.Context, method, path string, payload []byte) ([]byte, int, error) {
	if a.token == "" {
		return nil, 0, errors.New("provider credential is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.hetzner.cloud/v1"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, errors.New("cannot prepare bounded provider request")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("provider request failed")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxInventoryBytes+1))
	closeErr := res.Body.Close()
	if err != nil || closeErr != nil || len(raw) > maxInventoryBytes {
		return nil, 0, errors.New("provider response exceeded its read bound")
	}
	return raw, res.StatusCode, nil
}

// Every list used to qualify retirement must fit a complete bounded page.
func readBuilderList[T any](ctx context.Context, a migrationProvider, path, field string) ([]T, error) {
	raw, status, err := a.request(ctx, http.MethodGet, path)
	if err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("provider %s inventory is unavailable", field)
	}
	var response map[string]json.RawMessage
	// Use wire names explicitly; a missing total or next-page marker is a refusal.
	var envelope struct {
		Pagination struct {
			Page    int             `json:"page"`
			Last    int             `json:"last_page"`
			PerPage int             `json:"per_page"`
			Total   *int            `json:"total_entries"`
			Next    json.RawMessage `json:"next_page"`
		} `json:"pagination"`
	}
	var items []T
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(response["meta"], &envelope) != nil ||
		json.Unmarshal(response[field], &items) != nil || items == nil {
		return nil, fmt.Errorf("provider %s inventory is malformed", field)
	}
	p := envelope.Pagination
	if p.Page != 1 || p.Last != 1 || p.PerPage != 50 || p.Total == nil || *p.Total != len(items) || !bytes.Equal(bytes.TrimSpace(p.Next), []byte("null")) {
		return nil, fmt.Errorf("provider %s inventory is incomplete", field)
	}
	return items, nil
}

func (a migrationProvider) readBuilder(ctx context.Context, path string) (builderServer, error) {
	raw, status, err := a.request(ctx, http.MethodGet, path)
	var result struct{ Server builderServer }
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &result) != nil {
		return builderServer{}, errors.New("builder detail is unavailable or malformed")
	}
	return result.Server, nil
}

func (a migrationProvider) verifyBuilderDependencies(ctx context.Context, b builderServer, path string) error {
	actions, err := readBuilderList[builderAction](ctx, a, path+"/actions?per_page=50&page=1", "actions")
	if err != nil {
		return err
	}
	if err := validateBuilderActions(actions, b.Created); err != nil {
		return err
	}
	images, err := readBuilderList[builderImage](ctx, a, "/images?type=snapshot&type=backup&per_page=50&page=1", "images")
	if err != nil {
		return err
	}
	for _, image := range images {
		if image.BoundTo == b.ID || image.CreatedFrom.ID == b.ID {
			return errors.New("builder has a snapshot or a provider backup")
		}
	}
	lbs, err := readBuilderList[builderLoadBalancer](ctx, a, "/load_balancers?per_page=50&page=1", "load_balancers")
	if err != nil {
		return err
	}
	ipv4, err4 := netip.ParseAddr(b.PublicNet.IPv4.IP)
	ipv6, err6 := netip.ParsePrefix(b.PublicNet.IPv6.IP)
	if err4 != nil || err6 != nil || !ipv4.Is4() || !ipv6.Addr().Is6() {
		return errors.New("builder Primary IP addresses are incomplete")
	}
	for _, lb := range lbs {
		if lb.Targets == nil {
			return errors.New("load balancer targets are incomplete")
		}
		for _, target := range lb.Targets {
			switch target.Type {
			case "server":
				if target.Server.ID <= 0 || target.Server.ID == b.ID {
					return errors.New("builder may be a load balancer server target")
				}
			case "ip":
				address, err := netip.ParseAddr(target.IP.IP)
				if err != nil || address == ipv4 || ipv6.Contains(address) {
					return errors.New("builder may be a load balancer IP target")
				}
			default:
				return errors.New("load balancer has targets whose association is unverified")
			}
		}
	}
	return a.verifyPrimaryIPs(ctx, b, false)
}

func (a migrationProvider) verifyPrimaryIPs(ctx context.Context, b builderServer, absent bool) error {
	ips, err := readBuilderList[builderPrimaryIP](ctx, a, "/primary_ips?per_page=50&page=1", "primary_ips")
	if err != nil {
		return err
	}
	matches := map[string]int{}
	for _, ip := range ips {
		if ip.AssigneeID != b.ID && ip.ID != b.PublicNet.IPv4.ID && ip.ID != b.PublicNet.IPv6.ID {
			continue
		}
		wantedID := (ip.Type == "ipv4" && ip.ID == b.PublicNet.IPv4.ID) || (ip.Type == "ipv6" && ip.ID == b.PublicNet.IPv6.ID)
		if absent || ip.AssigneeID != b.ID || ip.AutoDelete == nil || !*ip.AutoDelete || ip.Protection.Delete == nil || *ip.Protection.Delete || !wantedID {
			return errors.New("builder Primary IP ownership, auto-delete or final absence is unverified")
		}
		matches[ip.Type]++
	}
	if !absent && (matches["ipv4"] != 1 || matches["ipv6"] != 1) {
		return errors.New("the two approved Primary IPs are missing or duplicated")
	}
	return nil
}

func authorizedProviderMutation(getenv func(string) string, workflow string) bool {
	return getenv("GITHUB_ACTIONS") == "true" && getenv("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" && getenv("GITHUB_REF") == "refs/heads/main" &&
		getenv("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/"+workflow+".yaml@refs/heads/main" &&
		getenv("GITHUB_RUN_ATTEMPT") == "1" && getenv("RUNNER_ENVIRONMENT") == "github-hosted" &&
		getenv("GITHUB_ACTOR") == "devantler" && getenv("GITHUB_TRIGGERING_ACTOR") == "devantler" &&
		getenv("PROD_OPERATION") == workflow && getenv("PROD_OPERATION_CONFIRM") == workflow
}

func runBuilderRetirement(ctx context.Context, a migrationProvider, readNodes func(context.Context) ([]byte, error), apply bool) error {
	before, nodes, err := readStableAccount(ctx, a.client, a.token, readNodes)
	if err != nil {
		return err
	}
	_, candidate, err := joinAccountServers(before, nodes)
	if err != nil || candidate == nil {
		return errors.New("the approved builder did not uniquely join outside the native cluster")
	}
	// Verify the joined nodes as well as the unmatched account row.
	if _, err := describeAccountOwnership(before, nodes); err != nil {
		return err
	}
	path := fmt.Sprintf("/servers/%d", candidate.ID)
	builder, err := a.readBuilder(ctx, path)
	if err != nil {
		return err
	}
	if err := builder.validate(*candidate, approvedBuilder, time.Now()); err != nil {
		return err
	}
	if err := a.verifyBuilderDependencies(ctx, builder, path); err != nil {
		return err
	}
	last, currentNodes, err := readStableAccount(ctx, a.client, a.token, readNodes)
	if err != nil || !reflect.DeepEqual(before, last) || !reflect.DeepEqual(nodes, currentNodes) {
		return errors.New("provider or cluster inventory changed during retirement checks")
	}
	current, err := a.readBuilder(ctx, path)
	if err != nil || !reflect.DeepEqual(builder, current) {
		return errors.New("builder changed after the attachment checks")
	}
	if !apply {
		fmt.Println("BUILDER_RETIREMENT=VERIFIED_PLAN")
		return nil
	}
	if err := a.deleteBuilder(ctx, builder, path); err != nil {
		return err
	}
	after, finalNodes, err := readStableAccount(ctx, a.client, a.token, readNodes)
	expected := slices.DeleteFunc(before, func(s server) bool { return s.ID == builder.ID })
	if err != nil || !reflect.DeepEqual(nodes, finalNodes) || !reflect.DeepEqual(after, expected) {
		return errors.New("remaining provider and cluster rosters could not be verified after deletion")
	}
	fmt.Println("BUILDER_RETIREMENT=VERIFIED")
	return nil
}

func (a migrationProvider) deleteBuilder(ctx context.Context, b builderServer, path string) error {
	raw, status, err := a.request(ctx, http.MethodDelete, path)
	var accepted builderDeleteAction
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &accepted) != nil || !accepted.matches(b.ID) {
		return errors.New("builder delete response is unverified; read provider state before any retry")
	}
	actionPath := fmt.Sprintf("/servers/actions/%d", accepted.Action.ID)
	// Hetzner removal is asynchronous. Poll observations only; never repeat DELETE.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		raw, status, err := a.request(ctx, http.MethodGet, actionPath)
		var progress builderDeleteAction
		if err != nil || status != http.StatusOK || json.Unmarshal(raw, &progress) != nil ||
			progress.Action.ID != accepted.Action.ID || !progress.matches(b.ID) {
			return errors.New("builder deletion action could not be verified")
		}
		switch progress.Action.Status {
		case "success":
			raw, status, err := a.request(ctx, http.MethodGet, path)
			var missing struct{ Error struct{ Code string } }
			if err == nil && status == http.StatusNotFound && json.Unmarshal(raw, &missing) == nil && missing.Error.Code == "not_found" &&
				a.verifyPrimaryIPs(ctx, b, true) == nil {
				return nil
			}
		case "running":
		default:
			return errors.New("builder deletion action did not report successful removal")
		}
		select {
		case <-ctx.Done():
			return errors.New("builder and both Primary IP removals could not be verified")
		case <-ticker.C:
		}
	}
}
