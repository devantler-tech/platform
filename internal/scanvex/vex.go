// Package scanvex serializes proven native VEX facts and refuses any native
// suppression outside the complete advisory, version and source occurrence set.
package scanvex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Scope admits only the exact component occurrence proven by its caller.
type Scope struct {
	IDs                            []string
	Module, Version, Justification string
	Paths                          []string
}

func allowed(scopes []Scope, id, module, version string, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, scope := range scopes {
		if !slices.Contains(scope.IDs, id) || scope.Module != module || scope.Version != version {
			continue
		}
		valid := true
		for _, path := range paths {
			if !slices.Contains(scope.Paths, strings.TrimPrefix(path, "/")) {
				valid = false
			}
		}
		if valid {
			return true
		}
	}
	return false
}

// Write creates a versioned component document; callers must validate every
// suppressed native occurrence against these scopes after the complete scan.
func Write(path string, scopes []Scope) error {
	if len(scopes) == 0 || !filepath.IsAbs(path) {
		return fmt.Errorf("a private absolute VEX destination and proven scopes are required")
	}
	var statements []any
	for _, scope := range scopes {
		if len(scope.IDs) == 0 || len(scope.Paths) == 0 || scope.Module == "" || scope.Version == "" || scope.Justification != "vulnerable_code_not_in_execute_path" {
			return fmt.Errorf("incomplete or unsupported source applicability proof")
		}
		statements = append(statements, map[string]any{
			"vulnerability": map[string]any{"name": scope.IDs[0], "aliases": scope.IDs[1:]},
			"products":      []any{map[string]any{"@id": "pkg:golang/" + scope.Module + "@" + scope.Version}},
			"status":        "not_affected", "justification": scope.Justification,
			"impact_statement": "The complete source snapshot passed its advisory-specific native applicability proof; changed advisory scope, source occurrences or call paths refuse this scan.",
		})
	}
	identity, _ := json.Marshal(statements)
	document := map[string]any{"@context": "https://openvex.dev/ns/v0.2.0", "@id": fmt.Sprintf("https://github.com/devantler-tech/platform#ci-vex-%x", sha256.Sum256(identity)), "author": "https://github.com/devantler-tech/platform", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "version": 1, "statements": statements}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ValidateTrivy retains native findings and refuses a VEX match at an unproved
// source path, version, component or advisory, including duplicate identities.
func ValidateTrivy(data []byte, vexPath string, scopes []Scope) error {
	var report struct {
		Results []struct {
			Target                       string
			ExperimentalModifiedFindings []struct {
				Type, Status, Statement, Source string
				Finding                         struct{ ID, VulnerabilityID, PkgName, InstalledVersion string }
			}
		}
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Results) == 0 {
		return fmt.Errorf("native Trivy report has no examined targets")
	}
	for _, result := range report.Results {
		for _, finding := range result.ExperimentalModifiedFindings {
			// --show-suppressed also retains the pre-existing, explicitly supplied
			// native misconfiguration ignore file. Its frozen inputs and premises
			// are checked by the source/runtime guards; these are not VEX matches.
			if finding.Type == "misconfiguration" && finding.Status == "ignored" && finding.Source == ".trivyignore.yaml" && finding.Finding.ID != "" {
				continue
			}
			if finding.Type != "vulnerability" || finding.Status != "not_affected" || finding.Statement != "vulnerable_code_not_in_execute_path" || finding.Source != vexPath || !allowed(scopes, finding.Finding.VulnerabilityID, finding.Finding.PkgName, finding.Finding.InstalledVersion, []string{result.Target}) {
				return fmt.Errorf("native Trivy suppressed a finding outside the complete proven occurrence set")
			}
		}
	}
	return nil
}

type grypeMatch struct {
	Vulnerability struct {
		ID string `json:"id"`
	} `json:"vulnerability"`
	Artifact struct {
		Name, Version string
		Locations     []struct{ Path, AccessPath string }
	} `json:"artifact"`
	Rules []struct {
		Namespace     string `json:"namespace"`
		Status        string `json:"vex-status"`
		Vulnerability string `json:"vulnerability"`
	} `json:"appliedIgnoreRules"`
}

// ValidateGrype checks every suppressed location and returns all remaining
// native matches, including UNKNOWN findings below native severity thresholds.
func ValidateGrype(data []byte, scopes []Scope) (int, error) {
	var report struct {
		Descriptor struct{ Name, Version string }
		Source     struct{ Type string }
		Matches    []grypeMatch
		Ignored    []grypeMatch `json:"ignoredMatches"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Descriptor.Name != "grype" || report.Descriptor.Version != "0.118.0" || report.Source.Type != "directory" {
		return 0, fmt.Errorf("native Grype report identity or complete source is missing")
	}
	for _, match := range report.Ignored {
		var paths []string
		for _, location := range match.Artifact.Locations {
			if location.Path != "" {
				paths = append(paths, location.Path)
			}
			if location.AccessPath != "" {
				paths = append(paths, location.AccessPath)
			}
		}
		if len(match.Rules) != 1 {
			return 0, fmt.Errorf("native Grype ignore provenance changed")
		}
		rule := match.Rules[0]
		if rule.Namespace == "vex" {
			if rule.Status != "not_affected" || !allowed(scopes, match.Vulnerability.ID, match.Artifact.Name, match.Artifact.Version, paths) {
				return 0, fmt.Errorf("native Grype suppressed a finding outside the complete proven occurrence set")
			}
		} else if match.Vulnerability.ID != "CVE-2018-20225" || rule.Vulnerability != "CVE-2018-20225" {
			return 0, fmt.Errorf("native Grype used an unreviewed ignore rule")
		}
	}
	return len(report.Matches), nil
}
