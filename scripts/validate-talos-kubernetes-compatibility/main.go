package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"gopkg.in/yaml.v3"
)

var versionPin = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

func main() {
	path := "ksail.prod.yaml"
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: validate-talos-kubernetes-compatibility [ksail-config]")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		path = os.Args[1]
	}
	if err := validate(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: pinned Kubernetes/Talos versions are compatible\n", path)
}

func validate(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read version pins: %w", err)
	}
	var config struct {
		Spec struct {
			Cluster struct {
				Kubernetes string `yaml:"kubernetesVersion"`
				Talos      struct {
					Version string `yaml:"version"`
				} `yaml:"talos"`
			} `yaml:"cluster"`
		} `yaml:"spec"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("parse version pins: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected exactly one YAML document", path)
	}
	talosPin, kubernetesPin := config.Spec.Cluster.Talos.Version, config.Spec.Cluster.Kubernetes
	if !versionPin.MatchString(talosPin) || !versionPin.MatchString(kubernetesPin) {
		return fmt.Errorf("%s: explicit vMAJOR.MINOR.PATCH Talos and Kubernetes pins are required (got Talos %q, Kubernetes %q)", path, talosPin, kubernetesPin)
	}
	talos, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: talosPin})
	if err != nil {
		return fmt.Errorf("parse Talos pin %q: %w", talosPin, err)
	}
	kubernetes, err := compatibility.ParseKubernetesVersion(kubernetesPin)
	if err != nil {
		return fmt.Errorf("parse Kubernetes pin %q: %w", kubernetesPin, err)
	}
	// Talos calls this same upstream predicate from RuntimeValidate. The offline
	// talosctl validate command does not call it, even with --strict. Unknown
	// Talos release families fail closed rather than inheriting an old ceiling.
	if err := kubernetes.SupportedWith(talos); err != nil {
		return fmt.Errorf("%s: Kubernetes %s is not verified compatible with Talos %s: %w. Choose compatible pins; complete any Talos upgrade separately before raising Kubernetes. If the Talos release is unknown, update the reviewed machinery dependency and revalidate", path, kubernetesPin, talosPin, err)
	}
	return nil
}
