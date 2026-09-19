// This file extends the silent-no-op argument in instance.go from the verify
// patch to EVERY patch in spec.kustomize.patches (#2996).
//
// The other patches carry controls just as easy to lose: the HA replicas for
// three controllers, the topology spread that keeps one bad worker from taking
// out reconciliation, and the DNS ndots setting. A target that names no
// resource the operator generates removes that control from the cluster while
// the FluxInstance reconciles cleanly and CI stays green.
//
// No renderer can see the result, because flux-operator applies these patches
// at reconcile time. So each target is checked against what this FluxInstance
// itself declares the operator will generate: one Deployment per entry in
// spec.components, all carrying fluxComponentLabelSelector, plus the root
// source. A target outside that set is reported, not guessed at.
package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// fluxComponentLabelSelector is the label flux-operator puts on every
// controller Deployment it generates. A label-selected patch using anything
// else cannot be proven to match a generated resource.
const fluxComponentLabelSelector = "app.kubernetes.io/part-of=flux"

// validatePatchTargets reports every spec.kustomize.patches target that cannot
// be shown to select a resource this FluxInstance generates, or nil when every
// patch lands.
func validatePatchTargets(manifest []byte) error {
	documents, err := decodeAll(manifest)
	if err != nil {
		return fmt.Errorf("FluxInstance manifest does not parse, so patch targets cannot be checked: %w", err)
	}

	instance, err := findInstance(documents)
	if err != nil {
		return err
	}

	components := instanceComponents(instance)
	patches, _ := lookup(instance, []string{"spec", "kustomize", "patches"})
	entries, _ := patches.([]any)

	var problems []string
	for index, entry := range entries {
		patch, ok := asMapping(entry)
		if !ok {
			problems = append(problems, fmt.Sprintf("patch %d is not a mapping", index))

			continue
		}
		if reason := patchTargetProblem(patch["target"], components); reason != "" {
			problems = append(problems, fmt.Sprintf("patch %d (target %s): %s", index, describeTarget(patch["target"]), reason))
		}
	}

	if len(problems) > 0 {
		return errors.New(
			"FluxInstance kustomize patches that select no generated resource are silently dropped by flux-operator:\n  " +
				strings.Join(problems, "\n  "))
	}

	return nil
}

// instanceComponents returns spec.components, the controllers flux-operator
// generates a Deployment for.
func instanceComponents(instance any) []string {
	value, _ := lookup(instance, []string{"spec", "components"})
	list, _ := value.([]any)

	components := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
			components = append(components, strings.TrimSpace(name))
		}
	}

	return components
}

// patchTargetProblem explains why a target cannot be shown to select a
// generated resource, or returns "" when it can.
func patchTargetProblem(value any, components []string) string {
	target, ok := asMapping(value)
	if !ok {
		return "has no target, so the patch is not aimed at any named resource"
	}
	if targetsRootSource(value) {
		return ""
	}

	kind, _ := target["kind"].(string)
	name, _ := target["name"].(string)
	selector, _ := target["labelSelector"].(string)
	namespace, _ := target["namespace"].(string)
	kind, name, selector, namespace = strings.TrimSpace(kind), strings.TrimSpace(name), strings.TrimSpace(selector), strings.TrimSpace(namespace)

	if kind != "Deployment" {
		return fmt.Sprintf("kind %q is neither a controller Deployment nor the root %s", kind, rootSourceKind)
	}
	if namespace != "" && namespace != rootSourceName {
		return fmt.Sprintf("namespace %q excludes every generated controller, which run in %s", namespace, rootSourceName)
	}
	if _, present := target["annotationSelector"]; present {
		return "an annotationSelector cannot be matched against the generated controllers"
	}

	switch {
	case name != "" && selector != "":
		return "names a Deployment and a labelSelector at once; name it by one or the other"
	case name != "":
		if !slices.Contains(components, name) {
			return fmt.Sprintf("no component named %q is declared in spec.components %v", name, components)
		}
	case selector != "":
		if selector != fluxComponentLabelSelector {
			return fmt.Sprintf("labelSelector %q is not %q, the label every generated controller carries", selector, fluxComponentLabelSelector)
		}
	default:
		return "a Deployment target with neither a name nor a labelSelector does not say which controller it changes"
	}

	return ""
}
