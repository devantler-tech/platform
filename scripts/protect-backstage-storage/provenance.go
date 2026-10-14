package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

const provenancePredicate = "https://slsa.dev/provenance/v1"
const artifactSubject = "ghcr.io/devantler-tech/platform/manifests"
const repositoryURI = "https://github.com/devantler-tech/platform"

type verifiedAttestation struct {
	VerificationResult struct {
		Signature struct {
			Certificate struct {
				Issuer                 string `json:"issuer"`
				SubjectAlternativeName string `json:"subjectAlternativeName"`
				SourceRepositoryURI    string `json:"sourceRepositoryURI"`
				SourceRepositoryDigest string `json:"sourceRepositoryDigest"`
				SourceRepositoryRef    string `json:"sourceRepositoryRef"`
				RunInvocationURI       string `json:"runInvocationURI"`
			} `json:"certificate"`
		} `json:"signature"`
		Statement struct {
			PredicateType       string `json:"predicateType"`
			LegacyPredicateType string `json:"predicate_type"`
			Subject             []struct {
				Name   string            `json:"name"`
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		} `json:"statement"`
	} `json:"verificationResult"`
}

// artifactAccepted binds the immutable live artifact to the accepted run using
// signed certificate facts. Predicate-provided source and run IDs are ignored.
func (e engine) artifactAccepted(ctx context.Context, workflow, ref, attempt string) bool {
	digest := strings.TrimPrefix(e.revision, "latest@")
	identity := repositoryURI + "/" + workflow + "@" + ref
	b, err := e.execute(ctx, "gh", "attestation", "verify", "oci://"+artifactSubject+"@"+digest,
		"--bundle-from-oci", "--repo", "devantler-tech/platform",
		"--predicate-type", provenancePredicate, "--source-digest", e.sha,
		"--cert-identity-regex", "^"+regexp.QuoteMeta(identity)+"$", "--format", "json")
	if err != nil || len(b) > responseLimit || !json.Valid(b) {
		return false
	}
	var entries []verifiedAttestation
	if json.Unmarshal(b, &entries) != nil || len(entries) == 0 || len(entries) > 30 {
		return false
	}
	wantRun := repositoryURI + "/actions/runs/" + e.deploymentRun + "/attempts/" + attempt
	wantHex := strings.TrimPrefix(digest, "sha256:")
	matched := false
	for _, entry := range entries {
		cert := entry.VerificationResult.Signature.Certificate
		statement := entry.VerificationResult.Statement
		predicate := statement.PredicateType
		if predicate == "" {
			predicate = statement.LegacyPredicateType
		}
		if cert.Issuer != "https://token.actions.githubusercontent.com" || cert.SourceRepositoryURI != repositoryURI || cert.SourceRepositoryDigest != e.sha || cert.SourceRepositoryRef != ref || cert.SubjectAlternativeName != identity || predicate != provenancePredicate || len(statement.Subject) != 1 {
			return false
		}
		subject := statement.Subject[0]
		if subject.Name != artifactSubject || len(subject.Digest) != 1 || subject.Digest["sha256"] != wantHex || !regexp.MustCompile(`^https://github\.com/devantler-tech/platform/actions/runs/[1-9][0-9]{0,19}/attempts/[1-9][0-9]{0,5}$`).MatchString(cert.RunInvocationURI) {
			return false
		}
		matched = matched || cert.RunInvocationURI == wantRun
	}
	return matched
}
