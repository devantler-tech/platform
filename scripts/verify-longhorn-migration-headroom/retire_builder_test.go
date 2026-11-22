package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetirementConfirmsProviderActionAndAbsence(t *testing.T) {
	actions := 0
	response := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	api := migrationProvider{token: "synthetic", client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Method + " " + req.URL.Path {
			case "DELETE /v1/servers/77":
				actions++
				return response(http.StatusOK, `{"action":{"id":82,"command":"delete_server","status":"running","resources":[{"id":77,"type":"server"}]}}`)
			case "GET /v1/servers/actions/82":
				return response(http.StatusOK, `{"action":{"id":82,"command":"delete_server","status":"success","resources":[{"id":77,"type":"server"}]}}`)
			case "GET /v1/servers/77":
				return response(http.StatusNotFound, `{"error":{"code":"not_found"}}`)
			case "GET /v1/primary_ips":
				return response(http.StatusOK, `{"primary_ips":[],"meta":{"pagination":{"page":1,"last_page":1,"per_page":50,"next_page":null,"total_entries":0}}}`)
			default:
				t.Fatalf("unexpected provider call: %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		}),
	}}
	b := builderServer{server: server{ID: 77}}
	b.PublicNet.IPv4.ID, b.PublicNet.IPv6.ID = 12, 13
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := api.deleteBuilder(ctx, b, "/servers/77"); err != nil || actions != 1 {
		t.Fatalf("retirement did not verify one action and absence: actions=%d err=%v", actions, err)
	}
}

func TestProviderMutationsRequireTheirOwnProductionWorkflow(t *testing.T) {
	env := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform",
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/" + builderWorkflow + ".yaml@refs/heads/main",
		"GITHUB_RUN_ATTEMPT":  "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_ACTOR": "devantler", "GITHUB_TRIGGERING_ACTOR": "devantler",
		"PROD_OPERATION": builderWorkflow, "PROD_OPERATION_CONFIRM": builderWorkflow,
	}
	getenv := func(key string) string { return env[key] }
	if !authorizedProviderMutation(getenv, builderWorkflow) || authorizedProviderMutation(getenv, indexWorkflow) {
		t.Fatal("provider operation was not bound to its own workflow")
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizedProviderMutation(getenv, builderWorkflow) {
		t.Fatal("repeated production attempt was admitted")
	}
}

func TestRetirementRequiresExactBuilderAndPrimaryIPs(t *testing.T) {
	created := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	builder := builderServer{
		server: server{
			ID: 77, Name: "hcloud-upload-image-deadbeef", Status: "running",
			Labels: map[string]string{
				"apricote.de/created-by":   "hcloud-upload-image",
				"ksail.io/cluster":         "prod",
				"ksail.io/talos-version":   "v1.13.9",
				"ksail.io/talos-schematic": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		},
		Created: created,
		Volumes: []int64{}, LoadBalancers: []int64{},
	}
	if err := json.Unmarshal([]byte(`{"private_net":[]}`), &builder.server); err != nil {
		t.Fatal(err)
	}
	builder.Locked, builder.RescueEnabled, builder.Protection.Delete = new(bool), new(bool), new(bool)
	builder.Image.Type = "system"
	builder.Image.Description = "Ubuntu 24.04"
	builder.PublicNet.FloatingIPs = []int64{}
	builder.PublicNet.IPv4 = builderAddress{ID: 81, IP: "192.0.2.10"}
	builder.PublicNet.IPv6 = builderAddress{ID: 82, IP: "2001:db8:0:1::/64"}
	witness := builder.witness()
	now := created.Add(60 * 24 * time.Hour)
	if err := builder.validate(builder.server, witness, now); err != nil {
		t.Fatalf("qualified builder was refused: %v", err)
	}
	for _, tt := range []struct {
		name   string
		update func(*builderServer)
	}{
		{"swapped primary IP", func(s *builderServer) { s.PublicNet.IPv4.ID++ }},
		{"appeared attached volume", func(s *builderServer) { s.Volumes = []int64{99} }},
		{"appeared direct load balancer", func(s *builderServer) { s.LoadBalancers = []int64{99} }},
		{"missing floating IP inventory", func(s *builderServer) { s.PublicNet.FloatingIPs = nil }},
		{"delete protected", func(s *builderServer) { flag := true; s.Protection.Delete = &flag }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := builder
			tt.update(&changed)
			if err := changed.validate(builder.server, witness, now); err == nil {
				t.Fatal("changed builder was admitted")
			}
		})
	}
	if err := builder.validate(builder.server, "different-witness", now); err == nil {
		t.Fatal("unapproved builder was admitted")
	}
}

func TestBuilderRetirementRejectsOngoingOrRecentWork(t *testing.T) {
	created := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	create := builderAction{Command: "create_server", Status: "success", Started: created}
	if err := validateBuilderActions([]builderAction{create}, created); err != nil {
		t.Fatalf("creation history was refused: %v", err)
	}
	for _, action := range []builderAction{
		{Command: "start_server", Status: "running", Started: created.Add(time.Second)},
		{Command: "reboot_server", Status: "success", Started: created.Add(3 * time.Hour)},
		{Command: "create_image", Status: "error", Started: created.Add(time.Second)},
	} {
		if err := validateBuilderActions([]builderAction{create, action}, created); err == nil {
			t.Fatalf("unsafe action was admitted: %+v", action)
		}
	}
}
