package main

import (
	"encoding/json"
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

func fixedNativeUpdatePolicy(b []byte) bool {
	var config struct {
		IgnorePaths         []string `json:"ignorePaths"`
		VulnerabilityAlerts struct {
			Enabled bool `json:"enabled"`
		} `json:"vulnerabilityAlerts"`
	}
	if json.Unmarshal(b, &config) != nil || !config.VulnerabilityAlerts.Enabled {
		return false
	}
	const target = "scripts/retire-backstage-stateless/poller-native/go.mod"
	count := 0
	for _, path := range config.IgnorePaths {
		if strings.ContainsAny(path, "*?![]{}") || strings.HasPrefix(path, "/") {
			return false
		}
		if path == target {
			count++
		}
	}
	return count == 1
}

func TestNativeGraphUpdaterPolicyIsNarrowAndComplete(t *testing.T) {
	b, err := os.ReadFile("../../../.github/renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	if !fixedNativeUpdatePolicy(b) {
		t.Fatal("independent updates can change the fixed native conformance graph")
	}
	const good = `{"vulnerabilityAlerts":{"enabled":true},"ignorePaths":["scripts/retire-backstage-stateless/poller-native/go.mod"]}`
	if !fixedNativeUpdatePolicy([]byte(good)) {
		t.Fatal("correct scoped policy rejected")
	}
	for _, bad := range []string{
		strings.Replace(good, `"enabled":true`, `"enabled":false`, 1),
		strings.Replace(good, `scripts/retire-backstage-stateless/poller-native/go.mod`, `**/go.mod`, 1),
		strings.Replace(good, `scripts/retire-backstage-stateless/poller-native/go.mod`, `scripts/render-helm-chart/go.mod`, 1),
		`{"vulnerabilityAlerts":{"enabled":true},"packageRules":[{"matchManagers":["gomod"],"matchFileNames":["scripts/retire-backstage-stateless/poller-native/go.mod"],"enabled":false}]}`,
	} {
		if fixedNativeUpdatePolicy([]byte(bad)) {
			t.Fatal("ineffective or overly broad policy accepted")
		}
	}
}

func publishedLocks(t *testing.T) ([]byte, []byte) {
	t.Helper()
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod = []byte(strings.ReplaceAll(strings.ReplaceAll(string(mod), "golang.org/x/sync v0.22.0", "golang.org/x/sync v0.20.0"), "oras.land/oras-go/v2 v2.6.2", "oras.land/oras-go/v2 v2.6.0"))
	sums, err := os.ReadFile("go.sum")
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(sums), "\n") {
		if !strings.HasPrefix(line, "golang.org/x/sync v0.22.0") && !strings.HasPrefix(line, "oras.land/oras-go/v2 v2.6.2") {
			kept = append(kept, line)
		}
	}
	return mod, []byte(strings.Join(kept, "\n"))
}

func publishedBuild(t *testing.T, sums []byte) *debug.BuildInfo {
	t.Helper()
	checksum := func(path, version string) string {
		for _, line := range strings.Split(string(sums), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == path && f[1] == version {
				return f[2]
			}
		}
		t.Fatalf("missing published checksum %s %s", path, version)
		return ""
	}
	info := &debug.BuildInfo{}
	for path, version := range map[string]string{
		"github.com/fluxcd/helm-controller": "v1.5.5", "github.com/fluxcd/helm-controller/api": "v1.5.5",
		"github.com/fluxcd/pkg/ssa": "v0.67.6", "helm.sh/helm/v4": "v4.2.0",
		"oras.land/oras-go/v2": "v2.6.0", "golang.org/x/sync": "v0.20.0",
	} {
		info.Deps = append(info.Deps, &debug.Module{Path: path, Version: version, Sum: checksum(path, version)})
	}
	info.Deps = append(info.Deps, &debug.Module{Path: "sigs.k8s.io/kustomize/api", Version: "v0.21.1", Replace: &debug.Module{Path: "sigs.k8s.io/kustomize/api", Version: "v0.20.1", Sum: checksum("sigs.k8s.io/kustomize/api", "v0.20.1")}})
	return info
}

func TestPublishedNativeGraphAcceptsIntentionalProducerReplacement(t *testing.T) {
	mod, sums := publishedLocks(t)
	if err := validateNativeGraph(publishedBuild(t, sums), mod, sums); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGraphRejectsIndependentDeclaredAndLinkedChanges(t *testing.T) {
	for _, change := range []string{"declared indirect", "checksum lock", "linked indirect", "linked checksum", "new dependency", "unexpected replacement", "replacement drift", "missing producer", "duplicate dependency", "missing build info"} {
		t.Run(change, func(t *testing.T) {
			mod, sums := publishedLocks(t)
			info := publishedBuild(t, sums)
			find := func(path string) *debug.Module {
				for _, d := range info.Deps {
					if d.Path == path {
						return d
					}
				}
				t.Fatal("fixture missing")
				return nil
			}
			switch change {
			case "declared indirect":
				mod = []byte(strings.ReplaceAll(string(mod), "oras.land/oras-go/v2 v2.6.0", "oras.land/oras-go/v2 v2.6.2"))
			case "checksum lock":
				sums = append(sums, '\n')
			case "linked indirect":
				find("golang.org/x/sync").Version = "v0.22.0"
			case "linked checksum":
				find("golang.org/x/sync").Sum = "h1:foreign"
			case "new dependency":
				info.Deps = append(info.Deps, &debug.Module{Path: "example.invalid/extra", Version: "v1.0.0"})
			case "unexpected replacement":
				find("oras.land/oras-go/v2").Replace = &debug.Module{Path: "example.invalid/oras", Version: "v2.6.0"}
			case "replacement drift":
				find("sigs.k8s.io/kustomize/api").Replace.Version = "v0.21.2"
			case "missing producer":
				find("github.com/fluxcd/helm-controller").Path = "example.invalid/controller"
			case "duplicate dependency":
				info.Deps = append(info.Deps, find("golang.org/x/sync"))
			case "missing build info":
				info = nil
			}
			if err := validateNativeGraph(info, mod, sums); err == nil {
				t.Fatal("changed native graph was accepted")
			}
		})
	}
}

func TestCommittedNativeLocksStayOnPublishedGraph(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile("go.sum")
	if err != nil {
		t.Fatal(err)
	}
	_, published := publishedLocks(t)
	if err := validateNativeGraph(publishedBuild(t, published), mod, sums); err != nil {
		t.Fatal(err)
	}
}
