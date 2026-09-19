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
// itself declares the operator will generate: one Deployment per component,
// all carrying fluxComponentLabelSelector, plus the root source. The two
// flux-operator behaviours recorded in the production FluxInstance are
// enforced as well: component patches run before the namespace transformer,
// so a namespaced Deployment selector matches nothing, and only the first of
// two name-targeted patches on one controller is applied.
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

// defaultFluxComponents is what flux-operator deploys when a FluxInstance
// omits spec.components.
var defaultFluxComponents = []string{
	"source-controller",
	"kustomize-controller",
	"helm-controller",
	"notification-controller",
}

// patchSelector is the resource a patch is aimed at, from its target or, for a
// target-less strategic-merge patch, from the document it merges.
type patchSelector struct {
	kind, name, namespace, labelSelector string
	annotationSelector                   bool
}

// validatePatchTargets reports every spec.kustomize.patches entry that cannot
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
	namedDeployments := map[string]int{}
	for index, entry := range entries {
		patch, ok := asMapping(entry)
		if !ok {
			problems = append(problems, fmt.Sprintf("patch %d is not a mapping", index))

			continue
		}

		selector, reason := selectorOf(patch)
		if reason == "" {
			reason = selectorProblem(selector, components)
		}
		if reason == "" && selector.kind == "Deployment" && selector.name != "" {
			if first, seen := namedDeployments[selector.name]; seen {
				reason = fmt.Sprintf("patch %d already targets %q by name; flux-operator applies only the first, so merge them", first, selector.name)
			} else {
				namedDeployments[selector.name] = index
			}
		}
		if reason != "" {
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

// instanceComponents returns spec.components, or flux-operator's default set
// when the field is absent.
func instanceComponents(instance any) []string {
	value, present := lookup(instance, []string{"spec", "components"})
	list, isList := value.([]any)
	if !present || !isList {
		return defaultFluxComponents
	}

	components := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
			components = append(components, strings.TrimSpace(name))
		}
	}

	return components
}

// selectorOf reads the resource a patch is aimed at. A target-less patch is
// accepted only as a strategic merge whose own document names the resource: a
// target-less JSON6902 operation list selects nothing.
func selectorOf(patch map[string]any) (patchSelector, string) {
	if target, ok := asMapping(patch["target"]); ok {
		_, annotated := target["annotationSelector"]

		return patchSelector{
			kind:               trimmed(target["kind"]),
			name:               trimmed(target["name"]),
			namespace:          trimmed(target["namespace"]),
			labelSelector:      trimmed(target["labelSelector"]),
			annotationSelector: annotated,
		}, ""
	}

	body, _ := patch["patch"].(string)
	documents, err := decodeAll([]byte(body))
	if err != nil || len(documents) != 1 {
		return patchSelector{}, "has no target, and its patch is not a single strategic-merge document that names the resource"
	}
	document, ok := asMapping(documents[0])
	if !ok {
		return patchSelector{}, "has no target, and a JSON6902 operation list without one selects nothing"
	}
	metadata, _ := asMapping(document["metadata"])

	return patchSelector{
		kind:      trimmed(document["kind"]),
		name:      trimmed(metadata["name"]),
		namespace: trimmed(metadata["namespace"]),
	}, ""
}

// selectorProblem explains why a selector cannot be shown to select a
// generated resource, or returns "" when it can.
func selectorProblem(selector patchSelector, components []string) string {
	if selector.kind == rootSourceKind && selector.name == rootSourceName &&
		(selector.namespace == "" || selector.namespace == rootSourceName) &&
		selector.labelSelector == "" && !selector.annotationSelector {
		return ""
	}
	if selector.kind != "Deployment" {
		return fmt.Sprintf("kind %q is neither a controller Deployment nor the root %s", selector.kind, rootSourceKind)
	}
	if selector.namespace != "" {
		return fmt.Sprintf("namespace %q on a Deployment matches nothing, because flux-operator applies component patches before it sets the namespace; omit it", selector.namespace)
	}
	if selector.annotationSelector {
		return "an annotationSelector cannot be matched against the generated controllers"
	}

	switch {
	case selector.name != "" && selector.labelSelector != "":
		return "names a Deployment and a labelSelector at once; name it by one or the other"
	case selector.name != "":
		if !slices.Contains(components, selector.name) {
			return fmt.Sprintf("no component named %q is generated; components are %v", selector.name, components)
		}
	case selector.labelSelector != "":
		if selector.labelSelector != fluxComponentLabelSelector {
			return fmt.Sprintf("labelSelector %q is not %q, the label every generated controller carries", selector.labelSelector, fluxComponentLabelSelector)
		}
	default:
		return "a Deployment target with neither a name nor a labelSelector does not say which controller it changes"
	}

	return ""
}

func trimmed(value any) string {
	text, _ := value.(string)

	return strings.TrimSpace(text)
}
