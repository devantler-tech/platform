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

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "valid config exits 0", args: []string{good}, want: 0},
		{name: "misplaced block exits 1", args: []string{bad}, want: 1},
		{name: "missing file exits 1", args: []string{filepath.Join(dir, "absent.yaml")}, want: 1},
		{name: "no arguments exits 1", args: nil, want: 1},
		{name: "two arguments exits 1", args: []string{good, good}, want: 1},
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
