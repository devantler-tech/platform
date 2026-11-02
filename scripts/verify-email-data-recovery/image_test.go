package main

import (
	"strings"
	"testing"
)

const fixtureImage = "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestImageFactUsesTheDeclaredDigestAndCompatibleBinaryBase(t *testing.T) {
	declaration := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
images:
  - name: ghcr.io/devantler-tech/platform-private-mail-bootstrap
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
`
	dockerfile := "FROM other.example/build:1 AS build\nFROM " + recoveryBaseImage + "\nUSER 2000:2000\n"
	image, err := declaredRecoveryImage(strings.NewReader(declaration), strings.NewReader(dockerfile))
	if err != nil || image != fixtureImage {
		t.Fatal("reviewed runtime digest and compatible binary base were refused")
	}
	for name, input := range map[string]string{
		"unpinned":        strings.ReplaceAll(declaration, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "latest"),
		"foreign-image":   strings.ReplaceAll(declaration, "ghcr.io/devantler-tech/platform-private-mail-bootstrap", "other.example/foreign"),
		"duplicate":       declaration + "  - name: ghcr.io/devantler-tech/platform-private-mail-bootstrap\n    digest: sha256:2222222222222222222222222222222222222222222222222222222222222222\n",
		"second-document": declaration + "---\nkind: Kustomization\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := declaredRecoveryImage(strings.NewReader(input), strings.NewReader(dockerfile)); err == nil {
				t.Fatal("ambiguous or unreviewed image fact was accepted")
			}
		})
	}
	if _, err := declaredRecoveryImage(strings.NewReader(declaration), strings.NewReader("FROM other.example/incompatible:1\n")); err == nil {
		t.Fatal("an incompatible datastore reader was accepted")
	}
}

func TestCurrentProductionDeclarationProvidesOneCompatibleRecoveryImage(t *testing.T) {
	t.Chdir("../..")
	image, err := loadRecoveryImage()
	if err != nil || !strings.HasPrefix(image, "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:") {
		t.Fatal("current production declarations do not provide a compatible fixed reader")
	}
}
