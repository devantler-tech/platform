package main

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Called by the verified, isolated API-server fixture. Never use ambient config.
func TestNativeCorePodDeletePreconditions(t *testing.T) {
	path := os.Getenv("MAIL_NATIVE_API_CONFIG")
	if path == "" {
		t.Skip("requires the isolated native API-server fixture")
	}
	expected, err := url.Parse(os.Getenv("MAIL_NATIVE_API_SERVER"))
	if err != nil || expected.Scheme != "https" || expected.Hostname() != "127.0.0.1" || expected.Port() == "" {
		t.Fatal("fixture must use a loopback TLS server")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Clusters []struct{ Cluster struct{ Server string } }
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != expected.String() {
		t.Fatal("fixture config is not bound to the isolated server")
	}
	c := commandClient{kubeconfig: path}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pod mailPod
	var claim persistentClaim
	for _, item := range []struct {
		key string
		out any
	}{{podKey, &pod}, {claimKey, &claim}} {
		found, err := c.read(ctx, item.key, item.out)
		if err != nil || !found {
			t.Fatal("fixture object unavailable")
		}
	}
	if pod.Metadata.UID == "" || pod.Metadata.ResourceVersion == "" || claim.Metadata.UID == "" {
		t.Fatal("real API identities missing")
	}
	for _, test := range []struct{ name, uid, version string }{
		{"wrong UID", "00000000-0000-4000-8000-000000000000", pod.Metadata.ResourceVersion},
		{"wrong version", pod.Metadata.UID, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if c.deletePod(ctx, test.uid, test.version) == nil {
				t.Fatal("core API accepted conflicting deletion")
			}
			var current mailPod
			found, err := c.read(ctx, podKey, &current)
			if err != nil || !found || current.Metadata.UID != pod.Metadata.UID || current.Metadata.DeletionTimestamp != nil {
				t.Fatal("conflict altered the Pod")
			}
		})
	}
	if err = c.deletePod(ctx, pod.Metadata.UID, pod.Metadata.ResourceVersion); err != nil {
		t.Fatal("matching conditional deletion failed")
	}
	var deleted mailPod
	if found, err := c.read(ctx, podKey, &deleted); err != nil || found {
		t.Fatal("matching Pod deletion not observed")
	}
	var retained persistentClaim
	found, err := c.read(ctx, claimKey, &retained)
	if err != nil || !found || retained.Metadata.UID != claim.Metadata.UID {
		t.Fatal("claim identity was not retained")
	}
	t.Log("Native Kubernetes refused both conflicting identities, deleted the matching Pod, and retained its separate claim.")
}
