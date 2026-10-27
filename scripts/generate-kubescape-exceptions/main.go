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
// The CR's ignore action maps to native disable. Native alertOnly acknowledges
// a finding but keeps it failing in current Kubescape result handling.
//
// Fail-closed by design: any CR shape this converter does not recognise (an
// unknown spec.match key, a posture action other than `ignore`, a
// namespaceSelector that isn't a `kubernetes.io/metadata.name In [...]` or
// `NotIn [...]` expression) aborts with a non-zero exit instead of silently
// dropping or widening an exception.
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

	// clusterWideAnnotation declares that an exception is MEANT to suppress its
	// controls for every workload in the cluster. An omitted `spec.match` is
	// what produces that scope, so without this marker the widest exception the
	// repository can express is also the one written by typing the least, and a
	// forgotten scope is indistinguishable from a deliberate one. Suppression is
	// silent — an excepted workload that genuinely violates a control reads
	// exactly like a compliant one — so the scope has to be stated, not inferred
	// from an absence.
	clusterWideAnnotation = "platform.devantler.tech/cluster-wide"
	// clusterWideDeclared is the only accepted clusterWideAnnotation value. Any
	// other value fails closed rather than being read as a declaration: a typo
	// must not grant cluster-wide suppression.
	clusterWideDeclared = "declared"
)

// designator identifies the Kubernetes resources covered by an exception.
type designator struct {
	DesignatorType string            `json:"designatorType"`
	Attributes     map[string]string `json:"attributes"`
}

// posturePolicy identifies one Kubescape control excluded by a policy.
type posturePolicy struct {
	ControlID     string `json:"controlID"`
	FrameworkName string `json:"frameworkName,omitempty"`
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

type namespacePrefix struct {
	terminal bool
	children map[rune]*namespacePrefix
}

// escapeRegexClassRune uses the small character-class escape vocabulary shared
// by RE2 (Go/OPA) and other common regex engines, rather than RE2-only
// hex-brace escapes, so a generated designator stays portable.
func escapeRegexClassRune(r rune) string {
	switch r {
	case '\\', '-', ']', '^':
		return "\\" + string(r)
	default:
		return string(r)
	}
}

// namespaceNotInPattern renders the complement of a finite set without regex
// lookarounds. Kubescape and OPA use RE2, which deliberately does not support
// negative lookahead, so a direct `^(?!excluded$).+$` cannot be consumed by the
// actual exception engines. The prefix tree instead emits one alternative for
// every first point at which a non-empty namespace differs from an exclusion,
// plus extensions of an excluded name.
func namespaceNotInPattern(values []string) string {
	root := &namespacePrefix{children: map[rune]*namespacePrefix{}}

	for _, value := range values {
		node := root
		for _, r := range value {
			child := node.children[r]
			if child == nil {
				child = &namespacePrefix{children: map[rune]*namespacePrefix{}}
				node.children[r] = child
			}
			node = child
		}
		node.terminal = true
	}

	var alternatives []string
	var walk func(*namespacePrefix, string)
	walk = func(node *namespacePrefix, prefix string) {
		if prefix != "" && !node.terminal {
			alternatives = append(alternatives, regexp.QuoteMeta(prefix))
		}

		if len(node.children) == 0 {
			alternatives = append(alternatives, regexp.QuoteMeta(prefix)+".+")
			return
		}

		children := make([]rune, 0, len(node.children))
		for r := range node.children {
			children = append(children, r)
		}
		sort.Slice(children, func(i, j int) bool { return children[i] < children[j] })

		var excludedNext strings.Builder
		for _, r := range children {
			excludedNext.WriteString(escapeRegexClassRune(r))
		}
		alternatives = append(alternatives,
			regexp.QuoteMeta(prefix)+"[^"+excludedNext.String()+"].*")

		for _, r := range children {
			walk(node.children[r], prefix+string(r))
		}
	}

	walk(root, "")

	return "^(" + strings.Join(alternatives, "|") + ")$"
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
	operator, operatorOK := expr["operator"].(string)
	if expr["key"] != namespaceNameKey || !operatorOK || (operator != "In" && operator != "NotIn") {
		return nil, cseErrorf(path, name,
			"only `%s In [...]` or `%s NotIn [...]` matchExpressions are supported",
			namespaceNameKey, namespaceNameKey)
	}

	rawValues, ok := expr["values"].([]any)
	if !ok || len(rawValues) == 0 {
		return nil, cseErrorf(path, name, "namespaceSelector matchExpression has no values")
	}

	values := make([]string, 0, len(rawValues))
	quoted := make([]string, 0, len(rawValues))

	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		if !ok {
			return nil, cseErrorf(path, name, "namespaceSelector values must be strings, got %v", rawValue)
		}

		values = append(values, value)
		quoted = append(quoted, regexp.QuoteMeta(value))
	}

	pattern := "^(" + strings.Join(quoted, "|") + ")$"
	if operator == "NotIn" {
		pattern = namespaceNotInPattern(values)
	}

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

	// No match => the exception applies to every resource for its controls.
	// Matching on kind, which every Kubernetes resource has, also covers
	// cluster-scoped resources that do not carry a namespace.
	return []designator{{
		DesignatorType: designatorTypeAttr,
		Attributes:     map[string]string{"kind": ".*"},
	}}, nil
}

// resolveClusterWideDeclaration reads the CR's cluster-wide scope marker.
//
// Absent annotation => not declared, which is the safe default: an exception
// that forgot to scope itself then fails closed at conversion instead of
// silently excepting its controls for every workload in the cluster.
func resolveClusterWideDeclaration(metadata map[string]any, path, name string) (bool, error) {
	rawAnnotations, present := metadata["annotations"]
	if !present || rawAnnotations == nil {
		return false, nil
	}

	// Present but not a mapping must fail closed: a malformed annotations block
	// must not be read as "no marker", because here that reading is the
	// permissive one.
	annotations, ok := rawAnnotations.(map[string]any)
	if !ok {
		return false, cseErrorf(path, name, "metadata.annotations must be a mapping, got %v", rawAnnotations)
	}

	raw, ok := annotations[clusterWideAnnotation]
	if !ok {
		return false, nil
	}

	value, ok := raw.(string)
	if !ok {
		return false, cseErrorf(path, name, "%s must be a string, got %v", clusterWideAnnotation, raw)
	}

	if value != clusterWideDeclared {
		return false, cseErrorf(path, name, "unsupported %s value %q (only %q is recognised)", clusterWideAnnotation, value, clusterWideDeclared)
	}

	return true, nil
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
	if _, ok := spec["expiresAt"]; ok {
		return nil, cseErrorf(path, name, "spec.expiresAt cannot be preserved in Kubescape exceptions")
	}

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

		converted := posturePolicy{ControlID: anchored}

		if _, ok := control["frameworkName"]; ok {
			frameworkName, err := stringField(control, "frameworkName", path, name)
			if err != nil {
				return nil, err
			}

			converted.FrameworkName, err = anchor(frameworkName, path, name)
			if err != nil {
				return nil, err
			}
		}

		policies = append(policies, converted)
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

	clusterWide, err := resolveClusterWideDeclaration(metadata, path, name)
	if err != nil {
		return nil, err
	}

	switch {
	case len(match) == 0 && !clusterWide:
		return nil, cseErrorf(path, name,
			"no spec.match, which excepts these controls for EVERY workload; scope it, or declare the scope with the %s: %s annotation",
			clusterWideAnnotation, clusterWideDeclared)
	case len(match) > 0 && clusterWide:
		return nil, cseErrorf(path, name,
			"declares %s: %s but also sets spec.match; the marker would claim a scope the exception does not have",
			clusterWideAnnotation, clusterWideDeclared)
	}

	resources, err := resolveMatch(match, path, name)
	if err != nil {
		return nil, err
	}

	result := &policy{
		Name:            name,
		PolicyType:      "postureExceptionPolicy",
		Actions:         []string{"disable"},
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
	// The file is only ever read, so a close error says nothing about whether
	// the documents below were decoded correctly — discard it explicitly rather
	// than leaving it unchecked.
	defer func() { _ = file.Close() }()

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

// main converts the configured exception directory and writes the exceptions.
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
