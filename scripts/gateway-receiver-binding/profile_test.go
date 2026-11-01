package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTransportSourceRequiresAnExplicitPairedProfile(t *testing.T) {
	for _, tc := range []struct {
		name, provider, receiver string
		fence, valid, proxy      bool
	}{
		{"disabled", "false", "false", false, true, false},
		{"fenced-disabled", "false", "false", true, true, false},
		{"fenced-active", "true", "true", true, true, true},
		{"receiver-ahead", "false", "true", true, false, false},
		{"provider-ahead", "true", "false", true, false, false},
		{"unfenced-active", "true", "true", false, false, false},
		{"missing-provider", "null", "false", true, false, false},
		{"missing-receiver", "false", "null", true, false, false},
		{"wrong-type-receiver", "false", "'true'", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := []string{mutatingPolicy, validatingPolicy}
			if tc.fence {
				kinds = append(kinds, mutatingBinding, validBinding)
			}
			infra := string(renderedResources(kinds...)) + "---\napiVersion: gateway.networking.k8s.io/v1\nkind: Gateway\nmetadata:\n  name: platform\n  namespace: kube-system\nspec:\n  infrastructure:\n    annotations:\n      load-balancer.hetzner.cloud/uses-proxyprotocol: "
			if tc.provider == "null" {
				infra += "null\n"
			} else {
				infra += "'" + tc.provider + "'\n"
			}
			controllers := "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: cilium\n  namespace: kube-system\nspec:\n  values:\n    gatewayAPI:\n      enableProxyProtocol: " + tc.receiver + "\n"
			got, err := sourceTransportProfile([]byte(infra), []byte(controllers))
			if (err == nil) != tc.valid || (tc.valid && got != (transportProfile{Fence: tc.fence, Proxy: tc.proxy})) {
				t.Fatal("source selection admitted an ambiguous or mismatched transport profile")
			}
			if tc.valid {
				_, err = sourceTransportProfile([]byte(infra+"---\n"+infra), []byte(controllers))
				if err == nil {
					t.Fatal("duplicate gateway source was accepted")
				}
				_, err = sourceTransportProfile([]byte(infra), []byte(controllers+"---\n"+controllers))
				if err == nil {
					t.Fatal("duplicate receiver source was accepted")
				}
				_, err = sourceTransportProfile([]byte(strings.Replace(infra, "namespace: kube-system", "namespace: other", 1)), []byte(controllers))
				if err == nil {
					t.Fatal("foreign gateway source was accepted")
				}
			}
		})
	}
}

func TestCurrentNativeRendersSelectTheFencedDisabledTransport(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := renderTransportProfile(ctx)
	if err != nil || got != (transportProfile{Fence: true, Proxy: false}) {
		t.Fatalf("current native Kustomize renders do not establish the expected paired disabled baseline: %v", err)
	}
}
