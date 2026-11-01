package gatewayreceiver_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// This public multi-platform image is the default in Cilium's reviewed 1.20.4
// chart. Audit the image and HCM defaults again when that chart changes.
const nativeEnvoyImage = "quay.io/cilium/cilium-envoy@sha256:547ab45f6092f681d9f54f3be9fa8fb1e705c9c5dc2df388efe3a52aa6965629"

func verifyNativeEnvoyProfile(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "kustomize", "../../../k8s/providers/hetzner/infrastructure/controllers")
	rendered, err := cmd.Output()
	if err != nil {
		t.Fatal("native proxy proof requires the rendered production controller profile")
	}
	var found bool
	for _, document := range strings.Split(string(rendered), "\n---\n") {
		var object struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
			Spec     struct {
				Chart  struct{ Spec struct{ Version string } }
				Values struct {
					GatewayAPI struct {
						UseRemoteAddress  *bool
						XffNumTrustedHops *int
					}
					Envoy struct{ Image map[string]any }
				}
			}
		}
		if yaml.Unmarshal([]byte(document), &object) != nil {
			t.Fatal("native proxy profile render is invalid")
		}
		if object.Kind != "HelmRelease" || object.Metadata.Name != "cilium" {
			continue
		}
		if found || object.Metadata.Namespace != "kube-system" || object.Spec.Chart.Spec.Version != "1.20.4" {
			t.Fatal("audit the native proxy image for the rendered Cilium chart")
		}
		found = true
		gateway := object.Spec.Values.GatewayAPI
		if gateway.UseRemoteAddress != nil && !*gateway.UseRemoteAddress || gateway.XffNumTrustedHops != nil && *gateway.XffNumTrustedHops != 0 {
			t.Fatal("audit native proxy proof for the rendered forwarded-address settings")
		}
		if len(object.Spec.Values.Envoy.Image) != 0 {
			t.Fatal("audit native proxy proof for the rendered Envoy image override")
		}
	}
	if !found {
		t.Fatal("native proxy proof did not bind the rendered Cilium release")
	}
}

// A real connection peer and a PROXY-advertised address must stay distinct.
// The same test deliberately substitutes the logical-address matcher: its
// forged-header failure proves that the negative cases reach that boundary.
func TestNativeEnvoyPhysicalPeerBoundary(t *testing.T) {
	if os.Getenv("GATEWAY_RECEIVER_NATIVE_TEST") != "1" {
		t.Skip("native container proof is enabled explicitly by CI")
	}
	verifyNativeEnvoyProfile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	work := t.TempDir()
	if os.Chmod(work, 0755) != nil {
		t.Fatal("native fixture workspace")
	}
	config := filepath.Join(work, "admitted.json")
	if os.WriteFile(config, nativeEnvoyConfiguration(t), 0644) != nil {
		t.Fatal("native admitted listener")
	}
	binary := filepath.Join(work, "proof")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, "./testdata/native-envoy-proof.go")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") && !strings.HasPrefix(entry, "CGO_ENABLED=") {
			build.Env = append(build.Env, entry)
		}
	}
	build.Env = append(build.Env, "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("native probe build: %v: %s", err, output)
	}
	for _, mutant := range []bool{false, true} {
		entropy := make([]byte, 8)
		if _, err := rand.Read(entropy); err != nil {
			t.Fatal(err)
		}
		name := "gateway-peer-proof-" + hex.EncodeToString(entropy)
		cleanup := func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			_ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", name).Run()
			remaining, err := exec.CommandContext(cleanupCtx, "docker", "ps", "--all", "--format", "{{.Names}}").Output()
			present := false
			for _, candidate := range strings.Split(strings.TrimSpace(string(remaining)), "\n") {
				present = present || candidate == name
			}
			if err != nil || present {
				t.Error("native proxy fixture cleanup could not be verified")
			}
		}
		t.Cleanup(cleanup)
		args := []string{"run", "--rm", "--name", name, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532", "--cpus", "1", "--memory", "256m", "--pids-limit", "64", "--mount", "type=bind,src=" + work + ",dst=/fixture,readonly", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,mode=1777", nativeEnvoyImage, "/fixture/proof", "/usr/bin/cilium-envoy", "/fixture/admitted.json"}
		if mutant {
			args = append(args, "--logical-peer-mutant")
		}
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if !mutant {
			if err != nil || !strings.Contains(string(output), "NATIVE_ENVOY_PEER=PASS") {
				t.Fatalf("actual proxy boundary: %v: %s", err, output)
			}
		} else if err == nil || !strings.Contains(string(output), "NATIVE_ENVOY_PEER=FAIL reason=forged-physical-peer-accepted") {
			t.Fatalf("logical-peer mutation was not caught at the forged-header boundary: %v: %s", err, output)
		}
	}
	t.Log("NATIVE_ENVOY_PEER=PASS physical=FENCED logical_matcher_mutation=DETECTED")
}
