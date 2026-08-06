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
