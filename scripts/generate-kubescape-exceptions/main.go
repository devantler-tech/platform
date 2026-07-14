// Command generate-kubescape-exceptions renders a Kubescape exceptions file
// from the ClusterSecurityException CRs.
//
// The platform documents every justified posture finding as a
// ClusterSecurityException CR in k8s/bases/infrastructure/cluster-security-exceptions/
// — that directory is the single source of truth for what is excepted and why.
// The in-cluster kubescape-operator consumes the CRs directly, but the offline
// CI scan (`ksail workload scan --exceptions <file>`) takes Kubescape's native
// format: a JSON array of PostureExceptionPolicy objects. This command derives
// that file from the CRs at scan time, so CI and the cluster can never disagree
// about the exception set.
//
// Fail-closed by design: any CR shape this converter does not recognise (an
// unknown spec.match key, a posture action other than `ignore`, a
// namespaceSelector that isn't the `kubernetes.io/metadata.name In [...]`
// expression) aborts with a non-zero exit instead of silently dropping or
// widening an exception.
//
// Usage, from the repository root:
//
//	go run ./scripts/generate-kubescape-exceptions -o /tmp/exceptions.json
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultDir         = "k8s/bases/infrastructure/cluster-security-exceptions"
	namespaceNameKey   = "kubernetes.io/metadata.name"
	exceptionKind      = "ClusterSecurityException"
	designatorTypeAttr = "Attributes"
)

// designator identifies the Kubernetes resources covered by an exception.
type designator struct {
	DesignatorType string            `json:"designatorType"`
	Attributes     map[string]string `json:"attributes"`
}

// posturePolicy identifies one Kubescape control excluded by a policy.
type posturePolicy struct {
	ControlID string `json:"controlID"`
}

// policy is Kubescape's native PostureExceptionPolicy representation.
type policy struct {
	Name            string          `json:"name"`
	PolicyType      string          `json:"policyType"`
	Actions         []string        `json:"actions"`
	Resources       []designator    `json:"resources"`
	PosturePolicies []posturePolicy `json:"posturePolicies"`
	Reason          string          `json:"reason,omitempty"`
}

// cseErrorf builds the fail-closed error naming the offending CR.
func cseErrorf(path, name, format string, args ...any) error {
	return fmt.Errorf("%s: ClusterSecurityException %q: %s", path, name, fmt.Sprintf(format, args...))
}

// anchor pins a plain value into an exact-match regex and keeps explicit ones.
//
// CR authors write resource `name` fields as anchored regexes already
// (`^velero-server$`) but plain kind/controlID values; Kubescape treats every
// designator attribute and controlID as a regex, so an unanchored plain value
// would substring-match (C-0002 would also match C-0020). A value anchored on
// only one end (`^foo` or `foo$`) is still substring-matchable at the open end,
// so it fails closed instead of passing through unescaped.
func anchor(value, path, name string) (string, error) {
	hasPrefix := strings.HasPrefix(value, "^")
	hasSuffix := strings.HasSuffix(value, "$")

	if hasPrefix && hasSuffix {
		return value, nil
	}

	if hasPrefix || hasSuffix {
		return "", cseErrorf(path, name, "partially anchored regex value %q", value)
	}

	return "^" + regexp.QuoteMeta(value) + "$", nil
}

// stringField reads a required string value, failing closed on any other type.
func stringField(m map[string]any, key, path, name string) (string, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return "", cseErrorf(path, name, "missing %s", key)
	}

	value, ok := raw.(string)
	if !ok {
		return "", cseErrorf(path, name, "%s must be a string, got %v", key, raw)
	}

	return value, nil
}

// unknownKeys reports the keys of m that are not in allowed, sorted.
func unknownKeys(m map[string]any, allowed ...string) []string {
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}

	var unknown []string

	for key := range m {
		if !permitted[key] {
			unknown = append(unknown, key)
		}
	}

	sort.Strings(unknown)

	return unknown
}

// asMapSlice coerces a YAML sequence of mappings, failing closed on any other shape.
func asMapSlice(raw any, path, name, field string) ([]map[string]any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, cseErrorf(path, name, "%s must be a list, got %v", field, raw)
	}

	entries := make([]map[string]any, 0, len(items))

	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, cseErrorf(path, name, "%s entries must be mappings, got %v", field, item)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// convertNamespaceSelector maps a namespaceSelector to one namespace-regex designator.
func convertNamespaceSelector(selector map[string]any, path, name string) ([]designator, error) {
	if unknown := unknownKeys(selector, "matchExpressions"); len(unknown) > 0 {
		return nil, cseErrorf(path, name, "unsupported namespaceSelector keys %v", unknown)
	}

	expressions, err := asMapSlice(selector["matchExpressions"], path, name, "namespaceSelector.matchExpressions")
	if err != nil {
		return nil, err
	}

	if len(expressions) != 1 {
		return nil, cseErrorf(path, name, "expected exactly one namespaceSelector matchExpression")
	}

	expr := expressions[0]
	if expr["key"] != namespaceNameKey || expr["operator"] != "In" {
		return nil, cseErrorf(path, name, "only `%s In [...]` matchExpressions are supported", namespaceNameKey)
	}

	rawValues, ok := expr["values"].([]any)
	if !ok || len(rawValues) == 0 {
		return nil, cseErrorf(path, name, "namespaceSelector matchExpression has no values")
	}

	quoted := make([]string, 0, len(rawValues))

	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		if !ok {
			return nil, cseErrorf(path, name, "namespaceSelector values must be strings, got %v", rawValue)
		}

		quoted = append(quoted, regexp.QuoteMeta(value))
	}

	pattern := "^(" + strings.Join(quoted, "|") + ")$"

	return []designator{{
		DesignatorType: designatorTypeAttr,
		Attributes:     map[string]string{"namespace": pattern},
	}}, nil
}

// convertResources maps match.resources entries to Attributes designators.
//
// apiGroup is intentionally dropped: PostureExceptionPolicy designator
// attributes have no apiGroup field, and the anchored kind+name pair is what
// scopes the exception (the same mapping the in-cluster operator applies).
func convertResources(resources []map[string]any, path, name string) ([]designator, error) {
	designators := make([]designator, 0, len(resources))

	for _, entry := range resources {
		// The CRD's match.resources[] schema allows exactly apiGroup, kind and
		// name — a namespace key would be dropped in-cluster, so accepting it
		// here would let the CI exception diverge from what the operator
		// applies. Fail closed on it like any other unknown key.
		if unknown := unknownKeys(entry, "apiGroup", "kind", "name"); len(unknown) > 0 {
			return nil, cseErrorf(path, name, "unsupported match.resources keys %v", unknown)
		}

		if _, ok := entry["kind"]; !ok {
			return nil, cseErrorf(path, name, "match.resources entry without a kind")
		}

		kind, err := stringField(entry, "kind", path, name)
		if err != nil {
			return nil, err
		}

		anchoredKind, err := anchor(kind, path, name)
		if err != nil {
			return nil, err
		}

		attributes := map[string]string{"kind": anchoredKind}

		if _, ok := entry["name"]; ok {
			resourceName, err := stringField(entry, "name", path, name)
			if err != nil {
				return nil, err
			}

			anchoredName, err := anchor(resourceName, path, name)
			if err != nil {
				return nil, err
			}

			attributes["name"] = anchoredName
		}

		designators = append(designators, designator{
			DesignatorType: designatorTypeAttr,
			Attributes:     attributes,
		})
	}

	return designators, nil
}

// resolveMatch maps spec.match to designators (resources / namespaceSelector / all).
func resolveMatch(match map[string]any, path, name string) ([]designator, error) {
	if unknown := unknownKeys(match, "resources", "namespaceSelector"); len(unknown) > 0 {
		return nil, cseErrorf(path, name, "unsupported match keys %v", unknown)
	}

	rawResources, hasResources := match["resources"]
	rawSelector, hasSelector := match["namespaceSelector"]

	if hasResources && hasSelector {
		return nil, cseErrorf(path, name, "both match.resources and match.namespaceSelector set")
	}

	if hasResources {
		resources, err := asMapSlice(rawResources, path, name, "match.resources")
		if err != nil {
			return nil, err
		}

		if len(resources) == 0 {
			return nil, cseErrorf(path, name, "match.resources is empty")
		}

		return convertResources(resources, path, name)
	}

	if hasSelector {
		selector, ok := rawSelector.(map[string]any)
		if !ok || len(selector) == 0 {
			return nil, cseErrorf(path, name, "match.namespaceSelector is empty")
		}

		return convertNamespaceSelector(selector, path, name)
	}

	// No match => the exception applies cluster-wide for its controls.
	return []designator{{
		DesignatorType: designatorTypeAttr,
		Attributes:     map[string]string{"namespace": ".*"},
	}}, nil
}

// convertDocument converts one ClusterSecurityException document; nil for other kinds.
func convertDocument(doc any, path string) (*policy, error) {
	document, ok := doc.(map[string]any)
	if !ok || document["kind"] != exceptionKind {
		return nil, nil //nolint:nilnil // a non-CSE document is skipped, not an error
	}

	metadata, _ := document["metadata"].(map[string]any)

	name, _ := metadata["name"].(string)
	if name == "" {
		return nil, cseErrorf(path, "<unnamed>", "missing metadata.name")
	}

	spec, _ := document["spec"].(map[string]any)

	posture, err := asMapSlice(spec["posture"], path, name, "spec.posture")
	if err != nil || len(posture) == 0 {
		return nil, cseErrorf(path, name, "spec.posture is empty")
	}

	policies := make([]posturePolicy, 0, len(posture))

	for _, control := range posture {
		if action := control["action"]; action != "ignore" {
			return nil, cseErrorf(path, name, "unsupported posture action %v", action)
		}

		controlID, err := stringField(control, "controlID", path, name)
		if err != nil {
			return nil, cseErrorf(path, name, "posture entry without a controlID")
		}

		anchored, err := anchor(controlID, path, name)
		if err != nil {
			return nil, err
		}

		policies = append(policies, posturePolicy{ControlID: anchored})
	}

	match := map[string]any{}

	if raw, ok := spec["match"]; ok && raw != nil {
		// Fail closed: an explicit-but-malformed match ([], "", false, {}) must
		// never be coerced into the cluster-wide default.
		parsed, isMap := raw.(map[string]any)
		if !isMap || len(parsed) == 0 {
			return nil, cseErrorf(path, name, "spec.match must be a non-empty mapping, got %v", raw)
		}

		match = parsed
	}

	resources, err := resolveMatch(match, path, name)
	if err != nil {
		return nil, err
	}

	result := &policy{
		Name:            name,
		PolicyType:      "postureExceptionPolicy",
		Actions:         []string{"alertOnly"},
		Resources:       resources,
		PosturePolicies: policies,
	}

	if reason, ok := spec["reason"].(string); ok && strings.TrimSpace(reason) != "" {
		result.Reason = strings.Join(strings.Fields(reason), " ")
	}

	return result, nil
}

// generate converts every CSE document under directory into sorted policies.
func generate(directory string) ([]policy, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", directory, err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if ext := filepath.Ext(entry.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}

		names = append(names, entry.Name())
	}

	sort.Strings(names)

	var policies []policy

	seen := map[string]bool{}

	for _, filename := range names {
		path := filepath.Join(directory, filename)

		documents, err := decodeDocuments(path)
		if err != nil {
			return nil, err
		}

		for _, doc := range documents {
			converted, err := convertDocument(doc, path)
			if err != nil {
				return nil, err
			}

			if converted == nil {
				continue
			}

			if seen[converted.Name] {
				return nil, cseErrorf(path, converted.Name, "duplicate exception name")
			}

			seen[converted.Name] = true

			policies = append(policies, *converted)
		}
	}

	if len(policies) == 0 {
		return nil, fmt.Errorf("%s: no ClusterSecurityException documents found", directory)
	}

	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })

	return policies, nil
}

// decodeDocuments reads every YAML document in a multi-document file.
func decodeDocuments(path string) ([]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	var documents []any

	decoder := yaml.NewDecoder(file)

	for {
		var document any

		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		documents = append(documents, document)
	}

	return documents, nil
}

// render marshals the policies as Kubescape's native exceptions JSON.
func render(policies []policy) ([]byte, error) {
	encoded, err := json.MarshalIndent(policies, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal exceptions: %w", err)
	}

	return append(encoded, '\n'), nil
}

// main converts the configured exception directory and writes Kubescape JSON.
func main() {
	output := flag.String("o", "", "output file (stdout if omitted)")
	flag.StringVar(output, "output", "", "output file (stdout if omitted)")
	flag.Parse()

	directory := defaultDir
	if flag.NArg() > 0 {
		directory = flag.Arg(0)
	}

	policies, err := generate(directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	rendered, err := render(policies)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *output == "" {
		_, _ = os.Stdout.Write(rendered)

		return
	}

	if err := os.WriteFile(*output, rendered, 0o644); err != nil { //nolint:gosec // a CI scan input, not a secret
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "wrote %d exception policies to %s\n", len(policies), *output)
}
