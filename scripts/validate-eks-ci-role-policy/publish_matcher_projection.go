package main

import "strings"

// publishMatcherSurfaceDocument recognizes only strict subsets of the shared
// 40-hex signer pattern already covered by the authorization approval. The
// required approved-revisions guard separately enforces each consumer's exact
// generated set. Issuer, workflow, artifact, identity count and all other
// resource fields remain fingerprinted, so regeneration needs no hash refresh.
func publishMatcherSurfaceDocument(identity resourceIdentity, document map[string]any) map[string]any {
	if identity.apiVersion != "source.toolkit.fluxcd.io/v1" || identity.kind != "OCIRepository" {
		return document
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return document
	}
	var workflow string
	switch spec["url"] {
	case "oci://ghcr.io/devantler-tech/github-config/manifests", "oci://ghcr.io/devantler-tech/aws/manifests":
		workflow = "publish-manifests"
	case "oci://ghcr.io/devantler-tech/ascoachingogvaner/manifests", "oci://ghcr.io/devantler-tech/wedding-app/manifests":
		workflow = "publish-app"
	default:
		return document
	}
	verify, ok := spec["verify"].(map[string]any)
	if !ok || verify["provider"] != "cosign" {
		return document
	}
	identities, ok := verify["matchOIDCIdentity"].([]any)
	if !ok || len(identities) != 1 {
		return document
	}
	matcher, ok := identities[0].(map[string]any)
	if !ok || matcher["issuer"] != `^https://token\.actions\.githubusercontent\.com$` {
		return document
	}
	subject, ok := matcher["subject"].(string)
	if !ok {
		return document
	}
	projectedSubject, ok := projectPublishSubject(subject, workflow)
	if !ok {
		return document
	}

	projected := cloneStringAnyMap(document)
	projectedSpec := cloneStringAnyMap(spec)
	projectedVerify := cloneStringAnyMap(verify)
	projectedMatcher := cloneStringAnyMap(matcher)
	projectedMatcher["subject"] = projectedSubject
	projectedVerify["matchOIDCIdentity"] = []any{projectedMatcher}
	projectedSpec["verify"] = projectedVerify
	projected["spec"] = projectedSpec
	return projected
}

// maxPublishRevisions is the largest approved set a workflow's consumers carry: the
// applied signer and the default-branch pin, plus the latest released actions revision
// for both publish-app and publish-manifests consumers (#4416).
func maxPublishRevisions() int {
	return 3
}

// isExactPublishRevisionSet accepts one concrete SHA or one parenthesized set of at most
// limit concrete SHAs; regular expressions, floating refs, and additional alternatives
// stay exact.
func isExactPublishRevisionSet(ref string, limit int) bool {
	if exactGitCommit.MatchString(ref) {
		return true
	}
	if !strings.HasPrefix(ref, "(") || !strings.HasSuffix(ref, ")") {
		return false
	}
	revisions := strings.Split(ref[1:len(ref)-1], "|")
	if len(revisions) > limit {
		return false
	}
	for _, revision := range revisions {
		if !exactGitCommit.MatchString(revision) {
			return false
		}
	}
	return true
}

const (
	publishSubjectHost     = `^https://github\.com/devantler-tech/`
	legacyPublishFamily    = `actions/\.github/workflows/`
	canonicalPublishFamily = `\.github/\.github/workflows/`
	// canonicalPublishWorkflow is the only workflow whose artifacts may also accept the
	// canonical publisher; application-image publishers stay legacy-only (#4502).
	canonicalPublishWorkflow = "publish-manifests"
)

// projectPublishSubject replaces the generated legacy signer set with the shared pattern
// and leaves everything else exactly as written. A legacy-only subject projects to the
// approved pattern subject. A two-family subject keeps its group, its repository family
// and its canonical commit, so adding, removing or moving the canonical approval always
// moves the fingerprint, while the daily legacy regeneration inside it does not.
func projectPublishSubject(subject, workflow string) (string, bool) {
	legacy := legacyPublishFamily + workflow + `\.yaml@`
	if rest, ok := strings.CutPrefix(subject, publishSubjectHost+legacy); ok {
		ref, anchored := strings.CutSuffix(rest, "$")
		if !anchored || !isExactPublishRevisionSet(ref, maxPublishRevisions()) {
			return "", false
		}
		return publishSubjectHost + legacy + `[0-9a-f]{40}$`, true
	}
	if workflow != canonicalPublishWorkflow {
		return "", false
	}
	inner, ok := strings.CutPrefix(subject, publishSubjectHost+"("+legacy)
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, ")$")
	if !ok {
		return "", false
	}
	canonical := `|` + canonicalPublishFamily + workflow + `\.yaml@`
	ref, commit, ok := strings.Cut(inner, canonical)
	if !ok || !exactGitCommit.MatchString(commit) || !isExactPublishRevisionSet(ref, maxPublishRevisions()) {
		return "", false
	}
	return publishSubjectHost + "(" + legacy + `[0-9a-f]{40}` + canonical + commit + ")$", true
}
