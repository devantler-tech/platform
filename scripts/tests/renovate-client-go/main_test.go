// Package renovateclientgo checks that Renovate only offers the modern v0.x
// family of k8s.io/client-go to this repository's Go modules (#5813).
//
// client-go also carries legacy v1.x to v12.x tags from before Kubernetes
// moved its libraries to v0.x. Go reports the higher ones as
// v11.0.0+incompatible (or v12.0.0 when read from the source tags), which
// outranks every v0.x release, so without a rule
// Renovate proposes it as a major update of the modern client.
package renovateclientgo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const clientGo = "k8s.io/client-go"

var (
	modern = []string{"v0.36.4", "v0.37.2", "v0.38.0", "v0.40.0-alpha.1"}
	legacy = []string{"v12.0.0", "v11.0.0+incompatible", "v10.0.0+incompatible", "v2.0.0+incompatible", "v1.5.2", "v1.4.0"}
)

type packageRule struct {
	MatchManagers     []string `json:"matchManagers"`
	MatchPackageNames []string `json:"matchPackageNames"`
	MatchUpdateTypes  []string `json:"matchUpdateTypes"`
	MatchFileNames    []string `json:"matchFileNames"`
	AllowedVersions   string   `json:"allowedVersions"`
	Enabled           *bool    `json:"enabled"`
}

func readRules(t *testing.T) []packageRule {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", ".github", "renovate.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var config struct {
		PackageRules []packageRule `json:"packageRules"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return config.PackageRules
}

// problems lists every way the rules fail to keep client-go on v0.x. The rule
// must be narrow: the gomod manager, client-go alone, every module and update
// type, and it must not switch updates off.
func problems(rules []packageRule) []string {
	var found []packageRule
	for _, rule := range rules {
		if slices.Contains(rule.MatchPackageNames, clientGo) {
			found = append(found, rule)
		}
	}
	if len(found) != 1 {
		return []string{fmt.Sprintf("want exactly one Renovate rule naming %s, found %d", clientGo, len(found))}
	}
	rule := found[0]
	var out []string
	if !slices.Equal(rule.MatchManagers, []string{"gomod"}) {
		out = append(out, fmt.Sprintf("the rule must match only the gomod manager, got %v", rule.MatchManagers))
	}
	if !slices.Equal(rule.MatchPackageNames, []string{clientGo}) {
		out = append(out, fmt.Sprintf("the rule must name only %s, got %v", clientGo, rule.MatchPackageNames))
	}
	if len(rule.MatchUpdateTypes) != 0 || len(rule.MatchFileNames) != 0 {
		out = append(out, fmt.Sprintf("the rule must cover every update type and module, got types %v files %v", rule.MatchUpdateTypes, rule.MatchFileNames))
	}
	if rule.Enabled != nil && !*rule.Enabled {
		out = append(out, "the rule must not disable updates")
	}
	value := rule.AllowedVersions
	if len(value) < 3 || !strings.HasPrefix(value, "/") || !strings.HasSuffix(value, "/") {
		return append(out, fmt.Sprintf("allowedVersions must be a /regex/, got %q", value))
	}
	pattern, err := regexp.Compile(value[1 : len(value)-1])
	if err != nil {
		return append(out, fmt.Sprintf("allowedVersions %q does not compile: %v", value, err))
	}
	// Renovate may compare with or without the leading v; both must agree.
	for _, version := range modern {
		for _, form := range []string{version, strings.TrimPrefix(version, "v")} {
			if !pattern.MatchString(form) {
				out = append(out, fmt.Sprintf("modern client-go %s must stay eligible", form))
			}
		}
	}
	for _, version := range legacy {
		for _, form := range []string{version, strings.TrimPrefix(version, "v")} {
			if pattern.MatchString(form) {
				out = append(out, fmt.Sprintf("legacy client-go %s must be rejected", form))
			}
		}
	}
	return out
}

func TestRenovateOffersOnlyModernClientGo(t *testing.T) {
	for _, problem := range problems(readRules(t)) {
		t.Error(problem)
	}
}

// Each unsafe variant of the committed rule must be caught for its own reason.
func TestCheckRejectsUnsafeRules(t *testing.T) {
	rules := readRules(t)
	index := slices.IndexFunc(rules, func(rule packageRule) bool {
		return slices.Contains(rule.MatchPackageNames, clientGo)
	})
	if index < 0 {
		t.Fatalf("no Renovate rule names %s", clientGo)
	}
	disabled := false
	cases := map[string]struct {
		edit   func(*packageRule)
		reason string
	}{
		"missing rule":     {func(r *packageRule) { r.MatchPackageNames = []string{"example.com/other"} }, "found 0"},
		"admits legacy":    {func(r *packageRule) { r.AllowedVersions = "/^v?[0-9]+\\./" }, "legacy client-go v11.0.0+incompatible"},
		"drops modern":     {func(r *packageRule) { r.AllowedVersions = "/^v?0\\.36\\./" }, "modern client-go v0.37.2"},
		"semver range":     {func(r *packageRule) { r.AllowedVersions = "<1.0.0" }, "must be a /regex/"},
		"disables updates": {func(r *packageRule) { r.Enabled = &disabled }, "must not disable"},
		"one module only":  {func(r *packageRule) { r.MatchFileNames = []string{"go.mod"} }, "every update type and module"},
		"major only":       {func(r *packageRule) { r.MatchUpdateTypes = []string{"major"} }, "every update type and module"},
		"every manager":    {func(r *packageRule) { r.MatchManagers = nil }, "only the gomod manager"},
		"broad packages":   {func(r *packageRule) { r.MatchPackageNames = []string{clientGo, "k8s.io/api"} }, "must name only"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			edited := slices.Clone(rules)
			rule := edited[index]
			rule.MatchPackageNames = slices.Clone(rule.MatchPackageNames)
			tc.edit(&rule)
			edited[index] = rule
			got := strings.Join(problems(edited), "\n")
			if !strings.Contains(got, tc.reason) {
				t.Fatalf("want a problem containing %q, got %q", tc.reason, got)
			}
		})
	}
}
