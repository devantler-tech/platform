package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"sigs.k8s.io/yaml"
)

type executor func(context.Context, ...string) ([]byte, error)

func allowedDispatch(getEnv func(string) string) bool {
	return getEnv("GITHUB_ACTIONS") == "true" &&
		getEnv("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		getEnv("GITHUB_EVENT_NAME") == "workflow_dispatch" &&
		getEnv("GITHUB_REF") == "refs/heads/main" &&
		getEnv("GITHUB_RUN_ATTEMPT") == "1" &&
		getEnv("CROSSPLANE_RECOVERY_CONFIRM") == "read-crossplane-alert-recovery"
}

func runObservation(ctx context.Context, getEnv func(string) string, read executor, expected map[string]string, checksum string, now time.Time, out io.Writer) int {
	unknown := func() int { fmt.Fprintln(out, "CROSSPLANE_RECOVERY=UNKNOWN"); return 3 }
	if !allowedDispatch(getEnv) {
		return unknown()
	}
	base := []string{"--context=admin@prod", "--request-timeout=20s"}
	requests := [][]string{
		{"-n", "observability", "get", "configmap", "crossplane-sync-exporter", "-o", "json"},
		{"-n", "observability", "get", "deployment", "crossplane-sync-exporter", "-o", "json"},
		{"-n", "observability", "get", "cronjob", "crossplane-sync-alerter", "-o", "json"},
		{"get", "--raw", "/api/v1/namespaces/kubescape/pods/http:alertmanager-0:9093/proxy/api/v2/alerts"},
		{"get", "--raw", "/api/v1/namespaces/kubescape/pods/http:alertmanager-1:9093/proxy/api/v2/alerts"},
	}
	var evidence recoveryEvidence
	outputs := []*[]byte{&evidence.Configuration, &evidence.Deployment, &evidence.Cron, &evidence.Peers[0], &evidence.Peers[1]}
	for i, request := range requests {
		args := append(append([]string(nil), base...), request...)
		raw, err := read(ctx, args...)
		if err != nil {
			return unknown()
		}
		*outputs[i] = raw
	}
	count, err := assessRecovery(evidence, expected, checksum, now)
	if err != nil {
		return unknown()
	}
	if count != 0 {
		fmt.Fprintf(out, "CROSSPLANE_RECOVERY=ALERTS_PRESENT peer_alert_records=%d\n", count)
		return 1
	}
	fmt.Fprintln(out, "CROSSPLANE_RECOVERY=RECOVERED")
	return 0
}

type responseBuffer struct {
	body     []byte
	overflow bool
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if len(p) > maxResponse-len(b.body) {
		b.overflow = true
		return 0, errors.New("response limit exceeded")
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func nativeRead(ctx context.Context, args ...string) ([]byte, error) {
	readContext, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	command := exec.CommandContext(readContext, "kubectl", args...)
	var response responseBuffer
	command.Stdout = &response
	// Even ordinary API errors can include internal names or identities.
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || response.overflow {
		return nil, errors.New("fixed observation failed")
	}
	return response.body, nil
}

func reviewedInputs() (map[string]string, string, error) {
	const directory = "k8s/bases/infrastructure/coroot/components/crossplane-sync-exporter/"
	raw, err := os.ReadFile(directory + "config-map.yaml")
	if err != nil || len(raw) > maxResponse {
		return nil, "", errors.New("reviewed configuration unavailable")
	}
	var config struct{ Data map[string]string }
	if err := yaml.Unmarshal(raw, &config); err != nil || len(config.Data) == 0 {
		return nil, "", errors.New("reviewed configuration invalid")
	}
	raw, err = os.ReadFile(directory + "deployment.yaml")
	if err != nil || len(raw) > maxResponse {
		return nil, "", errors.New("reviewed deployment unavailable")
	}
	var deployment deploymentObservation
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		return nil, "", errors.New("reviewed deployment invalid")
	}
	checksum := deployment.Spec.Template.Metadata.Annotations["platform.devantler.tech/config-checksum"]
	if checksum == "" {
		return nil, "", errors.New("reviewed checksum unavailable")
	}
	return config.Data, checksum, nil
}

func main() {
	// This observer is deliberately unusable with a developer's ambient identity.
	if !allowedDispatch(os.Getenv) {
		fmt.Println("CROSSPLANE_RECOVERY=UNKNOWN")
		os.Exit(3)
	}
	expected, checksum, err := reviewedInputs()
	if err != nil {
		fmt.Println("CROSSPLANE_RECOVERY=UNKNOWN")
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	code := runObservation(ctx, os.Getenv, nativeRead, expected, checksum, time.Now().UTC(), os.Stdout)
	cancel()
	os.Exit(code)
}
