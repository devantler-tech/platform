package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodConfig is the shape the corrected config (#2919) has: a cosign block at
// the one path KSail reads.
const goodConfig = `
apiVersion: ksail.io/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cluster:
    distribution: Talos
    gitOpsEngine: Flux
  workload:
    sourceDirectory: k8s
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity:
          - issuer: '^https://token\.actions\.githubusercontent\.com$'
            subject: '^https://github\.com/devantler-tech/platform/.+$'
`

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// wantErr is a substring the failure must name, so an arm cannot pass
		// on the wrong error. Empty means the config must validate.
		wantErr string
		config  string
	}{
		{
			name:   "corrected config validates",
			config: goodConfig,
		},
		{
			// The reproduced outage: a complete, correct cosign block at a path
			// ClusterSpec has no field for. Present in the text, inert in effect.
			name: "block at spec.cluster.verify is rejected",
			config: `
spec:
  cluster:
    verify:
      provider: cosign
  workload:
    flux: {}
`,
			wantErr: "spec.cluster.verify",
		},
		{
			// Missing the flux level is the same class as the outage: one level
			// off, silently discarded.
			name: "block at spec.workload.verify is rejected",
			config: `
spec:
  workload:
    verify:
      provider: cosign
    flux: {}
`,
			wantErr: "spec.workload.verify",
		},
		{
			// 🔴 THE DISCRIMINATING ARM. A whitespace-only provider is at the
			// correct path and is a non-empty Go string, so a `provider != ""`
			// check accepts it — while KSail's TrimSpace predicate renders no
			// spec.verify at all. Removing the TrimSpace flips exactly this arm
			// and nothing else.
			name: "whitespace-only provider is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: "  \t "
`,
			wantErr: "DISABLED",
		},
		{
			name: "empty provider is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: ""
`,
			wantErr: "DISABLED",
		},
		{
			// Fail-closed, and the message must name what the file actually
			// says rather than the "" the failed type assertion leaves behind.
			name: "non-string provider is rejected and reported as written",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: 123
`,
			wantErr: `provider is 123, which KSail treats as verification DISABLED`,
		},
		{
			name: "verify block with no provider key is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        matchOIDCIdentity:
          - issuer: '^https://token\.actions\.githubusercontent\.com$'
`,
			wantErr: "DISABLED",
		},
		{
			// A correct live block plus a leftover copy. The live one works
			// today, but the stray invites a cleanup that deletes the wrong one.
			name: "stray copy alongside a correct block is rejected",
			config: `
spec:
  cluster:
    verify:
      provider: cosign
  workload:
    flux:
      verify:
        provider: cosign
`,
			wantErr: "spec.cluster.verify",
		},
		{
			name: "missing verify block entirely is rejected",
			config: `
spec:
  workload:
    flux:
      sourceDirectory: k8s
`,
			wantErr: "no verify block",
		},
		{
			name: "scalar verify at the read path is rejected",
			config: `
spec:
  workload:
    flux:
      verify: true
`,
			wantErr: "not a mapping",
		},
		{
			name:    "unparseable config is rejected",
			config:  "spec:\n\t- broken\n  indent: [",
			wantErr: "does not parse",
		},
		{
			// 🔴 REGRESSION ARM. yaml.Unmarshal reads document one and drops the
			// rest without a word, so this validated CLEAN before: a correct
			// document one, and a stray block in document two that nothing read
			// and nothing reported.
			name: "a second YAML document is rejected",
			config: goodConfig + `
---
spec:
  cluster:
    verify:
      provider: cosign
`,
			wantErr: "only the first is read as the cluster spec",
		},
		{
			// A trailing separator is not a second resource and must not trip the
			// rule above — the real ksail.prod.yaml opens with one.
			name:   "leading and trailing document separators are fine",
			config: "---\n" + goodConfig + "\n---\n",
		},
		{
			// 🔴 REGRESSION ARM. One non-string key switches the WHOLE mapping
			// from map[string]any to map[any]any in yaml.v3, so a walk that
			// matched only the former would skip this level entirely and miss
			// the stray beside it — while the read path below stays valid, so
			// nothing else reports the problem. Fail-open, in a check that only
			// exists to fail closed.
			name: "stray verify beside a non-string key is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
  extras:
    1: numeric-key-demotes-this-mapping
    verify:
      provider: cosign
`,
			wantErr: "spec.extras.verify",
		},
		{
			// Determinism: two strays must be listed in a stable order, or the
			// message differs run to run.
			name: "multiple strays are reported in sorted order",
			config: `
spec:
  aaa:
    verify: {provider: cosign}
  zzz:
    verify: {provider: cosign}
  workload:
    flux:
      verify:
        provider: cosign
`,
			wantErr: "spec.aaa.verify, spec.zzz.verify",
		},
		{
			// A verify key nested inside a list still counts; the walk must not
			// stop at the first mapping level.
			name: "stray verify nested in a sequence is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
  extras:
    - name: a
      verify:
        provider: cosign
`,
			wantErr: "spec.extras",
		},
		{
			// 🔴 #2948. The block is present, at the read path, and "on" by KSail's
			// predicate — and constrains NOBODY. Flux reads a missing secretRef as
			// keyless, so this trusts any keylessly-signed artifact from any signer
			// on the platform's root source. Same failure class the gate exists to
			// catch: configured, in effect, guaranteeing nothing.
			name: "provider-only block is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
`,
			wantErr: "constrains no signer",
		},
		{
			// Presence is not the assertion. An empty list satisfies "the key is
			// there" while matching no identity at all.
			name: "empty matchOIDCIdentity is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity: []
`,
			wantErr: "constrains no signer",
		},
		{
			// A blank issuer leaves the certificate issuer unconstrained, so the
			// subject alone decides trust. Non-emptiness is the assertion.
			name: "matchOIDCIdentity entry with a blank issuer is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity:
          - issuer: '   '
            subject: '^https://github\.com/devantler-tech/platform/.+$'
`,
			wantErr: "constrains no signer",
		},
		{
			// The mirror of the arm above: a blank subject trusts every workflow
			// the issuer will sign for, which on a public OIDC issuer is everyone.
			name: "matchOIDCIdentity entry with a blank subject is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity:
          - issuer: '^https://token\.actions\.githubusercontent\.com$'
            subject: ''
`,
			wantErr: "constrains no signer",
		},
		{
			// One usable entry is enough. A commented-out or half-written sibling
			// must not veto a block that does constrain a signer, or the gate
			// starts failing correct configs.
			name: "one complete matcher among incomplete ones validates",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        matchOIDCIdentity:
          - issuer: ''
            subject: ''
          - issuer: '^https://token\.actions\.githubusercontent\.com$'
            subject: '^https://github\.com/devantler-tech/platform/.+$'
`,
		},
		{
			// Keyed verification constrains the signer without OIDC matchers, so
			// this is a legitimately complete block and must pass. Rejecting it
			// would push a future keyed setup into disabling the gate.
			name: "secretRef-only block validates",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        secretRef:
          name: cosign-public-key
`,
		},
		{
			// 🔴 REGRESSION ARM, and the counter-intuitive one. Flux's own
			// OCIRepository has a trustedRootSecretRef, so this block LOOKS like a
			// complete keyed configuration — but KSail's FluxVerifySpec models only
			// provider, secretRef and matchOIDCIdentity, so the key is dropped in
			// silence and what reaches the cluster is a bare provider that trusts
			// everyone. Accepting it because Flux understands it would reproduce
			// this validator's founding bug: honouring a field the CONSUMER does
			// not read. Rejected until KSail models it.
			name: "trustedRootSecretRef-only block is rejected: KSail does not model that field",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        trustedRootSecretRef:
          name: cosign-trusted-root
`,
			wantErr: "constrains no signer",
		},
		{
			// A secretRef that names nothing constrains nothing — the same
			// presence-is-not-the-assertion rule applied to the keyed branch.
			name: "secretRef with a blank name is rejected",
			config: `
spec:
  workload:
    flux:
      verify:
        provider: cosign
        secretRef:
          name: '  '
`,
			wantErr: "constrains no signer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validate([]byte(test.config))

			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the config to validate, got: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected a failure naming %q, got nil", test.wantErr)
			}

			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected the failure to name %q, got: %v", test.wantErr, err)
			}
		})
	}
}

// TestEnabledMirrorsKSail pins the predicate itself, separately from the
// validator, because this is the one line that must track ksail's
// FluxVerifySpec.Enabled() rather than a local opinion of it.
func TestEnabledMirrorsKSail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "cosign", want: true},
		{provider: "notation", want: true},
		{provider: "", want: false},
		{provider: " ", want: false},
		{provider: "  \t ", want: false},
		{provider: "\n", want: false},
		{provider: " cosign ", want: true},
	}

	for _, test := range tests {
		if got := enabled(test.provider); got != test.want {
			t.Errorf("enabled(%q) = %v, want %v", test.provider, got, test.want)
		}
	}
}

// TestRealConfigValidates is acceptance criterion 2: the check must pass on the
// config production actually deploys, not only on the fixtures above. It reads
// the real file so a future edit that breaks verification fails this test.
func TestRealConfigValidates(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "ksail.prod.yaml")

	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the production cluster config: %v", err)
	}

	if err := validate(config); err != nil {
		t.Fatalf("the production config must have verification in effect, got: %v", err)
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte(goodConfig), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("spec:\n  cluster:\n    verify:\n      provider: cosign\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	instance := filepath.Join(dir, "instance.yaml")
	if err := os.WriteFile(instance, []byte(goodInstance), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	unpatched := filepath.Join(dir, "unpatched.yaml")
	if err := os.WriteFile(unpatched, []byte("kind: FluxInstance\nspec:\n  kustomize:\n    patches: []\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "valid config and instance exits 0", args: []string{good, instance}, want: 0},
		{name: "misplaced block exits 1", args: []string{bad, instance}, want: 1},
		// The half #2922 is about: a correct cluster config beside a
		// FluxInstance that never puts verify on the live root source.
		{name: "unpatched instance exits 1", args: []string{good, unpatched}, want: 1},
		{name: "missing config file exits 1", args: []string{filepath.Join(dir, "absent.yaml"), instance}, want: 1},
		{name: "missing instance file exits 1", args: []string{good, filepath.Join(dir, "absent.yaml")}, want: 1},
		{name: "no arguments exits 1", args: nil, want: 1},
		{name: "one argument exits 1", args: []string{good}, want: 1},
		{name: "three arguments exits 1", args: []string{good, instance, good}, want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stderr strings.Builder
			if got := run(test.args, &stderr); got != test.want {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", test.args, got, test.want, stderr.String())
			}
		})
	}
}
