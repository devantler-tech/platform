package arcstaging_test

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// appPublisher is a reviewed maintained publish-app revision the publishapp
// attestor accepts. deletedPublishers are the deleted legacy publisher's
// commit-pinned identities, which no attestor accepts at any commit (#6264). A
// release tag under that repository name is a different identity: the
// publishapp attestor accepts any first-party repository's release workflow on
// a release tag except `.github` (#6265), see ownReleaseSigners.
const appPublisher = "https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812"

var deletedPublishers = []string{
	"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("b", 40),
	"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328",
}

// ownReleaseSigners are first-party release-workflow identities (#6265, ADR 0017).
// The publishapp attestor accepts a valid one for every ordinary first-party
// image: one repository segment that starts with a letter or digit, one workflow
// file directly in .github/workflows/, and a strict semver release tag. Every
// other attestor refuses them all. A repository named `actions` would be a
// first-party repository like any other, so its release tags are accepted;
// `.github` hosts reusable workflows other repositories call, so its tags are
// refused.
var ownReleaseSigners = []struct {
	subject string
	valid   bool
}{
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3", true},
	{"https://github.com/devantler-tech/ascoachingogvaner/.github/workflows/cd.yml@refs/tags/v1.2.3-rc.1", true},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/heads/main", false},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@" + strings.Repeat("c", 40), false},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2", false},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/latest", false},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3/../evil", false},
	{"https://github.com/devantler-tech/wedding-app/.github/workflows/nested/release.yaml@refs/tags/v1.2.3", false},
	{"https://github.com/devantler-tech/wedding-app/sub/.github/workflows/release.yaml@refs/tags/v1.2.3", false},
	{"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v21.3.4", false},
	{"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v1.2.3", false},
	{"https://github.com/devantler-tech/.github/.github/workflows/release.yaml@refs/tags/v1.2.3", false},
	{"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@refs/tags/v1.2.3", true},
	{"https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@refs/tags/v5.4.0", true},
	{"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328", false},
	{"https://github.com/other-owner/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3", false},
}

// Evaluate the committed CEL unchanged. Only the remote cryptographic lookup
// is replaced with deterministic certificate fixtures; routing is not mocked.
func TestAnalysisImageAdmissionRoutingAndSignerIsolation(t *testing.T) {
	policy := readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml")
	evaluator := newImagePolicyEvaluator(t, policy)
	identityPublisher := "https://github.com/devantler-tech/business-site/.github/workflows/publish-identity.yaml@refs/heads/main"
	retirementPublisher := "https://github.com/devantler-tech/platform/.github/workflows/publish-longhorn-v1-retirement.yaml@refs/heads/main"
	v2Publisher := "https://github.com/devantler-tech/platform/.github/workflows/publish-longhorn-v2-only.yaml@refs/heads/main"
	fioPublisher := "https://github.com/devantler-tech/platform/.github/workflows/publish-longhorn-fio.yaml@refs/heads/main"
	senderPublisher := "https://github.com/devantler-tech/platform/.github/workflows/publish-mail-submission.yaml@refs/heads/main"
	tests := []struct {
		repository, attestor, subject string
	}{
		{analysisRepository, "publishksailanalysis", analysisPublisher},
		{mailRepository, "publishprivatemail", mailPublisher},
		{mailCandidateRepository, "publishprivatemail", mailPublisher},
		{"ghcr.io/devantler-tech/business-site-identity", "publishcustomeridentity", identityPublisher},
		{"ghcr.io/devantler-tech/business-site-identity-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/business-site-identity/helper", "publishapp", appPublisher},
		{mailRepository + "-other", "publishapp", appPublisher},
		{mailCandidateRepository + "-other", "publishapp", appPublisher},
		{mailCandidateRepository + "/helper", "publishapp", appPublisher},
		{analysisRepository + "-other", "ksailcd", "https://github.com/devantler-tech/ksail/.github/workflows/cd.yaml@refs/tags/v1.2.3"},
		{"ghcr.io/devantler-tech/wedding-app", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-kubescape-storage", "publishkubescapestorage", "https://github.com/devantler-tech/platform/.github/workflows/publish-kubescape-storage-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-kubescape-node-agent", "publishkubescapenodeagent", "https://github.com/devantler-tech/platform/.github/workflows/publish-kubescape-node-agent-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-coroot-node-agent", "publishcorootnodeagent", "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-node-agent-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-longhorn-instance-manager", "publishlonghornretirement", retirementPublisher},
		{"ghcr.io/devantler-tech/platform-longhorn-manager", "publishlonghornv2", v2Publisher},
		{"ghcr.io/devantler-tech/platform-longhorn-fio", "publishlonghornfio", fioPublisher},
		{"ghcr.io/devantler-tech/platform-longhorn-fio-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-longhorn-instance-manager-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-longhorn-manager/other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-coroot", "publishcorootcheckpoint", "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-checkpoint-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-coroot-cluster-agent", "publishcorootcheckpoint", "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-checkpoint-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-coroot-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-coroot-cluster-agent-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-kyverno-reports", "publishkyvernoreports", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-reports-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-kyverno-reports-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-kyverno-migrate", "publishkyvernomigrate", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-migration-runtime.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-kyverno-migrate-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-kyverno-background", "publishkyvernobackground", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-background-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-kyverno-background-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-dex", "publishdexsecurity", "https://github.com/devantler-tech/platform/.github/workflows/publish-dex-security-image.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-dex-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-mail-submission", "publishmailsubmission", senderPublisher},
		{"ghcr.io/devantler-tech/platform-mail-submission-other", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/platform-mail-submission/helper", "publishapp", appPublisher},
		{"ghcr.io/devantler-tech/world-at-ruin/zone", "publishwarzone", "https://github.com/devantler-tech/world-at-ruin/.github/workflows/server-cd.yaml@refs/tags/v1.2.3"},
		{"ghcr.io/devantler-tech/provider-upjet-unifi", "publishprovider", "https://github.com/devantler-tech/provider-upjet-unifi/.github/workflows/publish-provider-package.yml@refs/tags/v1.2.3"},
		{"ghcr.io/devantler-tech/ksail", "ksailcd", "https://github.com/devantler-tech/ksail/.github/workflows/cd.yaml@refs/tags/v1.2.3"},
		{"ghcr.io/devantler-tech/ksail-steer", "ksailcd", "https://github.com/devantler-tech/ksail/.github/workflows/cd.yaml@refs/tags/v1.2.3"},
		{"ghcr.io/actions/actions-runner", "", ""},
	}
	for _, fixture := range tests {
		for _, suffix := range []string{"", ":revision", "@sha256:" + strings.Repeat("a", 64), ":revision@sha256:" + strings.Repeat("a", 64)} {
			image := fixture.repository + suffix
			for _, slot := range []string{"containers", "initContainers", "ephemeralContainers"} {
				t.Run(image+"/"+slot, func(t *testing.T) {
					for _, control := range []struct {
						name, subject, issuer string
						want                  bool
					}{
						{"correct signer", fixture.subject, githubIssuer, true},
						{"unsigned", "", "", fixture.attestor == ""},
						{"wrong issuer", fixture.subject, "https://other.example", fixture.attestor == ""},
						{"cross workflow signer", crossSigner(fixture.subject), githubIssuer, fixture.attestor == ""},
						{"customer identity signer", identityPublisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishcustomeridentity"},
						{"shared app signer", appPublisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishapp"},
						{"identity wrong workflow", strings.Replace(identityPublisher, "publish-identity.yaml", "publish-runtime.yaml", 1), githubIssuer, fixture.attestor == ""},
						// The business-site repository's workflow on a release tag is a
						// first-party release identity (#6265), so it satisfies only the
						// generic app attestor, never the dedicated identity attestor.
						{"identity tag ref", strings.Replace(identityPublisher, "refs/heads/main", "refs/tags/v4.20.0", 1), githubIssuer, fixture.attestor == "" || fixture.attestor == "publishapp"},
						{"identity pull request ref", strings.Replace(identityPublisher, "refs/heads/main", "refs/pull/1/merge", 1), githubIssuer, fixture.attestor == ""},
						{"identity lookalike branch", identityPublisher + "-other", githubIssuer, fixture.attestor == ""},
						{"checkpoint signer", "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-checkpoint-hotfix.yaml@refs/heads/main", githubIssuer, fixture.attestor == "" || fixture.attestor == "publishcorootcheckpoint"},
						{"Longhorn retirement signer", retirementPublisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishlonghornretirement"},
						{"Longhorn V2-only signer", v2Publisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishlonghornv2"},
						{"Longhorn fio signer", fioPublisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishlonghornfio"},
						{"reports signer", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-reports-hotfix.yaml@refs/heads/main", githubIssuer, fixture.attestor == "" || fixture.attestor == "publishkyvernoreports"},
						{"migrate signer", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-migration-runtime.yaml@refs/heads/main", githubIssuer, fixture.attestor == "" || fixture.attestor == "publishkyvernomigrate"},
						{"background signer", "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-background-hotfix.yaml@refs/heads/main", githubIssuer, fixture.attestor == "" || fixture.attestor == "publishkyvernobackground"},
						{"dex signer", "https://github.com/devantler-tech/platform/.github/workflows/publish-dex-security-image.yaml@refs/heads/main", githubIssuer, fixture.attestor == "" || fixture.attestor == "publishdexsecurity"},
						{"automated sender signer", senderPublisher, githubIssuer, fixture.attestor == "" || fixture.attestor == "publishmailsubmission"},
						{"wrong ref", strings.Split(fixture.subject, "@")[0] + "@refs/heads/other", githubIssuer, fixture.attestor == ""},
						{"wrong repository", strings.Replace(fixture.subject, "devantler-tech/", "other-owner/", 1), githubIssuer, fixture.attestor == ""},
					} {
						t.Run(control.name, func(t *testing.T) {
							images := map[string][]string{"containers": {}, "initContainers": {}, "ephemeralContainers": {}}
							images[slot] = []string{image}
							admitted, calls := evaluator.evaluate(t, images, map[string]certificateFixture{
								image: {issuer: control.issuer, subject: control.subject},
							})
							wantCalls := []string{}
							if fixture.attestor != "" {
								wantCalls = []string{image + "/" + fixture.attestor}
							}
							if !reflect.DeepEqual(calls, wantCalls) {
								t.Fatalf("verification calls = %v, want %v", calls, wantCalls)
							}
							equal(t, admitted, control.want)
						})
					}
					for _, deleted := range deletedPublishers {
						t.Run("deleted publisher "+deleted[len(deleted)-40:], func(t *testing.T) {
							images := map[string][]string{"containers": {}, "initContainers": {}, "ephemeralContainers": {}}
							images[slot] = []string{image}
							admitted, _ := evaluator.evaluate(t, images, map[string]certificateFixture{
								image: {issuer: githubIssuer, subject: deleted},
							})
							equal(t, admitted, fixture.attestor == "")
						})
					}
					for _, signer := range ownReleaseSigners {
						t.Run("own release "+signer.subject, func(t *testing.T) {
							images := map[string][]string{"containers": {}, "initContainers": {}, "ephemeralContainers": {}}
							images[slot] = []string{image}
							admitted, _ := evaluator.evaluate(t, images, map[string]certificateFixture{
								image: {issuer: githubIssuer, subject: signer.subject},
							})
							equal(t, admitted, fixture.attestor == "" || (fixture.attestor == "publishapp" && signer.valid))
						})
					}
				})
			}
		}
	}
}

type certificateFixture struct {
	issuer, subject string
}

type imagePolicyEvaluator struct {
	programs     []cel.Program
	attestors    map[string]string
	certificates map[string]certificateFixture
	calls        []string
}

func newImagePolicyEvaluator(t *testing.T, policy map[string]any) *imagePolicyEvaluator {
	t.Helper()
	attestors := map[string]string{}
	identities := map[string]map[string]any{}
	for _, item := range field(t, policy, "spec", "attestors").([]any) {
		name := field(t, item, "name").(string)
		entries := field(t, item, "cosign", "keyless", "identities").([]any)
		if len(entries) != 1 {
			t.Fatalf("%s must have exactly one identity", name)
		}
		if _, duplicate := identities[name]; duplicate {
			t.Fatalf("duplicate attestor %s", name)
		}
		attestors[name] = name
		identities[name] = entries[0].(map[string]any)
	}
	if _, exists := identities["publishksailanalysis"]; !exists {
		t.Fatal("analysis image lacks an independent attestor")
	}
	evaluator := &imagePolicyEvaluator{attestors: attestors}
	env, err := cel.NewEnv(
		cel.Variable("images", cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
		cel.Variable("attestors", cel.MapType(cel.StringType, cel.StringType)),
		cel.Function("verifyImageSignatures", cel.Overload("verify_image_fixture",
			[]*cel.Type{cel.StringType, cel.ListType(cel.StringType)}, cel.IntType,
			cel.BinaryBinding(func(imageValue, attestorValues ref.Val) ref.Val {
				list := attestorValues.(traits.Lister)
				if list.Size() != types.Int(1) {
					return types.NewErr("exactly one attestor required")
				}
				name := list.Get(types.Int(0)).Value().(string)
				image := imageValue.Value().(string)
				evaluator.calls = append(evaluator.calls, image+"/"+name)
				certificate := evaluator.certificates[image]
				identity := identities[name]
				if identity["issuer"] == certificate.issuer &&
					regexp.MustCompile(identity["subjectRegExp"].(string)).MatchString(certificate.subject) {
					return types.Int(1)
				}
				return types.Int(0)
			}))),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, validation := range field(t, policy, "spec", "validations").([]any) {
		ast, issues := env.Compile(field(t, validation, "expression").(string))
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		evaluator.programs = append(evaluator.programs, program)
	}
	return evaluator
}

// Each test owns an evaluator and uses its certificate fixtures sequentially.
func (e *imagePolicyEvaluator) evaluate(t *testing.T, images map[string][]string, certificates map[string]certificateFixture) (bool, []string) {
	t.Helper()
	e.certificates = certificates
	e.calls = []string{}
	admitted := true
	for _, program := range e.programs {
		result, _, err := program.Eval(map[string]any{"images": images, "attestors": e.attestors})
		if err != nil {
			t.Fatal(err)
		}
		value, ok := result.Value().(bool)
		if !ok {
			t.Fatalf("validation returned %T", result.Value())
		}
		admitted = admitted && value
	}
	return admitted, e.calls
}

func TestMixedImagePodCannotHideOneUnsignedImage(t *testing.T) {
	policy := readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml")
	evaluator := newImagePolicyEvaluator(t, policy)
	analysis := analysisRepository + ":revision"
	app := "ghcr.io/devantler-tech/wedding-app:revision"
	coroot := "ghcr.io/devantler-tech/platform-coroot-node-agent:revision"
	images := map[string][]string{
		"containers":          {analysis, app},
		"initContainers":      {coroot},
		"ephemeralContainers": {"ghcr.io/actions/actions-runner:revision"},
	}
	for _, unsigned := range []string{"", analysis, app, coroot} {
		t.Run(unsigned, func(t *testing.T) {
			certificates := map[string]certificateFixture{
				analysis: {githubIssuer, analysisPublisher},
				app:      {githubIssuer, appPublisher},
				coroot:   {githubIssuer, "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-node-agent-hotfix.yaml@refs/heads/main"},
			}
			delete(certificates, unsigned)
			admitted, calls := evaluator.evaluate(t, images, certificates)
			slices.Sort(calls)
			wantCalls := []string{analysis + "/publishksailanalysis", app + "/publishapp", coroot + "/publishcorootnodeagent"}
			slices.Sort(wantCalls)
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("mixed verification calls = %v, want %v", calls, wantCalls)
			}
			equal(t, admitted, unsigned == "")
		})
	}
}

func crossSigner(subject string) string {
	if subject == analysisPublisher {
		return appPublisher
	}
	return analysisPublisher
}

func TestCheckpointNodeSignerMatchesAdmissionBoundary(t *testing.T) {
	policy := readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml")
	var admissionIdentity map[string]any
	for _, entry := range field(t, policy, "spec", "attestors").([]any) {
		if field(t, entry, "name") == "publishcorootcheckpoint" {
			identities := field(t, entry, "cosign", "keyless", "identities").([]any)
			equal(t, len(identities), 1)
			admissionIdentity = identities[0].(map[string]any)
		}
	}
	if admissionIdentity == nil {
		t.Fatal("checkpoint admission identity is missing")
	}
	rules := field(t, readYAML(t, "talos/cluster/verify-first-party-images.yaml"), "rules").([]any)
	for _, repository := range []string{"ghcr.io/devantler-tech/platform-coroot", "ghcr.io/devantler-tech/platform-coroot-cluster-agent"} {
		t.Run(repository, func(t *testing.T) {
			found := 0
			for _, entry := range rules {
				if field(t, entry, "image") != repository {
					continue
				}
				found++
				identity := field(t, entry, "keyless").(map[string]any)
				equal(t, identity["issuer"], githubIssuer)
				equal(t, identity["issuer"], admissionIdentity["issuer"])
				equal(t, identity["subjectRegex"], admissionIdentity["subjectRegExp"])
				matcher, err := regexp.Compile(identity["subjectRegex"].(string))
				if err != nil {
					t.Fatal(err)
				}
				publisher := "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-checkpoint-hotfix.yaml@"
				for _, subject := range []string{publisher + "refs/heads/main", publisher + "refs/heads/other", publisher + "refs/tags/v1.0.0", publisher + "refs/heads/main-other", publisher + "refs/heads/main/other", strings.Replace(publisher, "checkpoint", "node-agent", 1) + "refs/heads/main", ""} {
					equal(t, matcher.MatchString(subject), subject == publisher+"refs/heads/main")
				}
			}
			equal(t, found, 1)
		})
	}
}
