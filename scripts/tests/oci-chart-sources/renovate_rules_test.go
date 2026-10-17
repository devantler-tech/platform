package ocichartsources

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// renovateDependency is what Renovate knows about one extracted dependency
// when it decides which package rules apply to it.
type renovateDependency struct {
	packageName, manager, datasource, packageFile string
}

// renovateUpdateTypes are the update types a package rule can select. The
// guard checks every one, so a rule limited to one of them is still judged.
var renovateUpdateTypes = []string{
	"major", "minor", "patch", "pin", "digest", "pinDigest",
	"lockFileMaintenance", "rollback", "bump", "replacement",
}

// ruleMatch is three-valued: a filter this guard cannot decide is a mayMatch,
// never a guess in either direction.
type ruleMatch int

const (
	noMatch ruleMatch = iota
	mayMatch
	matches
)

func negate(m ruleMatch) ruleMatch { return matches - m }

// notSetHere stands for whatever the presets, Renovate's defaults or the
// update-type blocks would supply. Only a rule in this file that certainly
// matches removes it from the possible values.
const notSetHere = "<not set by a rule in this file>"

// effectiveValues returns every value setting may take for an update of dep.
// Renovate applies package rules in order, after the update-type blocks, with
// a preset's rules before this file's, and the last matching rule that sets a
// key wins. A rule
// that certainly matches replaces the possible values; one that only may
// match adds its value to them. The setting is safe only when every possible
// value is the required one.
func effectiveValues(rules []map[string]any, dep renovateDependency, updateType, setting string) []string {
	possible := map[string]bool{notSetHere: true}
	for _, rule := range rules {
		match := ruleMatches(rule, dep, updateType)
		if match == noMatch {
			continue
		}
		value, ok := settingValue(rule, setting)
		if !ok {
			continue
		}
		if match == matches {
			clear(possible)
		}
		possible[value] = true
	}
	values := make([]string, 0, len(possible))
	for value := range possible {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// settingValue is the rule's own value for setting, or notSetHere when the
// rule extends a preset that may set it.
func settingValue(rule map[string]any, setting string) (string, bool) {
	if value, ok := rule[setting]; ok {
		return fmt.Sprintf("%#v", value), true
	}
	if _, ok := rule["extends"]; ok {
		return notSetHere, true
	}
	return "", false
}

// ruleMatches combines a rule's filters: all of them must match. Every key
// that begins with match or exclude is a filter, and only the filters listed
// here are decided; any other is a mayMatch.
func ruleMatches(rule map[string]any, dep renovateDependency, updateType string) ruleMatch {
	result := matches
	for key, filter := range rule {
		if !strings.HasPrefix(key, "match") && !strings.HasPrefix(key, "exclude") {
			continue
		}
		switch key {
		case "matchPackageNames", "matchDepNames":
			result = min(result, patternList(filter, dep.packageName))
		case "matchFileNames":
			result = min(result, patternList(filter, dep.packageFile))
		case "matchManagers":
			result = min(result, literalList(filter, dep.manager))
		case "matchDatasources":
			result = min(result, literalList(filter, dep.datasource))
		case "matchUpdateTypes":
			result = min(result, literalList(filter, updateType))
		default:
			result = min(result, mayMatch)
		}
	}
	return result
}

// literalList decides a list of plain names. A pattern, or a name that differs
// only in case, is a mayMatch. So is the legacy manager name "regex": whether
// it still selects the custom manager depends on Renovate's config migration.
func literalList(filter any, value string) ruleMatch {
	entries, ok := filter.([]any)
	if !ok || len(entries) == 0 {
		return mayMatch
	}
	result := noMatch
	for _, raw := range entries {
		entry, ok := raw.(string)
		switch {
		case !ok || strings.ContainsAny(entry, "*?[]{}()!/\\") || entry == "regex" ||
			(entry != value && strings.EqualFold(entry, value)):
			result = max(result, mayMatch)
		case entry == value:
			result = matches
		}
	}
	return result
}

// patternList follows Renovate's string pattern matching: at least one
// positive pattern must match when any is given, and no negated (!) pattern
// may match.
func patternList(filter any, value string) ruleMatch {
	entries, ok := filter.([]any)
	if !ok || len(entries) == 0 {
		return mayMatch
	}
	positive, negative, hasPositive := noMatch, matches, false
	for _, raw := range entries {
		entry, ok := raw.(string)
		if !ok {
			return mayMatch
		}
		if pattern, negated := strings.CutPrefix(entry, "!"); negated {
			negative = min(negative, negate(patternMatch(pattern, value)))
		} else {
			hasPositive = true
			positive = max(positive, patternMatch(entry, value))
		}
	}
	if !hasPositive {
		positive = matches
	}
	return min(positive, negative)
}

var regexPattern = regexp.MustCompile(`^/(.+)/([a-z]*)$`)

// patternMatch decides one /regex/ or glob pattern. A glob is compared twice:
// a strict reading that matches less than minimatch does and a permissive one
// that matches more, so only a pattern both readings agree on is decided.
func patternMatch(pattern, value string) ruleMatch {
	if parts := regexPattern.FindStringSubmatch(pattern); parts != nil {
		if strings.Trim(parts[2], "i") != "" {
			return mayMatch
		}
		expression := parts[1]
		if parts[2] != "" {
			expression = "(?i)" + expression
		}
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return mayMatch
		}
		if compiled.MatchString(value) {
			return matches
		}
		return noMatch
	}
	if strings.ContainsAny(pattern, "[]{}()!\\") || strings.HasPrefix(pattern, "#") {
		return mayMatch
	}
	strict, permissive := globExpressions(pattern)
	switch {
	case strict.MatchString(value):
		return matches
	case !permissive.MatchString(value):
		return noMatch
	default:
		return mayMatch
	}
}

// globExpressions translates a glob of literals, *, ** and ? into regular
// expressions. strict is case-sensitive and matches a ** segment only to one
// or more whole segments; permissive ignores case, lets * and ** cross path
// separators and makes every separator optional.
func globExpressions(glob string) (strict, permissive *regexp.Regexp) {
	var s, p strings.Builder
	for i, segment := range strings.Split(glob, "/") {
		if i > 0 {
			s.WriteString("/")
		}
		if strings.Trim(segment, "*") == "" && segment != "" {
			if segment == "**" {
				s.WriteString(`[^/]+(?:/[^/]+)*`)
			} else {
				s.WriteString(`[^/]+`)
			}
		} else {
			for _, r := range segment {
				switch r {
				case '*':
					s.WriteString(`[^/]*`)
				case '?':
					s.WriteString(`[^/]`)
				default:
					s.WriteString(regexp.QuoteMeta(string(r)))
				}
			}
		}
	}
	for _, r := range glob {
		switch r {
		case '*':
			p.WriteString(`.*`)
		case '?':
			p.WriteString(`.`)
		case '/':
			p.WriteString(`/*`)
		default:
			p.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile("^" + s.String() + "$"), regexp.MustCompile("(?i)^" + p.String() + "$")
}

// checkUpdatePolicy fails unless every update of dep, of any type, certainly
// resolves to each required setting.
func checkUpdatePolicy(rules []map[string]any, dep renovateDependency, required map[string]any) error {
	settings := make([]string, 0, len(required))
	for setting := range required {
		settings = append(settings, setting)
	}
	sort.Strings(settings)
	for _, updateType := range renovateUpdateTypes {
		for _, setting := range settings {
			want := fmt.Sprintf("%#v", required[setting])
			if got := effectiveValues(rules, dep, updateType, setting); !slices.Equal(got, []string{want}) {
				return fmt.Errorf("%s updates of %s: %s may resolve to %v, want only %s",
					updateType, dep.packageName, setting, got, want)
			}
		}
	}
	return nil
}

// The guard must fail whenever a rule in this file can still change either
// setting after the exemption, including a rule that matches the chart while
// a later rule that does not match it sets the right value.
func TestUpdatePolicyGuardRejectsMaskedSettings(t *testing.T) {
	config, dep := fluxOperatorRenovateConfig(t)
	required := fluxOperatorUpdatePolicy
	exemption := slices.IndexFunc(config.PackageRules, func(rule map[string]any) bool {
		names, _ := rule["matchPackageNames"].([]any)
		return len(names) == 1 && names[0] == dep.packageName
	})
	if exemption < 0 {
		t.Fatal("cannot find the flux-operator exemption rule")
	}
	masking := slices.IndexFunc(config.PackageRules[exemption+1:], func(rule map[string]any) bool {
		return rule["automerge"] == false && ruleMatches(rule, dep, "patch") == noMatch
	})
	if masking < 0 {
		t.Fatal("the controls need a later rule that sets automerge to false and does not match the chart")
	}
	afterExemption := func(rule map[string]any) []map[string]any {
		rules := slices.Clone(config.PackageRules)
		return slices.Insert(rules, exemption+1, rule)
	}
	cases := []struct {
		name    string
		rules   []map[string]any
		failure string
	}{
		{"glob over the chart's registry turns automerge on",
			afterExemption(map[string]any{"matchPackageNames": []any{"ghcr.io/controlplaneio-fluxcd/**"}, "automerge": true}), "automerge"},
		{"rule for the chart's patch updates turns automerge on",
			afterExemption(map[string]any{"matchPackageNames": []any{dep.packageName}, "matchUpdateTypes": []any{"patch"}, "automerge": true}), "patch updates"},
		{"regular expression turns automerge on",
			afterExemption(map[string]any{"matchPackageNames": []any{"/flux-operator$/"}, "automerge": true}), "automerge"},
		{"datasource rule restores the release-age wait",
			afterExemption(map[string]any{"matchDatasources": []any{"docker"}, "minimumReleaseAge": "7 days"}), "minimumReleaseAge"},
		{"filter the guard cannot decide turns automerge on",
			afterExemption(map[string]any{"matchCurrentVersion": "<1.0.0", "automerge": true}), "automerge"},
		{"preset extended by a matching rule",
			afterExemption(map[string]any{"matchPackageNames": []any{dep.packageName}, "extends": []any{"schedule:weekly"}}), notSetHere},
		{"exemption removed",
			slices.Delete(slices.Clone(config.PackageRules), exemption, exemption+1), "automerge"},
		{"rule for another registry",
			afterExemption(map[string]any{"matchPackageNames": []any{"quay.io/kubevirt/**"}, "automerge": true}), ""},
		{"negated pattern excludes the chart",
			afterExemption(map[string]any{"matchPackageNames": []any{"ghcr.io/**", "!ghcr.io/controlplaneio-fluxcd/**"}, "automerge": true}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUpdatePolicy(tc.rules, dep, required)
			switch {
			case tc.failure == "" && err != nil:
				t.Fatalf("a rule that cannot match the chart was rejected: %v", err)
			case tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)):
				t.Fatalf("guard did not reject the change for its intended reason (%s): %v", tc.failure, err)
			}
		})
	}
}
