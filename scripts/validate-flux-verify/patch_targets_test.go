package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const patchTargetsManifest = `apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  components:
    - source-controller
    - kustomize-controller
  kustomize:
    patches:
      - target:
          kind: Deployment
          name: kustomize-controller
        patch: replicas
      - target:
          kind: Deployment
          labelSelector: app.kubernetes.io/part-of=flux
        patch: spread
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: verify
`

func TestPatchTargetsAcceptEveryGeneratedResource(t *testing.T) {
	if err := validatePatchTargets([]byte(patchTargetsManifest)); err != nil {
		t.Fatalf("validatePatchTargets() error = %v", err)
	}
}

func TestPatchTargetsAcceptTheOperatorDefaults(t *testing.T) {
	cases := map[string]struct{ old, new string }{
		// flux-operator deploys the four core controllers when components is omitted.
		"omitted components": {
			old: "  components:\n    - source-controller\n    - kustomize-controller\n",
			new: "",
		},
		// An explicit empty list gets the same defaults as an omitted one.
		"empty components": {
			old: "  components:\n    - source-controller\n    - kustomize-controller\n",
			new: "  components: []\n",
		},
		"explicit matching group and version": {
			old: "kind: Deployment\n          name: kustomize-controller",
			new: "group: apps\n          version: v1\n          kind: Deployment\n          name: kustomize-controller",
		},
		"target-less strategic merge naming a controller": {
			old: "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - patch: |\n          apiVersion: apps/v1\n          kind: Deployment\n          metadata:\n            name: source-controller\n",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(patchTargetsManifest, testCase.old, testCase.new, 1)
			if mutated == patchTargetsManifest {
				t.Fatalf("fixture did not change for %q", testCase.old)
			}
			if err := validatePatchTargets([]byte(mutated)); err != nil {
				t.Fatalf("validatePatchTargets() error = %v", err)
			}
		})
	}
}

func TestPatchTargetsRejectATargetThatSelectsNothing(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{
			name: "undeclared component name",
			old:  "name: kustomize-controller\n        patch",
			new:  "name: kustomize-controllr\n        patch",
			want: `no component named "kustomize-controllr"`,
		},
		{
			// A typo shared by spec.components and the target must not certify it.
			name: "unsupported component declared and targeted",
			old:  "    - kustomize-controller\n  kustomize",
			new:  "    - kustomize-controller\n    - kustomize-controllr\n  kustomize",
			want: `spec.components names "kustomize-controllr", which flux-operator does not deploy`,
		},
		{
			name: "blank component entry",
			old:  "    - kustomize-controller\n  kustomize",
			new:  "    - kustomize-controller\n    - \"\"\n  kustomize",
			want: `spec.components names "", which flux-operator does not deploy`,
		},
		{
			// flux-operator uses the raw entry, so a padded name deploys nothing.
			name: "padded component name",
			old:  "    - kustomize-controller\n  kustomize",
			new:  "    - \"kustomize-controller \"\n  kustomize",
			want: `spec.components entry "kustomize-controller " carries surrounding whitespace`,
		},
		{
			name: "component removed from spec.components",
			old:  "    - kustomize-controller\n  kustomize",
			new:  "  kustomize",
			want: `no component named "kustomize-controller"`,
		},
		{
			name: "label selector the controllers do not carry",
			old:  "app.kubernetes.io/part-of=flux",
			new:  "app.kubernetes.io/part-of=fluxcd",
			want: `labelSelector "app.kubernetes.io/part-of=fluxcd"`,
		},
		{
			name: "wrong kind",
			old:  "kind: Deployment\n          name: kustomize-controller",
			new:  "kind: StatefulSet\n          name: kustomize-controller",
			want: `kind "StatefulSet"`,
		},
		{
			name: "foreign namespace",
			old:  "name: kustomize-controller\n        patch",
			new:  "name: kustomize-controller\n          namespace: kube-system\n        patch",
			want: `namespace "kube-system"`,
		},
		{
			name: "controller target in the wrong group",
			old:  "kind: Deployment\n          name: kustomize-controller",
			new:  "group: batch\n          kind: Deployment\n          name: kustomize-controller",
			want: `group "batch" is not "apps"`,
		},
		{
			name: "root source target at the wrong version",
			old:  "kind: OCIRepository\n          name: flux-system",
			new:  "version: v1beta2\n          kind: OCIRepository\n          name: flux-system",
			want: `version "v1beta2" is not "v1"`,
		},
		{
			name: "padded name",
			old:  "name: kustomize-controller\n        patch",
			new:  "name: \"kustomize-controller \"\n        patch",
			want: "name carries surrounding whitespace",
		},
		{
			name: "padded kind",
			old:  "kind: Deployment\n          name: kustomize-controller",
			new:  "kind: \" Deployment\"\n          name: kustomize-controller",
			want: "kind carries surrounding whitespace",
		},
		{
			// flux-operator applies component patches before the namespace
			// transformer, so even the controllers' own namespace matches nothing.
			name: "namespaced deployment target",
			old:  "name: kustomize-controller\n        patch",
			new:  "name: kustomize-controller\n          namespace: flux-system\n        patch",
			want: `namespace "flux-system" on a Deployment matches nothing`,
		},
		{
			name: "second name-targeted patch on one controller",
			old:  "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - target:\n          kind: Deployment\n          name: kustomize-controller\n        patch: more\n",
			want: `already targets "kustomize-controller" by name`,
		},
		{
			name: "target-less strategic merge naming an undeclared controller",
			old:  "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - patch: |\n          apiVersion: apps/v1\n          kind: Deployment\n          metadata:\n            name: image-reflector-controller\n",
			want: `no component named "image-reflector-controller"`,
		},
		{
			name: "target-less strategic merge with a core apiVersion",
			old:  "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - patch: |\n          apiVersion: v1\n          kind: Deployment\n          metadata:\n            name: source-controller\n",
			want: `group "" is not "apps"`,
		},
		{
			name: "target-less strategic merge without an apiVersion",
			old:  "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - patch: |\n          kind: Deployment\n          metadata:\n            name: source-controller\n",
			want: `group "" is not "apps"`,
		},
		{
			name: "target-less JSON6902 list",
			old:  "        patch: verify\n",
			new: "        patch: verify\n" +
				"      - patch: |\n          - op: add\n            path: /spec/replicas\n            value: 2\n",
			want: "JSON6902 operation list without one selects nothing",
		},
		{
			name: "deployment with neither name nor selector",
			old:  "          labelSelector: app.kubernetes.io/part-of=flux\n",
			new:  "",
			want: "neither a name nor a labelSelector",
		},
		{
			name: "name and selector together",
			old:  "          labelSelector: app.kubernetes.io/part-of=flux\n",
			new:  "          name: helm-controller\n          labelSelector: app.kubernetes.io/part-of=flux\n",
			want: "names a Deployment and a labelSelector at once",
		},
		{
			name: "annotation selector",
			old:  "          labelSelector: app.kubernetes.io/part-of=flux\n",
			new:  "          labelSelector: app.kubernetes.io/part-of=flux\n          annotationSelector: team=x\n",
			want: "annotationSelector",
		},
		{
			name: "root source misnamed",
			old:  "kind: OCIRepository\n          name: flux-system",
			new:  "kind: OCIRepository\n          name: flux-root",
			want: `kind "OCIRepository"`,
		},
		{
			name: "patch without target",
			old:  "      - target:\n          kind: Deployment\n          name: kustomize-controller\n        patch: replicas\n",
			new:  "      - patch: replicas\n",
			want: "has no target",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := strings.Replace(patchTargetsManifest, testCase.old, testCase.new, 1)
			if mutated == patchTargetsManifest {
				t.Fatalf("fixture did not change for %q", testCase.old)
			}
			err := validatePatchTargets([]byte(mutated))
			if err == nil {
				t.Fatal("a patch that selects no generated resource was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to contain %q", err, testCase.want)
			}
		})
	}
}

// TestCommittedFluxInstancesPatchOnlyGeneratedResources pins both providers'
// real FluxInstances, including the docker one CI does not pass on the command
// line.
func TestCommittedFluxInstancesPatchOnlyGeneratedResources(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "k8s", "providers", "*", "infrastructure", "controllers", "flux-instance", "flux-instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 2 {
		t.Fatalf("found %d committed FluxInstances, want both providers: %v", len(paths), paths)
	}
	for _, path := range paths {
		manifest, err := os.ReadFile(path) //nolint:gosec // Repository path from a fixed glob.
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePatchTargets(manifest); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
