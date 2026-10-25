package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

type assetPublicationFixture struct{ root, runner, bin string }

func publicationFixture(t *testing.T) assetPublicationFixture {
	t.Helper()
	f := assetPublicationFixture{root: t.TempDir()}
	f.runner, f.bin = filepath.Join(f.root, "runner"), filepath.Join(f.root, "bin")
	assetDir := filepath.Join(f.root, "scripts", "private-mail-config")
	for _, dir := range []string{f.runner, f.bin, assetDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, value string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Dockerfile", "stage-webui.sh"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(assetDir, name), string(data), 0755)
	}
	pins, err := json.Marshal(map[string]string{
		"version": "1.0.11", "url": "https://github.com/stalwartlabs/webui/releases/download/v1.0.11/webui.zip",
		"sha256":         "c8ab5014a65262af4c57ecf7602ffb31a6f6c67b5c6cb9f85f8a043e03e2f3bc",
		"license_sha256": "a7c6bf8df7d8fb707af606599f275e0eb192cd28492c95593b0608ca966c8df1",
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(assetDir, "webui-pins.json"), string(pins), 0644)
	write(filepath.Join(f.root, "asset-source"), "verified admin asset fixture\n", 0644)
	write(filepath.Join(f.root, "license-source"), "fixture license\n", 0644)
	write(filepath.Join(f.bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  test) exit 0 ;;
  build)
    output=''
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -o ]; then output=$2; shift 2; else shift; fi
    done
    [ "$output" = "$RUNNER_TEMP/private-mail-build/private-mail-bootstrap" ] || exit 64
    printf 'synthetic compiler output\n' >"$output"
    ;;
  *) exit 64 ;;
esac
`, 0755)
	write(filepath.Join(f.bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
[ "$*" = "buildx build --platform linux/amd64 $RUNNER_TEMP/private-mail-build" ]
test -s "$RUNNER_TEMP/private-mail-build/private-mail-bootstrap"
`, 0755)
	write(filepath.Join(f.bin, "curl"), `#!/usr/bin/env bash
set -euo pipefail
output='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --max-time|--max-filesize|--proto|--proto-redir) shift 2 ;;
    --disable|--fail|--silent|--show-error|--location) shift ;;
    https://*) url=$1; shift ;;
    *) exit 64 ;;
  esac
done
case "$output" in "$RUNNER_TEMP/"*) ;; *) exit 64 ;; esac
case "$url" in
  https://github.com/stalwartlabs/webui/releases/download/v1.0.11/webui.zip)
    if [ "${FAKE_DOWNLOAD_BAD:-}" = zip ]; then printf 'altered' >"$output"; else cp "$ASSET_FIXTURE_ROOT/asset-source" "$output"; fi ;;
  https://raw.githubusercontent.com/stalwartlabs/webui/v1.0.11/LICENSES/AGPL-3.0-only.txt)
    if [ "${FAKE_DOWNLOAD_BAD:-}" = license ]; then printf 'altered' >"$output"; else cp "$ASSET_FIXTURE_ROOT/license-source" "$output"; fi ;;
  *) exit 65 ;;
esac
`, 0755)
	return f
}

func (f assetPublicationFixture) run(script string, extra ...string) ([]byte, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(), "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RUNNER_TEMP="+f.runner, "ASSET_FIXTURE_ROOT="+f.root, "FAKE_DOWNLOAD_BAD=", "BASH_ENV=", "ENV=")
	cmd.Env = append(cmd.Env, extra...)
	return cmd.CombinedOutput()
}

func TestBothPublicationPathsStageTheAdministrationArtifacts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-private-mail-bootstrap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Run string }
		}
	}
	if yaml.Unmarshal(data, &workflow) != nil {
		t.Fatal("publication workflow could not be decoded")
	}
	for _, tc := range []struct{ job, step string }{
		{"verify", "Test and build static initializer"}, {"publish", "Build static initializer"},
	} {
		t.Run(tc.job, func(t *testing.T) {
			var script string
			for _, step := range workflow.Jobs[tc.job].Steps {
				if step.Name == tc.step {
					script = step.Run
				}
			}
			if script == "" {
				t.Fatal("publication producer is missing")
			}
			f := publicationFixture(t)
			if output, err := f.run(script); err != nil {
				t.Fatalf("publication producer failed: %v: %s", err, output)
			}
			for name, want := range map[string]string{"webui.zip": "verified admin asset fixture\n", "webui.LICENSE": "fixture license\n"} {
				got, err := os.ReadFile(filepath.Join(f.runner, "private-mail-build", name))
				if err != nil || string(got) != want {
					t.Fatalf("publication did not stage the required administration artifact %s", name)
				}
			}
		})
	}
}

func TestAssetStagingRefusesAlteredDownloadsWithoutPublishedFiles(t *testing.T) {
	for _, bad := range []string{"zip", "license"} {
		t.Run(bad, func(t *testing.T) {
			f := publicationFixture(t)
			output := filepath.Join(f.runner, "private-mail-build")
			if err := os.Mkdir(output, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := f.run(`bash scripts/private-mail-config/stage-webui.sh "$RUNNER_TEMP/private-mail-build"`, "FAKE_DOWNLOAD_BAD="+bad); err == nil {
				t.Fatal("altered administration download was staged successfully")
			}
			for _, name := range []string{"webui.zip", "webui.LICENSE", "webui.SOURCE"} {
				if _, err := os.Lstat(filepath.Join(output, name)); !os.IsNotExist(err) {
					t.Fatal("a rejected download left a published administration artifact")
				}
			}
			left, err := filepath.Glob(filepath.Join(output, ".webui-stage.*"))
			if err != nil || len(left) != 0 {
				t.Fatal("rejected download temporary files were not cleaned")
			}
		})
	}
}

func TestAssetStagingPreservesExistingBuildArtifacts(t *testing.T) {
	for _, existing := range []string{"webui.zip", "webui.LICENSE", "webui.SOURCE"} {
		t.Run(existing, func(t *testing.T) {
			f := publicationFixture(t)
			output := filepath.Join(f.runner, "private-mail-build")
			if err := os.Mkdir(output, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(output, existing)
			if err := os.WriteFile(path, []byte("pre-existing artifact\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := f.run(`bash scripts/private-mail-config/stage-webui.sh "$RUNNER_TEMP/private-mail-build"`); err == nil {
				t.Fatal("staging overwrote an existing build artifact")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "pre-existing artifact\n" {
				t.Fatal("existing artifact contents changed")
			}
			for _, name := range []string{"webui.zip", "webui.LICENSE", "webui.SOURCE"} {
				if name != existing {
					if _, err := os.Lstat(filepath.Join(output, name)); !os.IsNotExist(err) {
						t.Fatal("refused output reuse left new publication artifacts")
					}
				}
			}
		})
	}
}
