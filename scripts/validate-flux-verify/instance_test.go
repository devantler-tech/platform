package main

import (
	"os"
	"strings"
	"testing"
)

// goodInstance is the shape that puts spec.verify on the LIVE root source: a
// flux-operator kustomize patch whose target is the operator-generated
// flux-system OCIRepository.
const goodInstance = `
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: Deployment
          name: kustomize-controller
          namespace: flux-system
        patch: |
          - op: replace
            path: /spec/replicas
            value: 2
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/verify
            value:
              provider: cosign
              matchOIDCIdentity:
                - issuer: '^https://token\.actions\.githubusercontent\.com$'
                  subject: '^https://github\.com/devantler-tech/platform/.+$'
`

func TestValidateInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// wantErr is a substring the failure must name, so an arm cannot pass
		// on the wrong error. Empty means the manifest must validate.
		wantErr  string
		manifest string
	}{
		{
			name:     "patch on the root source validates",
			manifest: goodInstance,
		},
		{
			// The state this check was written for: every other patch present,
			// no verify patch at all, so the live root source pulls unverified
			// however correct ksail.prod.yaml is (#2922).
			name: "no verify patch is rejected",
			manifest: `
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: Deployment
          name: kustomize-controller
        patch: |
          - op: replace
            path: /spec/replicas
            value: 2
`,
			wantErr: "no kustomize patch adds /spec/verify",
		},
		{
			// The silent-no-op shape, measured: a patch whose target matches
			// nothing renders NOTHING, exit 0, no warning. The control looks
			// configured in the file and does not exist in the cluster.
			name:     "verify patch aimed at the wrong name is rejected",
			manifest: strings.Replace(goodInstance, "name: flux-system\n          namespace", "name: NOT-THE-ROOT-SOURCE\n          namespace", 1),
			wantErr:  "does not target the root source",
		},
		{
			name:     "verify patch aimed at the wrong kind is rejected",
			manifest: strings.Replace(goodInstance, "kind: OCIRepository", "kind: GitRepository", 1),
			wantErr:  "does not target the root source",
		},
		{
			// A target carrying no name matches every OCIRepository in scope,
			// so which resource it lands on is not stated by the manifest.
			// Ambiguity fails closed here rather than being read as a hit.
			name: "verify patch with no target name is rejected",
			manifest: strings.Replace(goodInstance, `
          kind: OCIRepository
          name: flux-system
          namespace: flux-system`, `
          kind: OCIRepository
          namespace: flux-system`, 1),
			wantErr: "does not target the root source",
		},
		{
			// The same inert-block class validate() already guards on the KSail
			// side: present, enabled, and constraining nobody.
			name: "verify patch that constrains no signer is rejected",
			manifest: `
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/verify
            value:
              provider: cosign
`,
			wantErr: "constrains no signer",
		},
		{
			// A FluxInstance under another identity is not the one the platform
			// reconciles, so a correct patch on it certifies nothing.
			name:     "a FluxInstance under another identity is not accepted",
			manifest: strings.Replace(goodInstance, "  name: flux\n", "  name: some-other-instance\n", 1),
			wantErr:  "no FluxInstance",
		},
		{
			// The decoy case, and the reason kind alone is not enough: the
			// instance that IS deployed carries no verify patch while a second
			// document does. Selecting by kind would pass this.
			name: "a decoy instance cannot satisfy the check for the real one",
			manifest: strings.Replace(goodInstance, "  name: flux\n", "  name: decoy\n", 1) + `
---
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches: []
`,
			wantErr: "no kustomize patch adds /spec/verify",
		},
		{
			name:     "duplicate FluxInstance documents are rejected",
			manifest: goodInstance + "\n---" + goodInstance,
			wantErr:  "does not say which one is applied",
		},
		{
			// add-then-remove WITHIN one patch body.
			name: "add then remove in one patch body is rejected",
			manifest: `
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/verify
            value:
              provider: cosign
              matchOIDCIdentity:
                - issuer: 'https://example.invalid'
                  subject: 'https://example.invalid/workflow'
          - op: remove
            path: /spec/verify
`,
			wantErr: "removes it again",
		},
		{
			// 🔴 THE SHAPE THAT HALTED PROD, pinned on the half that actually
			// reaches a running cluster. Three individually well-formed entries:
			// the patch targets correctly, the provider is cosign, and a signer is
			// constrained — so every other assertion in this file passes. cosign
			// still refuses every artifact, because a multi-entry matcher fails
			// CLOSED for the whole set.
			//
			// Asserted here as well as in the cluster-config half deliberately.
			// checkNoDrift only makes the two halves EQUAL, so two identically
			// broken copies satisfy it; equality is not correctness.
			name: "a multi-entry matchOIDCIdentity is rejected",
			manifest: `
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/verify
            value:
              provider: cosign
              matchOIDCIdentity:
                - issuer: '^https://token\.actions\.githubusercontent\.com$'
                  subject: '^https://github\.com/devantler-tech/platform/\.github/workflows/ci\.yaml@refs/heads/gh-readonly-queue/main/.+$'
                - issuer: '^https://token\.actions\.githubusercontent\.com$'
                  subject: '^https://github\.com/devantler-tech/platform/\.github/workflows/cd\.yaml@refs/heads/main$'
                - issuer: '^https://token\.actions\.githubusercontent\.com$'
                  subject: '^https://github\.com/devantler-tech/platform/\.github/workflows/dr-rebuild\.yaml@refs/heads/main$'
`,
			wantErr: "supports exactly ONE",
		},
		{
			// add-then-remove ACROSS two patches on the same target: the first
			// write says verified, the effective state is not.
			name: "a later patch removing the field is rejected",
			manifest: goodInstance + `      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: remove
            path: /spec/verify
`,
			wantErr: "removes it again",
		},
		{
			// The LAST write wins, so a later replace that constrains nobody is
			// what gets judged — not the well-formed earlier one.
			name: "a later replace that constrains no signer is rejected",
			manifest: goodInstance + `      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: replace
            path: /spec/verify
            value:
              provider: cosign
`,
			wantErr: "constrains no signer",
		},
		{
			// remove-then-add leaves the field PRESENT, so it must pass:
			// rejecting it would fail a legitimate reset-and-set.
			name: "remove then add is accepted",
			manifest: strings.Replace(goodInstance,
				"          - op: add\n            path: /spec/verify",
				"          - op: remove\n            path: /spec/verify\n          - op: add\n            path: /spec/verify",
				1),
		},
		{
			// Mirrors KSail's own Enabled() predicate: a blank provider renders
			// no verification at all.
			name:     "verify patch with a blank provider is rejected",
			manifest: strings.Replace(goodInstance, "provider: cosign", `provider: "  "`, 1),
			wantErr:  "DISABLED",
		},
		{
			// `remove` would delete the field this check exists to require, and
			// `test` asserts rather than writes. Only a write counts.
			name:     "a non-writing op on /spec/verify is rejected",
			manifest: strings.Replace(goodInstance, "op: add", "op: remove", 1),
			wantErr:  "no kustomize patch adds /spec/verify",
		},
		{
			name: "a manifest with no FluxInstance is rejected",
			manifest: `
kind: ConfigMap
metadata:
  name: not-a-flux-instance
`,
			wantErr: "no FluxInstance",
		},
		{
			name:     "unparseable manifest is rejected",
			manifest: "\tthis: [is not\n",
			wantErr:  "does not parse",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateInstance([]byte(test.manifest))

			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("manifest should validate, got: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("manifest should be rejected naming %q, got nil", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error should name %q, got: %v", test.wantErr, err)
			}
		})
	}
}

// TestRealFluxInstanceValidates pins the checked-in manifest itself, so the
// contract is asserted against what actually ships rather than only against
// fixtures. The path is relative to this package's directory.
func TestRealFluxInstanceValidates(t *testing.T) {
	t.Parallel()

	const path = "../../k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml"

	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := validateInstance(manifest); err != nil {
		t.Fatalf("the shipped FluxInstance should carry an effective verify patch: %v", err)
	}
}

// TestRealHalvesAgree pins the SHIPPED pair against each other. The two files
// are the ones CI passes, so this asserts the live duplication is consistent
// rather than only that each file is individually well-formed.
func TestRealHalvesAgree(t *testing.T) {
	t.Parallel()

	config, err := os.ReadFile("../../ksail.prod.yaml")
	if err != nil {
		t.Fatalf("read cluster config: %v", err)
	}

	manifest, err := os.ReadFile(
		"../../k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml")
	if err != nil {
		t.Fatalf("read FluxInstance: %v", err)
	}

	configBlock, err := configVerifyBlock(config)
	if err != nil {
		t.Fatalf("cluster config half: %v", err)
	}

	instanceBlock, err := instanceVerifyBlock(manifest)
	if err != nil {
		t.Fatalf("FluxInstance half: %v", err)
	}

	if err := checkNoDrift(configBlock, instanceBlock); err != nil {
		t.Fatalf("the two halves must pin the same signers: %v", err)
	}
}
