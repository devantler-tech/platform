package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const recoveryBaseImage = "ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c"
const recoveryRuntimeRepository = "ghcr.io/devantler-tech/platform-private-mail-bootstrap"

// The deployed bootstrap digest has one reviewed declarative owner. Its
// underlying server binary must still match the datastore format proved here.
func declaredRecoveryImage(declaration, dockerfile io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(declaration, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", errors.New("image-declaration")
	}
	var config struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Images     []struct {
			Name    string `yaml:"name"`
			Digest  string `yaml:"digest"`
			NewName string `yaml:"newName"`
			NewTag  string `yaml:"newTag"`
		} `yaml:"images"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&config) != nil || config.APIVersion != "kustomize.config.k8s.io/v1beta1" || config.Kind != "Kustomization" {
		return "", errors.New("image-declaration")
	}
	var trailing yaml.Node
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return "", errors.New("image-declaration-count")
	}
	image := ""
	for _, entry := range config.Images {
		if entry.Name != recoveryRuntimeRepository {
			continue
		}
		if image != "" || entry.NewName != "" || entry.NewTag != "" || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(entry.Digest) {
			return "", errors.New("image-pin")
		}
		image = entry.Name + "@" + entry.Digest
	}
	if image == "" {
		return "", errors.New("image-missing")
	}
	data, err = io.ReadAll(io.LimitReader(dockerfile, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", errors.New("binary-base")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lastBase := ""
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 && strings.EqualFold(fields[0], "FROM") {
			if len(fields) != 2 && (len(fields) != 4 || !strings.EqualFold(fields[2], "AS")) {
				return "", errors.New("binary-base")
			}
			lastBase = fields[1]
		}
	}
	if scanner.Err() != nil || lastBase != recoveryBaseImage {
		return "", errors.New("binary-compatibility")
	}
	return image, nil
}
func loadRecoveryImage() (string, error) {
	declaration, err := os.Open("k8s/bases/apps/private-mail-administration/kustomization.yaml")
	if err != nil {
		return "", errors.New("image-declaration")
	}
	defer func() { _ = declaration.Close() }()
	dockerfile, err := os.Open("scripts/private-mail-config/Dockerfile")
	if err != nil {
		return "", errors.New("binary-base")
	}
	defer func() { _ = dockerfile.Close() }()
	return declaredRecoveryImage(declaration, dockerfile)
}
