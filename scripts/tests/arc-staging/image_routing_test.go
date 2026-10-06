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

// Evaluate the committed CEL unchanged. Only the remote cryptographic lookup
// is replaced with deterministic certificate fixtures; routing is not mocked.
func TestAnalysisImageAdmissionRoutingAndSignerIsolation(t *testing.T) {
	policy := readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml")
	evaluator := newImagePolicyEvaluator(t, policy)
	tests := []struct {
		repository, attestor, subject string
	}{
		{analysisRepository, "publishksailanalysis", analysisPublisher},
		{analysisRepository + "-other", "ksailcd", "https://github.com/devantler-tech/ksail/.github/workflows/cd.yaml@refs/tags/v1.2.3"},
		{"ghcr.io/devantler-tech/wedding-app", "publishapp", "https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("b", 40)},
		{"ghcr.io/devantler-tech/platform-kubescape-storage", "publishkubescapestorage", "https://github.com/devantler-tech/platform/.github/workflows/publish-kubescape-storage-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-kubescape-node-agent", "publishkubescapenodeagent", "https://github.com/devantler-tech/platform/.github/workflows/publish-kubescape-node-agent-hotfix.yaml@refs/heads/main"},
		{"ghcr.io/devantler-tech/platform-coroot-node-agent", "publishcorootnodeagent", "https://github.com/devantler-tech/platform/.github/workflows/publish-coroot-node-agent-hotfix.yaml@refs/heads/main"},
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
				app:      {githubIssuer, "https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("a", 40)},
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
		return "https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("a", 40)
	}
	return analysisPublisher
}
