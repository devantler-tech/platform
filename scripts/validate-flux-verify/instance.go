// This file asserts the half of the contract that reaches the RUNNING cluster.
//
// validate() (main.go) checks ksail.prod.yaml, which is what KSail renders when
// it BOOTSTRAPS the FluxInstance. That path does not run on a routine deploy:
// measured on the live cluster, flux-operator is the Apply manager of
// ocirepository/flux-system and owns url/ref/interval/provider/secretRef, while
// KSail's only field-manager entry on that resource is a reconcile-trigger
// annotation — it writes no spec key there at all. So a correct ksail.prod.yaml
// can sit beside a live root source with spec.verify absent, which is exactly
// the state #2922 recorded.
//
// flux-operator's supported way to add a field to a resource it generates is
// spec.kustomize.patches, and the field then lands under the manager that
// already owns the resource — no second writer, no reconcile fight. That patch
// is therefore the thing that makes verification real, and this is the check
// that it exists and is aimed at the right resource.
//
// 🔴 A MISTARGETED KUSTOMIZE PATCH IS A SILENT NO-OP.
//
// These patches are applied by flux-operator at reconcile time, not by CI, so
// nothing in the pipeline renders them: `kubectl kustomize` copies the patch
// list into the FluxInstance verbatim and reports success whatever the target
// says. Measured separately against a rendered OCIRepository, a target naming
// the wrong kind or the wrong name produces no output, exit 0 and no warning.
// So a typo in the target below yields a security control that is present in
// the file, absent from the cluster, and green in CI — the FluxInstance
// reconciles cleanly and the deploy succeeds either way.
//
// That is why a verify patch pointing anywhere other than the root source is
// reported as a failure here rather than ignored as irrelevant: it is far
// likelier to be the intended control that missed than a deliberate one.
package main

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// rootSourceKind and rootSourceName identify the operator-generated source that
// every controller, tenant binding and policy arrives through. Its name is set
// by flux-operator and is not configurable per-instance.
const (
	rootSourceKind = "OCIRepository"
	rootSourceName = "flux-system"
	verifyOpPath   = "/spec/verify"
)

// writingOps are the JSON-patch operations that PUT a value at a path. `test`
// only asserts and `remove` deletes, so neither delivers the control even
// though both name the same path.
var writingOps = map[string]bool{"add": true, "replace": true}

// errNoInstance is returned when the manifest carries no FluxInstance, which
// means this check was pointed at the wrong file — a failure of the check's own
// wiring, not of the platform's configuration.
var errNoInstance = errors.New("no FluxInstance in manifest")

// validateInstance reports why the FluxInstance does not put an effective
// verify block on the live root source, or nil when it does.
func validateInstance(manifest []byte) error {
	_, err := instanceVerifyBlock(manifest)

	return err
}

// instanceVerifyBlock is validateInstance plus the block it validated, so the
// drift check below can compare the two halves without walking the tree a
// second time and inventing a second opinion about where the block lives.
func instanceVerifyBlock(manifest []byte) (map[string]any, error) {
	documents, err := decodeAll(manifest)
	if err != nil {
		return nil, fmt.Errorf("FluxInstance manifest does not parse, so verification cannot be established: %w", err)
	}

	instance, err := findInstance(documents)
	if err != nil {
		return nil, err
	}

	patches, _ := lookup(instance, []string{"spec", "kustomize", "patches"})

	entries, ok := patches.([]any)
	if !ok {
		entries = nil
	}

	var (
		mistargeted []string
		value       any
		found       bool
	)

	for _, entry := range entries {
		patch, ok := asMapping(entry)
		if !ok {
			continue
		}

		operation, ok := verifyWrite(patch["patch"])
		if !ok {
			continue
		}

		if !targetsRootSource(patch["target"]) {
			mistargeted = append(mistargeted, describeTarget(patch["target"]))

			continue
		}

		// Deliberately NOT a break: stopping at the first hit would make the
		// mistargeted report depend on patch ORDER, so the same pair of patches
		// would be reported or ignored according to which came first. Every
		// verify patch is inspected, and the first correctly-targeted value is
		// the one checked.
		if !found {
			value, found = operation["value"], true
		}
	}

	// Reported before the missing-patch case, because a mistargeted patch is
	// the reason the control is usually absent and naming it is the actionable
	// half — the same ordering, and the same reasoning, as the stray-block
	// check in validate().
	if len(mistargeted) > 0 {
		return nil, fmt.Errorf(
			"a kustomize patch adds %s but does not target the root source (%s/%s): its target is %s, "+
				"which renders NOTHING and reports no error, so that patch protects nothing: "+
				"point the target at kind %s, name %s, or delete it",
			verifyOpPath, rootSourceKind, rootSourceName, strings.Join(mistargeted, ", "),
			rootSourceKind, rootSourceName,
		)
	}

	if !found {
		return nil, fmt.Errorf(
			"no kustomize patch adds %s to the %s/%s source, so the live root source pulls unverified "+
				"however the cluster config reads (flux-operator owns that resource; a patch is how the "+
				"field reaches it): add a spec.kustomize.patches entry targeting kind %s, name %s",
			verifyOpPath, rootSourceKind, rootSourceName, rootSourceKind, rootSourceName,
		)
	}

	block, err := checkVerifyValue(value)
	if err != nil {
		return nil, err
	}

	return block, nil
}

// checkVerifyValue applies the SAME predicates validate() applies to the KSail
// config. Reusing them is the point: two opinions about what an effective
// verify block looks like is how one of them ends up weaker than the other.
func checkVerifyValue(value any) (map[string]any, error) {
	block, ok := asMapping(value)
	if !ok {
		return nil, fmt.Errorf("the %s patch value is not a mapping, so it cannot decode into a verify spec",
			verifyOpPath)
	}

	provider, isString := block["provider"].(string)
	if !enabled(provider) {
		reported := any(provider)
		if !isString {
			reported = block["provider"]
		}

		// Flux's own CRD would reject a blank provider outright rather than
		// silently disable verification, so this is a stricter superset of what
		// the API enforces. It shares validate()'s predicate on purpose: one
		// definition of "switched on" across both halves of the contract.
		return nil, fmt.Errorf(
			"the %s patch sets provider %#v, which is not a usable provider and leaves verification "+
				"DISABLED: set it to cosign",
			verifyOpPath, reported,
		)
	}

	if !constrainsSigner(block) {
		return nil, fmt.Errorf(
			"the %s patch is enabled but constrains no signer, so it accepts any keylessly-signed "+
				"artifact from any signer: add a matchOIDCIdentity entry with a non-blank issuer and "+
				"subject, or a secretRef naming a key Secret",
			verifyOpPath,
		)
	}

	return block, nil
}

// checkNoDrift requires the two halves to pin the SAME signers.
//
// The policy is deliberately written twice — once for bootstrap in the cluster
// config, once for the running cluster in the FluxInstance — because the two
// are applied by different components at different times and neither can cover
// the other's window. Two copies of a security policy that nothing compares is
// how one of them quietly stops matching the other: tighten the signer list in
// one place, and a rebuilt cluster keeps trusting whoever the stale copy named,
// with every check still green. Comparing them is what makes the duplication
// safe rather than merely documented.
//
// Only the signer-constraining fields are compared. They are the whole security
// content of the block, and requiring byte-equality of everything would fail on
// differences that change no trust decision.
func checkNoDrift(config, instance map[string]any) error {
	for _, field := range []string{"provider", "matchOIDCIdentity", "secretRef"} {
		if reflect.DeepEqual(config[field], instance[field]) {
			continue
		}

		return fmt.Errorf(
			"the cluster config and the FluxInstance patch pin different signers, so a bootstrapped "+
				"cluster and a running one would trust different publishers — %q differs:\n%s\n"+
				"make the two verify blocks identical",
			field, describeDrift(config[field], instance[field]),
		)
	}

	return nil
}

// describeDrift names the entries that differ rather than dumping both values.
//
// The lists are long, near-identical and full of escaped regex, so printing
// them whole hides the one changed character in a wall of text — and a gate
// whose message cannot be read is a gate people work around instead of fixing.
// Only what is present on one side and missing on the other is shown.
func describeDrift(config, instance any) string {
	configItems, configIsList := renderMatchers(config)
	instanceItems, instanceIsList := renderMatchers(instance)

	if !configIsList || !instanceIsList {
		return fmt.Sprintf("  cluster config:    %#v\n  FluxInstance patch: %#v", config, instance)
	}

	var lines []string
	for _, item := range difference(configItems, instanceItems) {
		lines = append(lines, "  only in the cluster config:    "+item)
	}

	for _, item := range difference(instanceItems, configItems) {
		lines = append(lines, "  only in the FluxInstance patch: "+item)
	}

	// Same members, different order or count. Order carries no meaning to Flux,
	// but the two files still do not match, so say what IS different rather
	// than printing an empty list and looking like a false positive.
	if len(lines) == 0 {
		return fmt.Sprintf("  the same %d entries in a different order or with duplicates",
			len(configItems))
	}

	sort.Strings(lines)

	return strings.Join(lines, "\n")
}

// renderMatchers flattens a matchOIDCIdentity list into comparable strings.
func renderMatchers(value any) ([]string, bool) {
	entries, ok := value.([]any)
	if !ok {
		return nil, false
	}

	items := make([]string, 0, len(entries))

	for _, entry := range entries {
		matcher, ok := asMapping(entry)
		if !ok {
			items = append(items, fmt.Sprintf("%#v", entry))

			continue
		}

		issuer, _ := matcher["issuer"].(string)
		subject, _ := matcher["subject"].(string)
		items = append(items, fmt.Sprintf("issuer=%s subject=%s", issuer, subject))
	}

	return items, true
}

// difference returns the members of a that b does not contain.
func difference(a, b []string) []string {
	present := make(map[string]bool, len(b))
	for _, item := range b {
		present[item] = true
	}

	var only []string

	for _, item := range a {
		if !present[item] {
			only = append(only, item)
		}
	}

	return only
}

// findInstance returns the single FluxInstance document. The manifest opens
// with a comment-only document, which decodes to nil, so selecting by kind
// rather than by position is what keeps this pointed at the right document.
func findInstance(documents []any) (any, error) {
	for _, document := range documents {
		mapping, ok := asMapping(document)
		if !ok {
			continue
		}

		if kind, _ := mapping["kind"].(string); kind == "FluxInstance" {
			return document, nil
		}
	}

	return nil, errNoInstance
}

// verifyWrite returns the operation writing verifyOpPath within one patch body,
// which flux-operator carries as a STRING holding a JSON6902 op list — so it
// has to be parsed a second time rather than walked as part of the outer tree.
func verifyWrite(body any) (map[string]any, bool) {
	text, ok := body.(string)
	if !ok {
		return nil, false
	}

	var operations []any
	if err := yaml.Unmarshal([]byte(text), &operations); err != nil {
		return nil, false
	}

	for _, item := range operations {
		operation, ok := asMapping(item)
		if !ok {
			continue
		}

		path, _ := operation["path"].(string)
		if strings.TrimSpace(path) != verifyOpPath {
			continue
		}

		if op, _ := operation["op"].(string); writingOps[strings.TrimSpace(op)] {
			return operation, true
		}
	}

	return nil, false
}

// targetsRootSource reports whether a patch target names the root source
// specifically.
//
// A target carrying no name matches every OCIRepository the operator generates,
// so the manifest does not state which resource the control lands on. That
// ambiguity is treated as a miss: a control whose subject is inferred rather
// than written is not one this check can certify.
func targetsRootSource(value any) bool {
	target, ok := asMapping(value)
	if !ok {
		return false
	}

	kind, _ := target["kind"].(string)
	name, _ := target["name"].(string)

	if strings.TrimSpace(kind) != rootSourceKind || strings.TrimSpace(name) != rootSourceName {
		return false
	}

	// A namespace is optional in a kustomize target — absent means "any", which
	// still includes the root source. A namespace that is PRESENT and different
	// excludes it, so only that case is a miss.
	namespace, isString := target["namespace"].(string)
	if isString && strings.TrimSpace(namespace) != "" && strings.TrimSpace(namespace) != rootSourceName {
		return false
	}

	return true
}

// describeTarget renders a target for the failure message. The message has to
// name the wrong target concretely, or the reader is told a patch missed
// without being told which one.
func describeTarget(value any) string {
	target, ok := asMapping(value)
	if !ok {
		return "absent"
	}

	kind, _ := target["kind"].(string)
	name, _ := target["name"].(string)

	if strings.TrimSpace(name) == "" {
		name = "<unnamed>"
	}

	if strings.TrimSpace(kind) == "" {
		kind = "<no kind>"
	}

	return fmt.Sprintf("kind %s, name %s", kind, name)
}
