package emailcredentials_test

import (
	"bytes"
	"io"
	"os/exec"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionRecoveryCannotSeedFromRegeneratedCredentialCaches(t *testing.T) {
	for _, test := range []struct {
		path string
		seed bool
	}{
		{"k8s/bases/apps/private-mail-administration", true},
		{"k8s/providers/hetzner/apps/private-mail", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			command := exec.Command("kubectl", "kustomize", test.path)
			command.Dir = repositoryRoot()
			raw, err := command.Output()
			if err != nil {
				t.Fatal("cannot render the declared credential mode")
			}
			decoder := yaml.NewDecoder(bytes.NewReader(raw))
			seeded, readers := map[string]bool{}, map[string]bool{}
			wanted := map[string]bool{"infrastructure/email/admin": true, "infrastructure/email/maintainer": true, "infrastructure/email/agent": true}
			for {
				var object struct {
					Kind     string
					Metadata struct{ Namespace string }
					Spec     struct {
						UpdatePolicy   string `yaml:"updatePolicy"`
						DeletionPolicy string `yaml:"deletionPolicy"`
						Data           []struct {
							RemoteRef struct{ Key string } `yaml:"remoteRef"`
							Match     struct {
								RemoteRef struct {
									RemoteKey string `yaml:"remoteKey"`
								} `yaml:"remoteRef"`
							}
						}
					}
				}
				if err := decoder.Decode(&object); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal("cannot decode the declared credential mode")
				}
				if object.Metadata.Namespace != "private-mail" {
					continue
				}
				for _, data := range object.Spec.Data {
					if object.Kind == "PushSecret" && wanted[data.Match.RemoteRef.RemoteKey] {
						if !test.seed {
							t.Fatal("restored production can seed a missing durable record from a regenerated cache")
						}
						if seeded[data.Match.RemoteRef.RemoteKey] || object.Spec.UpdatePolicy != "IfNotExists" || object.Spec.DeletionPolicy != "None" {
							t.Fatal("fresh initialization lost its create-only retained seed")
						}
						seeded[data.Match.RemoteRef.RemoteKey] = true
					}
					if object.Kind == "ExternalSecret" && wanted[data.RemoteRef.Key] {
						readers[data.RemoteRef.Key] = true
					}
				}
			}
			if len(readers) != 3 || test.seed && len(seeded) != 3 || !test.seed && len(seeded) != 0 {
				t.Fatal("credential modes lost their complete durable readers or fresh initialization")
			}
		})
	}
}
