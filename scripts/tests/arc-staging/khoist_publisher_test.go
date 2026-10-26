package arcstaging_test

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

const khoistImage = "ghcr.io/devantler-tech/khoist"
const khoistPublisher = "https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@2df24600b1113e291b87e300b91e59c160446134"

// Compile the complete committed CEL; only signature transport uses fixtures.
func TestKHoistPublisherIsIsolatedInAdmission(t *testing.T) {
	evaluator := newImagePolicyEvaluator(t, readYAML(t, "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"))
	for _, repository := range []string{khoistImage, khoistImage + "-other", khoistImage + "/child", "ghcr.io/devantler-tech/wedding-app", "ghcr.io/devantler-tech/data-product-controller"} {
		for _, suffix := range []string{"", ":v1.0.0", "@sha256:" + strings.Repeat("a", 64), ":v1.0.0@sha256:" + strings.Repeat("a", 64)} {
			image := repository + suffix
			for _, slot := range []string{"containers", "initContainers", "ephemeralContainers"} {
				t.Run(image+"/"+slot, func(t *testing.T) {
					images := map[string][]string{"containers": {}, "initContainers": {}, "ephemeralContainers": {}}
					images[slot] = []string{image}
					admitted, calls := evaluator.evaluate(t, images, map[string]certificateFixture{image: {githubIssuer, khoistPublisher}})
					equal(t, admitted, repository == khoistImage)
					if repository != khoistImage {
						return
					}
					if len(calls) != 1 || calls[0] != image+"/publishkhoist" {
						t.Fatal("KHoist verification was skipped or routed through a shared attestor", calls)
					}
					for _, certificate := range []certificateFixture{
						{"", ""}, {"https://other.example", khoistPublisher},
						{githubIssuer, strings.Replace(khoistPublisher, "devantler-tech/", "other-owner/", 1)},
						{githubIssuer, strings.Split(khoistPublisher, "@")[0] + "@refs/tags/v13.2.13"},
						{githubIssuer, strings.Split(khoistPublisher, "@")[0] + "@" + strings.Repeat("b", 40)},
						{githubIssuer, strings.Split(khoistPublisher, "@")[0] + "@0c7b6c3509b73adbc7537e030935bb01987e625b"},
					} {
						got, _ := evaluator.evaluate(t, images, map[string]certificateFixture{image: certificate})
						equal(t, got, false)
					}
					for _, retained := range []string{
						"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0",
						"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("b", 40),
					} {
						got, _ := evaluator.evaluate(t, images, map[string]certificateFixture{image: {githubIssuer, retained}})
						equal(t, got, true)
					}
				})
			}
		}
	}
}

func TestKHoistPublisherIsIsolatedInFirstMatchingNodeRule(t *testing.T) {
	rules := field(t, readYAML(t, "talos/cluster/verify-first-party-images.yaml"), "rules").([]any)
	for _, repository := range []string{khoistImage, khoistImage + "-other", khoistImage + "/child", "ghcr.io/devantler-tech/wedding-app", "ghcr.io/devantler-tech/data-product-controller"} {
		found := false
		for _, rule := range rules {
			matches, err := path.Match(field(t, rule, "image").(string), repository)
			if err != nil {
				t.Fatal(err)
			}
			if !matches {
				continue
			}
			found = true
			identity := regexp.MustCompile(field(t, rule, "keyless", "subjectRegex").(string))
			equal(t, field(t, rule, "keyless", "issuer"), githubIssuer)
			equal(t, identity.MatchString(khoistPublisher), repository == khoistImage)
			break
		}
		// The existing one-level node catch-all does not select nested paths.
		// Keep that baseline unchanged; the complete admission test above rejects
		// this child, and any broadened KHoist node rule would fail the match test.
		if !found && repository != khoistImage+"/child" {
			t.Fatal("first-party image escaped node verification", repository)
		}
	}
}
