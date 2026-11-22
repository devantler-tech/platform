package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const runtimePublicationCaptureOperation = "capture-runtime-publication"
const runtimePublicationCheckOperation = "check-runtime-publication"

func runRuntimePublication(ctx context.Context, getenv func(string) string) (err error) {
	phase := "checkout"
	defer func() {
		if err != nil {
			err = fmt.Errorf("runtime publication held: phase=%s", phase)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	head, err := inventoryExecuteCommand(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return errRuntimePublication
	}
	event, err := os.ReadFile(getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return errRuntimePublication
	}
	phase = "source-binding"
	binding, err := runtimePublicationContext(getenv, event, strings.TrimSpace(string(head)))
	if err != nil {
		return err
	}
	phase = "producer-configuration"
	configuration, err := os.ReadFile("ksail.prod.yaml")
	if err != nil {
		return errRuntimePublication
	}
	wrappers, err := inventoryExecuteCommand(ctx, "kubectl", "kustomize", "k8s/clusters/prod")
	if err != nil || runtimePublicationSource(configuration, wrappers) != nil {
		return errRuntimePublication
	}
	phase = "producer-inputs-before"
	fingerprint, err := runtimePublicationFingerprint("k8s")
	if err != nil {
		return err
	}
	if getenv("LONGHORN_V2_OPERATION") == runtimePublicationCaptureOperation {
		phase = "input-receipt"
		output, err := os.OpenFile(getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return errRuntimePublication
		}
		_, writeErr := fmt.Fprintf(output, "source_fingerprint=%s\n", fingerprint)
		closeErr := output.Close()
		if writeErr != nil || closeErr != nil {
			return errRuntimePublication
		}
		fmt.Println("LONGHORN_RUNTIME_PUBLICATION=INPUTS_BOUND writes=0")
		return nil
	}
	if getenv("LONGHORN_V2_OPERATION") != runtimePublicationCheckOperation || fingerprint != getenv("LONGHORN_RUNTIME_INPUT_FINGERPRINT") || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(fingerprint) {
		return errRuntimePublication
	}
	// These are the production Flux layers, not Helm's unrelated local profile.
	phase = "production-render"
	var render []byte
	var roots []string
	for _, path := range runtimePublicationRoots() {
		roots = append(roots, "k8s/"+path)
	}
	sort.Strings(roots)
	for _, root := range roots {
		raw, err := inventoryExecuteCommand(ctx, "kubectl", "kustomize", root)
		if err != nil {
			return errRuntimePublication
		}
		if len(render) > 0 {
			render = append(render, []byte("\n---\n")...)
		}
		render = append(render, raw...)
		if len(render) > 16<<20 {
			return errRuntimePublication
		}
	}
	phase = "candidate-image"
	if runtimePublicationFluxSource(render) != nil {
		return errRuntimePublication
	}
	image, err := runtimePublicationCandidate(render)
	if err != nil {
		return err
	}
	verdict := "NOT_REQUESTED"
	var receipt runtimeInventoryReceipt
	if image != runtimeStockImage {
		phase = "native-inventory"
		receipt, err = observeRuntimeInventory(ctx, client{run: runtimeInventoryReadCommand, context: "admin@prod"}, binding.Base)
		if err != nil {
			return err
		}
		phase = "declared-overlap"
		verdict, err = runtimePublicationVerdict(image, receipt)
		if err != nil {
			return err
		}
		if verdict == "FIT" {
			phase = "isolated-first-selection"
			if err := runtimePublicationFirstSelection(ctx, binding); err != nil {
				return err
			}
		}
	}
	phase = "producer-inputs-after"
	final, err := runtimePublicationFingerprint("k8s")
	if err != nil || final != fingerprint || !commandContextActive(ctx) {
		return errRuntimePublication
	}
	fmt.Printf("LONGHORN_RUNTIME_PUBLICATION=%s storageNodes=%d overlapNodes=%d epochSHA256=%s adoption=UNVERIFIED producerInputs=UNCHANGED writes=0\n", verdict, receipt.Nodes, receipt.OverlapNodes, receipt.EpochSHA)
	return nil
}

func runtimePublicationFirstSelection(ctx context.Context, binding runtimePublicationBinding) error {
	// A recovery checkout is not proof of the last deployed source. A first
	// switch therefore uses the protected queue's isolated, current-main base.
	if binding.Kind != "queue" {
		return errRuntimePublication
	}
	raw, err := inventoryExecuteCommand(ctx, "git", "diff", "--name-only", "--no-renames", binding.Base, binding.Candidate, "--")
	if err != nil || !runtimePublicationChangeScope(raw) {
		return errRuntimePublication
	}
	if _, err := inventoryExecuteCommand(ctx, "git", "diff", "--quiet", "HEAD", "--", "k8s/", "ksail.prod.yaml", "ksail.yaml", "talos/", "talos-local/", ".sops.yaml", ".gitignore"); err != nil {
		return errRuntimePublication
	}
	untracked, err := inventoryExecuteCommand(ctx, "git", "ls-files", "--others", "--", "k8s/")
	if err != nil || len(untracked) != 0 {
		return errRuntimePublication
	}
	baseline, err := inventoryExecuteCommand(ctx, "git", "show", binding.Base+":"+runtimeControllersRoot)
	if err != nil {
		return errRuntimePublication
	}
	candidate, err := os.ReadFile(runtimeControllersRoot)
	if err != nil || runtimePublicationActivation(baseline, candidate) != nil {
		return errRuntimePublication
	}
	profile, err := os.ReadFile("k8s/bases/components/longhorn-v1-retirement-runtime/kustomization.yaml")
	if err != nil || !runtimePublicationProfile(profile) {
		return errRuntimePublication
	}
	return nil
}

func runtimePublicationProfile(data []byte) bool {
	encoded, err := runtimeSingleYAML(data)
	var component object
	if err != nil || json.Unmarshal(encoded, &component) != nil || text(component, "apiVersion") != "kustomize.config.k8s.io/v1alpha1" || text(component, "kind") != "Component" || !runtimeOnlyKeys(component, "apiVersion", "kind", "patches") {
		return false
	}
	patches, ok := value(component, "patches").([]any)
	if !ok || len(patches) != 1 {
		return false
	}
	patch, ok := patches[0].(object)
	if !ok || !runtimeOnlyKeys(patch, "target", "patch") || !reflect.DeepEqual(value(patch, "target"), object{"group": "helm.toolkit.fluxcd.io", "version": "v2", "kind": "HelmRelease", "name": "longhorn", "namespace": "longhorn-system"}) {
		return false
	}
	encoded, err = runtimeSingleYAML([]byte(text(patch, "patch")))
	var release object
	if err != nil || json.Unmarshal(encoded, &release) != nil {
		return false
	}
	expected := object{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": object{"name": "longhorn", "namespace": "longhorn-system"}, "spec": object{"values": object{
		"image":           object{"longhorn": object{"instanceManager": object{"registry": "ghcr.io", "repository": "devantler-tech/platform-longhorn-instance-manager", "tag": strings.TrimPrefix(runtimeRetirementImage, "ghcr.io/devantler-tech/platform-longhorn-instance-manager:")}}},
		"longhornManager": object{"updateStrategy": object{"type": "RollingUpdate", "rollingUpdate": object{"maxUnavailable": float64(1)}}},
	}}}
	return reflect.DeepEqual(release, expected)
}
