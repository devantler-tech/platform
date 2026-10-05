package arcstaging_test

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

const analysisRepository = "ghcr.io/devantler-tech/platform-ksail-analysis-runner"
const analysisPublisher = "https://github.com/devantler-tech/platform/.github/workflows/publish-ksail-analysis-runner.yaml@refs/heads/main"
const githubIssuer = "https://token.actions.githubusercontent.com"

func TestAnalysisImageCanBePublishedOnlyFromMain(t *testing.T) {
	workflow := readYAML(t, ".github/workflows/publish-ksail-analysis-runner.yaml")
	equal(t, len(field(t, workflow, "permissions").(map[string]any)), 0)
	publish := field(t, workflow, "jobs", "publish")
	equal(t, field(t, publish, "if"), "github.event_name == 'push' && github.ref == 'refs/heads/main'")
	equal(t, field(t, publish, "needs"), "verify")
	equal(t, field(t, publish, "permissions", "contents"), "read")
	equal(t, field(t, publish, "permissions", "packages"), "write")
	equal(t, field(t, publish, "permissions", "id-token"), "write")
	equal(t, field(t, workflow, "env", "IMAGE"), analysisRepository)
	equal(t, field(t, workflow, "jobs", "verify", "permissions", "contents"), "read")
	equal(t, len(field(t, workflow, "jobs", "verify", "permissions").(map[string]any)), 1)
	for _, job := range field(t, workflow, "jobs").(map[string]any) {
		for _, step := range field(t, job, "steps").([]any) {
			mapping := step.(map[string]any)
			if uses, ok := mapping["uses"].(string); ok {
				if !regexp.MustCompile(`@[0-9a-f]{40}$`).MatchString(uses) {
					t.Fatalf("action must be immutable: %q", uses)
				}
			}
		}
	}
}

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
						strings.Replace(analysisPublisher, "platform/", "other/", 1),
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
