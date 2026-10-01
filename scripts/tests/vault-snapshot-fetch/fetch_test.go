package snapshotfetch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type container struct {
	Name         string                         `yaml:"name"`
	Image        string                         `yaml:"image"`
	Command      []string                       `yaml:"command"`
	Env          []struct{ Name, Value string } `yaml:"env"`
	VolumeMounts []struct {
		Name      string
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"volumeMounts"`
}

func fetcher(t *testing.T) ([]container, container) {
	t.Helper()
	b, err := os.ReadFile("../../../k8s/bases/infrastructure/vault-config/job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []container `yaml:"initContainers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &job); err != nil {
		t.Fatal(err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "fetch-snapshot" {
			return job.Spec.Template.Spec.InitContainers, c
		}
	}
	t.Fatal("snapshot fetcher missing")
	return nil, container{}
}

// Missing awk/grep in the replacement mc image must not silently turn a
// populated recovery mirror into an empty snapshot directory.
func TestFetcherHasPinnedClientAndPreinstalledShellTools(t *testing.T) {
	init, fetch := fetcher(t)
	if !strings.HasPrefix(fetch.Image, "quay.io/minio/aistor/mc:") || !strings.Contains(fetch.Image, "@sha256:") {
		t.Fatal("snapshot fetch still uses the unavailable legacy client")
	}
	installed := false
	for _, c := range init {
		if c.Name == fetch.Name {
			break
		}
		if c.Name == "install-fetch-tools" && strings.HasPrefix(c.Image, "docker.io/library/busybox:") && strings.Contains(c.Image, "-musl@sha256:") {
			for _, m := range c.VolumeMounts {
				if m.Name == "fetch-tools" && m.MountPath == "/tools" && !m.ReadOnly {
					installed = true
				}
			}
		}
	}
	if !installed {
		t.Fatal("static shell tools are not installed before snapshot fetch")
	}
	if len(fetch.Command) != 3 || fetch.Command[0] != "/tools/sh" {
		t.Fatal("fetch must run the supplied shell")
	}
	path := false
	for _, e := range fetch.Env {
		if e.Name == "PATH" && strings.HasPrefix(e.Value, "/tools:") {
			path = true
		}
	}
	if !path {
		t.Fatal("snapshot selection tools are absent from PATH")
	}
	mounted := false
	for _, m := range fetch.VolumeMounts {
		if m.Name == "fetch-tools" && m.MountPath == "/tools" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("fetcher must consume its installed tools read-only")
	}
}

func TestRenderedFetcherDownloadsOnlyNewestSnapshot(t *testing.T) {
	_, fetch := fetcher(t)
	for _, tc := range []struct {
		name, listing, want string
		credentials         bool
		listFails           bool
	}{
		{"latest", "[date] 1B openbao-20260929-020000.snap\n[date] 1B readme.txt\n[date] 1B openbao-20261001-020000.snap\n[date] 1B openbao-20260930-020000.snap\n", "openbao-20261001-020000.snap", true, false},
		{"empty", "", "", true, false},
		{"not-snapshots", "[date] 1B notes.txt\n", "", true, false},
		{"missing-secret", "", "", false, false},
		{"unreachable-mirror", "", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			creds, snapshots := filepath.Join(dir, "r2"), filepath.Join(dir, "snapshots")
			for _, d := range []string{creds, snapshots} {
				if err := os.Mkdir(d, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.credentials {
				for _, key := range []string{"access_key_id", "secret_access_key"} {
					if err := os.WriteFile(filepath.Join(creds, key), []byte("dummy"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "listing"), []byte(tc.listing), 0600); err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\ncase \"$1\" in\n alias) [ \"$2\" = set ] && [ \"$3\" = backup ] && [ \"$4\" = http://fixture ] && [ \"$5\" = dummy ] && [ \"$6\" = dummy ];;\n ls) [ \"$2\" = backup/fixture/openbao-snapshots/ ] || exit 2; [ \"$LIST_FAILS\" = false ] || exit 1; cat \"$FIXTURE_DIR/listing\";;\n cp) [ \"$2\" = \"backup/fixture/openbao-snapshots/$EXPECTED_SNAPSHOT\" ] || exit 2; printf snapshot-fixture >\"$3\";;\n *) exit 2;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "mc"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			body := strings.NewReplacer("/r2/", creds+"/", "/snapshots/", snapshots+"/", "${r2_endpoint}", "http://fixture", "${r2_bucket}", "fixture").Replace(fetch.Command[2])
			cmd := exec.Command("sh", "-ec", body)
			fails := "false"
			if tc.listFails {
				fails = "true"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "FIXTURE_DIR="+dir, "LIST_FAILS="+fails, "EXPECTED_SNAPSHOT="+tc.want)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fetch failed: %v\n%s", err, out)
			}
			entries, err := os.ReadDir(snapshots)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(entries) != 0 {
					t.Fatal("unexpected snapshot")
				}
				return
			}
			if len(entries) != 1 || entries[0].Name() != tc.want {
				t.Fatalf("wrong selected snapshot: %v", entries)
			}
			b, err := os.ReadFile(filepath.Join(snapshots, tc.want))
			if err != nil || string(b) != "snapshot-fixture" {
				t.Fatal("snapshot download did not reach the recovery directory")
			}
		})
	}
}
