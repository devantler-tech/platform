package arcstaging_test

import (
	"encoding/json"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const mailRepository = "ghcr.io/devantler-tech/platform-private-mail-bootstrap"
const mailPublisher = "https://github.com/devantler-tech/platform/.github/workflows/publish-private-mail-bootstrap.yaml@refs/heads/main"

func mailField[T any](t *testing.T, value any, keys ...string) T {
	t.Helper()
	raw := field(t, value, keys...)
	result, ok := raw.(T)
	if !ok {
		t.Fatalf("mail integration field %v has unexpected type %T", keys, raw)
	}
	return result
}

func TestPrivateMailCredentialsHaveExactSeedAndReadAuthorization(t *testing.T) {
	args, _ := captureBaoWrite(t, "bao", "write", "auth/kubernetes/role/external-secrets")
	hasSeedPolicy := false
	for _, argument := range args {
		if value, ok := strings.CutPrefix(argument, "policies="); ok {
			hasSeedPolicy = slices.Contains(strings.Split(value, ","), "vault-seed-write")
		}
	}
	if !hasSeedPolicy {
		t.Fatal("credential synchronizer must receive the seed/read policy")
	}
	_, policy := captureBaoWrite(t, "bao", "policy", "write", "vault-seed-write")
	entries := regexp.MustCompile(`(?s)path\s+"(secret/(?:data|metadata)/apps/private-mail/[^\"]+)"\s*\{\s*capabilities\s*=\s*(\[[^\]]+\])\s*\}`).FindAllStringSubmatch(policy, -1)
	want := map[string]bool{}
	for _, kind := range []string{"data", "metadata"} {
		for _, account := range []string{"admin", "maintainer", "agent"} {
			want["secret/"+kind+"/apps/private-mail/"+account] = true
		}
	}
	if len(entries) != len(want) {
		t.Fatalf("need one exact data and metadata grant per mail credential, got %d", len(entries))
	}
	for _, entry := range entries {
		if !want[entry[1]] {
			t.Fatal("mail credential authorization is duplicated or broader than its three entries")
		}
		delete(want, entry[1])
		var capabilities []string
		if err := json.Unmarshal([]byte(entry[2]), &capabilities); err != nil {
			t.Fatal(err)
		}
		slices.Sort(capabilities)
		if !reflect.DeepEqual(capabilities, []string{"create", "read", "update"}) {
			t.Fatal("mail credential grant must seed and read without list or delete authority")
		}
	}
}

// Match the first node rule exactly as declared, including catch-all precedence.
func TestPrivateMailImageUsesItsOwnExactNodeSigner(t *testing.T) {
	rules := mailField[[]any](t, readYAML(t, "talos/cluster/verify-first-party-images.yaml"), "rules")
	for _, repository := range []string{mailRepository, mailRepository + "-other", "ghcr.io/devantler-tech/wedding-app"} {
		t.Run(repository, func(t *testing.T) {
			for _, rule := range rules {
				match, err := path.Match(mailField[string](t, rule, "image"), repository)
				if err != nil {
					t.Fatal(err)
				}
				if !match {
					continue
				}
				equal(t, mailField[string](t, rule, "keyless", "issuer"), githubIssuer)
				identity, err := regexp.Compile(mailField[string](t, rule, "keyless", "subjectRegex"))
				if err != nil {
					t.Fatal(err)
				}
				equal(t, identity.MatchString(mailPublisher), repository == mailRepository)
				if repository == mailRepository {
					for _, forbidden := range []string{
						strings.Replace(mailPublisher, "refs/heads/main", "refs/pull/1/merge", 1),
						strings.Replace(mailPublisher, "refs/heads/main", "refs/heads/other", 1),
						strings.Replace(mailPublisher, "devantler-tech/", "other-owner/", 1),
						strings.Replace(mailPublisher, "publish-private-mail-bootstrap", "publish-coroot-node-agent-hotfix", 1),
						mailPublisher + ".other",
					} {
						if identity.MatchString(forbidden) {
							t.Fatal("mail node verification admitted another publisher")
						}
					}
				}
				return
			}
			t.Fatal("mail image verification rule is missing")
		})
	}
}

func TestPrivateMailSignerCannotAuthorizeOtherImageRepositories(t *testing.T) {
	evaluator := newImagePolicyEvaluator(t, readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"))
	for _, repository := range []string{mailRepository, mailRepository + "-other", "ghcr.io/devantler-tech/wedding-app"} {
		image := repository + "@sha256:" + strings.Repeat("a", 64)
		admitted, calls := evaluator.evaluate(t, map[string][]string{"containers": {image}, "initContainers": {}, "ephemeralContainers": {}}, map[string]certificateFixture{image: {issuer: githubIssuer, subject: mailPublisher}})
		equal(t, admitted, repository == mailRepository)
		if repository == mailRepository {
			if !slices.Equal(calls, []string{image + "/publishprivatemail"}) {
				t.Fatal("mail image must use only its dedicated attestor")
			}
		}
	}
}
