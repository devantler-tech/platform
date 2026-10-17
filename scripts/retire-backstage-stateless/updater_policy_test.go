package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Security updates ignore exclude-paths and the version-PR limit. Explicit
// all-version ignore is confined to this producer-coupled conformance fixture.
func nativeDependabotPolicy(b []byte) bool {
	var config struct {
		Version int `yaml:"version"`
		Updates []struct {
			Ecosystem    string   `yaml:"package-ecosystem"`
			Directory    string   `yaml:"directory"`
			Directories  []string `yaml:"directories"`
			TargetBranch string   `yaml:"target-branch"`
			Limit        *int     `yaml:"open-pull-requests-limit"`
			Schedule     struct {
				Interval string `yaml:"interval"`
			} `yaml:"schedule"`
			Ignore []struct {
				Name     string   `yaml:"dependency-name"`
				Versions []string `yaml:"versions"`
				Types    []string `yaml:"update-types"`
			} `yaml:"ignore"`
		} `yaml:"updates"`
	}
	if yaml.Unmarshal(b, &config) != nil || config.Version != 2 {
		return false
	}
	count := 0
	for _, job := range config.Updates {
		if job.Ecosystem != "gomod" || job.Directory != "/scripts/retire-backstage-stateless/poller-native" {
			continue
		}
		count++
		if len(job.Directories) != 0 || job.TargetBranch != "" || job.Limit == nil || *job.Limit != 0 || job.Schedule.Interval != "weekly" || len(job.Ignore) != 1 {
			return false
		}
		ignore := job.Ignore[0]
		if ignore.Name != "*" || !reflect.DeepEqual(ignore.Versions, []string{">= 0"}) || len(ignore.Types) != 0 {
			return false
		}
	}
	return count == 1
}

func TestNativeDependabotPolicyIncludesSecurityUpdatesWithoutGlobalDisable(t *testing.T) {
	b, err := os.ReadFile("../../.github/dependabot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !nativeDependabotPolicy(b) {
		t.Fatal("security updates can independently replace the native graph")
	}
	const good = `version: 2
updates:
  - package-ecosystem: gomod
    directory: /scripts/retire-backstage-stateless/poller-native
    schedule:
      interval: weekly
    open-pull-requests-limit: 0
    ignore:
      - dependency-name: "*"
        versions: [">= 0"]
`
	if !nativeDependabotPolicy([]byte(good)) {
		t.Fatal("scoped security-update policy rejected")
	}
	for _, bad := range []string{
		strings.Replace(good, "versions: [\">= 0\"]", "update-types: [version-update:semver-patch]", 1),
		strings.Replace(good, "versions: [\">= 0\"]", "versions: [\">= 1\"]", 1),
		strings.Replace(good, "    ignore:\n      - dependency-name: \"*\"\n        versions: [\">= 0\"]\n", "    exclude-paths: [\"**/*\"]\n", 1),
		strings.Replace(good, "directory: /scripts/retire-backstage-stateless/poller-native", "directory: /scripts", 1),
		strings.Replace(good, "    schedule:", "    target-branch: other\n    schedule:", 1),
	} {
		if bad == good {
			t.Fatal("policy control changed nothing")
		}
		if nativeDependabotPolicy([]byte(bad)) {
			t.Fatal("ineffective or overly broad security-update policy accepted")
		}
	}
}
