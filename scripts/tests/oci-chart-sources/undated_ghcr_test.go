package ocichartsources

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// undatedGHCRDependencies get no release date from Renovate, like the
// flux-operator chart, so the repository-wide minimumReleaseAge never lets an
// update of them through (#5008). Each row says how Renovate extracts the
// dependency, so the policy check judges the rules that really apply to it.
var undatedGHCRDependencies = []renovateDependency{
	{"ghcr.io/m0nsterrr/helm-charts/homepage", "flux", "docker", "k8s/bases/apps/homepage/oci-repository.yaml"},
	{"ghcr.io/cloudflare/origin-ca-issuer-charts/origin-ca-issuer", "flux", "docker", "k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/oci-repository.yaml"},
	{"ghcr.io/cloudnative-pg/postgresql", "custom.regex", "docker", "k8s/providers/hetzner/infrastructure/coroot/cluster.yaml"},
	{"ghcr.io/cloudnative-pg/postgresql", "custom.regex", "docker", "k8s/bases/apps/umami/cluster.yaml"},
	{"ghcr.io/coroot/coroot", "dockerfile", "docker", "k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile"},
	{"ghcr.io/coroot/coroot-cluster-agent", "dockerfile", "docker", "k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile"},
	{"ghcr.io/umami-software/umami", "kubernetes", "docker", "k8s/bases/apps/umami/cron-job.yaml"},
}

// Every ordinary update is proposed (no release-age wait) and none merges
// unreviewed. Vulnerability-fix PRs keep the repository-wide vulnerabilityAlerts
// fast path, which Renovate applies after package rules and checkUpdatePolicy
// deliberately does not model.
var undatedGHCRUpdatePolicy = map[string]any{"automerge": false, "minimumReleaseAge": "0 days"}

func TestUndatedGHCRDependenciesAreProposedAndReviewed(t *testing.T) {
	root := repositoryRoot(t)
	config := readRenovateConfig(t)
	for _, dep := range undatedGHCRDependencies {
		t.Run(dep.packageName+" in "+dep.packageFile, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join(root, dep.packageFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := extractedAs(config, dep, string(text)); err != nil {
				t.Fatal(err)
			}
			if err := checkUpdatePolicy(config.PackageRules, dep, undatedGHCRUpdatePolicy); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Without the exemption the wait applies again, and an extraction the row does
// not describe is not accepted as evidence for it.
func TestUndatedGHCRGuardRejectsMissingExemptionAndWrongManager(t *testing.T) {
	root := repositoryRoot(t)
	config := readRenovateConfig(t)
	exemption := slices.IndexFunc(config.PackageRules, func(rule map[string]any) bool {
		names, _ := rule["matchPackageNames"].([]any)
		return slices.Contains(names, any(undatedGHCRDependencies[0].packageName))
	})
	if exemption < 0 {
		t.Fatal("cannot find the undated ghcr.io exemption rule")
	}
	withoutExemption := slices.Delete(slices.Clone(config.PackageRules), exemption, exemption+1)
	for _, dep := range undatedGHCRDependencies {
		for setting, want := range undatedGHCRUpdatePolicy {
			err := checkUpdatePolicy(withoutExemption, dep, map[string]any{setting: want})
			if err == nil || !strings.Contains(err.Error(), setting) {
				t.Fatalf("%s: removing the exemption was not caught by the %s check: %v", dep.packageName, setting, err)
			}
		}
	}
	for _, tc := range []struct {
		name string
		dep  renovateDependency
	}{
		{"chart read by the kubernetes manager",
			renovateDependency{"ghcr.io/m0nsterrr/helm-charts/homepage", "kubernetes", "docker", "k8s/bases/apps/homepage/oci-repository.yaml"}},
		{"image read by the flux manager",
			renovateDependency{"ghcr.io/umami-software/umami", "flux", "docker", "k8s/bases/apps/umami/cron-job.yaml"}},
		{"name that is only a prefix of the pinned image",
			renovateDependency{"ghcr.io/coroot/coroot-cluster", "dockerfile", "docker", "k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile"}},
		{"custom manager that does not read the file",
			renovateDependency{"ghcr.io/cloudnative-pg/postgresql", "custom.regex", "docker", "k8s/bases/apps/umami/cron-job.yaml"}},
		{"wrong datasource",
			renovateDependency{"ghcr.io/coroot/coroot", "dockerfile", "github-releases", "k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join(root, tc.dep.packageFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := extractedAs(config, tc.dep, string(text)); err == nil {
				t.Fatal("an extraction the committed config does not perform was accepted")
			}
		})
	}
}

// extractedAs fails unless the committed config makes dep.manager read
// dep.packageFile and extract dep.packageName from it with dep.datasource.
func extractedAs(config renovateConfig, dep renovateDependency, text string) error {
	readsFile := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(pattern string) bool {
			return patternMatch(pattern, dep.packageFile) == matches
		})
	}
	switch dep.manager {
	case "dockerfile":
		// Renovate's documented defaults include prefix.Dockerfile. Model the
		// fixed, tagged FROM references used by these rows, not ARG expansion.
		// https://docs.renovatebot.com/modules/manager/dockerfile/
		patterns := []string{
			`/(^|/|\.)([Dd]ocker|[Cc]ontainer)file$/`,
			`/(^|/)([Dd]ocker|[Cc]ontainer)file[^/]*$/`,
		}
		patterns = append(patterns, config.Dockerfile.ManagerFilePatterns...)
		if (config.Dockerfile.Enabled != nil && !*config.Dockerfile.Enabled) ||
			(len(config.EnabledManagers) > 0 && !slices.Contains(config.EnabledManagers, dep.manager)) ||
			!readsFile(patterns) || (len(config.IncludePaths) > 0 && !readsFile(config.IncludePaths)) {
			return fmt.Errorf("the dockerfile manager does not read %s", dep.packageFile)
		}
		for _, pattern := range config.IgnorePaths {
			if patternMatch(pattern, dep.packageFile) != noMatch {
				return fmt.Errorf("the dockerfile source is ignored or undecidable: %s", dep.packageFile)
			}
		}
		image := regexp.MustCompile(`(?im)^\s*FROM\s+(?:--platform=\S+\s+)?` + regexp.QuoteMeta(dep.packageName) +
			`:[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?(?:@sha256:[0-9a-f]{64})?(?:\s+AS\s+[A-Za-z0-9_-]+)?\s*$`)
		if !image.MatchString(text) || dep.datasource != "docker" {
			return fmt.Errorf("the dockerfile manager does not extract a fixed tagged FROM for %s from %s", dep.packageName, dep.packageFile)
		}
		return nil
	case "flux":
		if !readsFile(config.Flux.ManagerFilePatterns) {
			return fmt.Errorf("the flux manager does not read %s", dep.packageFile)
		}
		var source map[string]any
		if err := yaml.Unmarshal([]byte(text), &source); err != nil {
			return err
		}
		tag, _ := object(object(source, "spec"), "ref")["tag"].(string)
		if source["kind"] != "OCIRepository" || object(source, "spec")["url"] != "oci://"+dep.packageName || tag == "" ||
			dep.datasource != "docker" {
			return fmt.Errorf("%s is not a tagged OCIRepository for %s", dep.packageFile, dep.packageName)
		}
		return nil
	case "kubernetes":
		image := regexp.MustCompile(`(?m)^\s*-?\s*image:\s*['"]?` + regexp.QuoteMeta(dep.packageName) + `[:@]\S`)
		if !readsFile(config.Kubernetes.ManagerFilePatterns) || !image.MatchString(text) || dep.datasource != "docker" {
			return fmt.Errorf("the kubernetes manager does not extract %s from %s", dep.packageName, dep.packageFile)
		}
		return nil
	case "custom.regex":
		count := 0
		for _, manager := range config.CustomManagers {
			if manager.CustomType != "regex" || !readsFile(manager.ManagerFilePatterns) {
				continue
			}
			for _, raw := range manager.MatchStrings {
				matcher, err := regexp.Compile(raw)
				if err != nil {
					return fmt.Errorf("cannot judge matchString %q: %w", raw, err)
				}
				for _, match := range matcher.FindAllStringSubmatch(text, -1) {
					field := func(group, template string) string {
						if index := matcher.SubexpIndex(group); index > 0 {
							return match[index]
						}
						return template
					}
					if field("depName", manager.DepNameTemplate) == dep.packageName &&
						field("datasource", manager.DatasourceTemplate) == dep.datasource &&
						field("currentValue", "") != "" && manager.PackageNameTemplate == "" {
						count++
					}
				}
			}
		}
		if count == 0 {
			return fmt.Errorf("no regex manager extracts %s from %s with the %s datasource",
				dep.packageName, dep.packageFile, dep.datasource)
		}
		return nil
	}
	return fmt.Errorf("manager %q is not modelled", dep.manager)
}

// A base-image dependency must not disappear when its manager or file is
// excluded, or when its real FROM is replaced by an inert textual mention.
func TestUndatedGHCRDockerfileGuardRejectsLostDiscovery(t *testing.T) {
	dep := undatedGHCRDependencies[4]
	body, err := os.ReadFile(filepath.Join(repositoryRoot(t), dep.packageFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"disabled", "other-managers-only", "ignored", "excluded", "wrong-filename", "commented-from", "unresolved-version"} {
		t.Run(mutation, func(t *testing.T) {
			config, candidate, text := readRenovateConfig(t), dep, string(body)
			if err := extractedAs(config, candidate, text); err != nil {
				t.Fatalf("positive control: %v", err)
			}
			switch mutation {
			case "disabled":
				enabled := false
				config.Dockerfile.Enabled = &enabled
			case "other-managers-only":
				config.EnabledManagers = []string{"custom.regex"}
			case "ignored":
				config.IgnorePaths = []string{dep.packageFile}
			case "excluded":
				config.IncludePaths = []string{"unrelated/**"}
			case "wrong-filename":
				candidate.packageFile += ".txt"
			case "commented-from":
				text = strings.Replace(text, "FROM "+dep.packageName+":", "# FROM "+dep.packageName+":", 1)
			case "unresolved-version":
				reference := regexp.MustCompile(regexp.QuoteMeta(dep.packageName) + `:[^\s@]+`)
				text = reference.ReplaceAllLiteralString(text, dep.packageName+":${UNRESOLVED_VERSION}")
			}
			if err := extractedAs(config, candidate, text); err == nil {
				t.Fatal("accepted lost dependency discovery")
			}
		})
	}
}
