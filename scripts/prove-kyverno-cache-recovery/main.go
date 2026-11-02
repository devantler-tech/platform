package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Published and keyless-signed by CD 41172573040; observed in two Ready
// application Pods. Fresh availability and verification are checked at runtime.
const signedImage = "ghcr.io/devantler-tech/ascoachingogvaner@sha256:d4b35022da531153cdfc01ee6401917f78723695c256dd423e2373b9667acf85"
const unsignedImage = "ghcr.io/devantler-tech/unsigned-probe-throwaway@sha256:5f2763c3032bc40a35781d3d5d12da97e07ba17d99d36bc24c7c265e3ce89dd4"
const validatingWebhook = "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images"

func admitted(raw []byte, name, image string) bool {
	var response struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
			InitContainers      []json.RawMessage `json:"initContainers"`
			EphemeralContainers []json.RawMessage `json:"ephemeralContainers"`
		} `json:"spec"`
	}
	fields := map[string][]string{"": {"apiVersion", "kind", "metadata", "spec"}, "metadata": {"name", "namespace"}, "spec": {"containers", "initContainers", "ephemeralContainers"}, "spec.containers[]": {"name", "image"}}
	return strictjson.Validate(raw, fields) == nil && json.Unmarshal(raw, &response) == nil && response.APIVersion == "v1" && response.Kind == "Pod" && response.Metadata.Name == name && response.Metadata.Namespace == "default" && len(response.Spec.Containers) == 1 && response.Spec.Containers[0].Name == "probe" && response.Spec.Containers[0].Image == image && len(response.Spec.InitContainers) == 0 && len(response.Spec.EphemeralContainers) == 0
}
func policyDenied(raw []byte, err error) bool {
	// Kyverno intentionally replaces internal verifier errors with the policy's
	// message. This identifies the denying policy, never the hidden error cause.
	return err != nil && strings.Contains(string(raw), `admission webhook "`+validatingWebhook+`" denied the request:`) && strings.Contains(string(raw), "Policy verify-app-images failed:") && strings.Contains(string(raw), "first-party app image is not signed by the publish-app release") && !strings.Contains(string(raw), "failed to get imagedata")
}
func runMain() error {
	meta, err := authorize(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	desired, err := loadReviewedSpec()
	if err != nil {
		return err
	}
	result, err := prove(ctx, execute, meta, desired)
	if err != nil {
		return err
	}
	if err = active(ctx); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := runMain(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
