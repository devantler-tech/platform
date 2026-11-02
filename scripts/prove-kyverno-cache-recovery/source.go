package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

func reviewedSpec(raw []byte) ([]byte, error) {
	var manifest struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec map[string]any `yaml:"spec"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if len(raw) > 1024*1024 || decoder.Decode(&manifest) != nil || manifest.APIVersion != "policies.kyverno.io/v1" || manifest.Kind != "ImageValidatingPolicy" || manifest.Metadata.Name != "verify-app-images" || manifest.Spec == nil {
		return nil, errors.New("fixed reviewed policy source is unqualified")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("fixed reviewed policy contains another document")
	}
	result, err := json.Marshal(manifest.Spec)
	if err != nil {
		return nil, errors.New("fixed reviewed policy spec cannot be encoded")
	}
	return result, nil
}

func loadReviewedSpec() ([]byte, error) {
	file, err := os.Open("k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml")
	if err != nil {
		return nil, errors.New("fixed reviewed policy source is unavailable")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.New("fixed reviewed policy read did not complete")
	}
	return reviewedSpec(raw)
}
