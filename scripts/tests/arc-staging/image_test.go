package arcstaging_test

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

const analysisRepository = "ghcr.io/devantler-tech/ksail-analysis-runner"
const analysisPublisher = "https://github.com/devantler-tech/ksail/.github/workflows/publish-ksail-analysis-runner.yaml@refs/heads/main"
const githubIssuer = "https://token.actions.githubusercontent.com"

func TestAnalysisImageUsesItsOwnExactNodeSigner(t *testing.T) {
	policy := readYAML(t, "talos/cluster/verify-first-party-images.yaml")
	rules := field(t, policy, "rules").([]any)
	images := []string{
		analysisRepository, analysisRepository + ":revision",
		analysisRepository + "@sha256:" + strings.Repeat("a", 64),
		analysisRepository + "-other:revision",
		"ghcr.io/devantler-tech/platform-coroot-node-agent:revision",
		"ghcr.io/devantler-tech/wedding-app:revision",
	}
	for _, image := range images {
		t.Run(image, func(t *testing.T) {
			repository := strings.Split(strings.Split(image, "@")[0], ":")[0]
			for _, rule := range rules {
				pattern := field(t, rule, "image").(string)
				match, err := path.Match(pattern, repository)
				if err != nil {
					t.Fatal(err)
				}
				if !match {
					continue
				}
				equal(t, field(t, rule, "keyless", "issuer"), githubIssuer)
				regex := regexp.MustCompile(field(t, rule, "keyless", "subjectRegex").(string))
				equal(t, regex.MatchString(analysisPublisher), repository == analysisRepository)
				if repository == analysisRepository {
					for _, forbidden := range []string{
						strings.Replace(analysisPublisher, "refs/heads/main", "refs/pull/1/merge", 1),
						strings.Replace(analysisPublisher, "refs/heads/main", "refs/heads/other", 1),
						strings.Replace(analysisPublisher, "ksail/", "other/", 1),
						strings.Replace(analysisPublisher, "publish-ksail-analysis-runner", "publish-coroot-node-agent-hotfix", 1),
						analysisPublisher + ".other", "https://other.example/" + analysisPublisher,
					} {
						if regex.MatchString(forbidden) {
							t.Fatalf("unexpected signer admitted: %q", forbidden)
						}
					}
				}
				return
			}
			t.Fatal("no image verification rule matched")
		})
	}
}
