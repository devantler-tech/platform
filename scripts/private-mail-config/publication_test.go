package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	write(filepath.Join(assetDir, "webui-v1-pins.json"), `{
  "version": "2.0.4",
  "url": "https://github.com/stalwartlabs/webui/releases/download/v2.0.4/webui.zip",
  "sha256": "73dd270926cf548ff40a41a8f7a523667df1f2cd665ed680de45711a1c2d04b2",
  "license_sha256": "a7c6bf8df7d8fb707af606599f275e0eb192cd28492c95593b0608ca966c8df1"
}`, 0644)
	write(filepath.Join(f.root, "asset-source"), "verified admin asset fixture\n", 0644)
	write(filepath.Join(f.root, "asset-v1-source"), "verified 1.0 admin asset fixture\n", 0644)
	write(filepath.Join(f.root, "license-source"), "fixture license\n", 0644)
	write(filepath.Join(f.bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
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
  https://github.com/stalwartlabs/webui/releases/download/v2.0.4/webui.zip)
    cp "$ASSET_FIXTURE_ROOT/asset-v1-source" "$output" ;;
  https://raw.githubusercontent.com/stalwartlabs/webui/v1.0.11/LICENSES/AGPL-3.0-only.txt|https://raw.githubusercontent.com/stalwartlabs/webui/v2.0.4/LICENSES/AGPL-3.0-only.txt)
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
			Steps    []struct{ Name, Run string }
			Strategy struct {
				Matrix struct {
					Include []struct{ Runtime, Server, CLI, Image string }
				}
			}
		}
	}
	if yaml.Unmarshal(data, &workflow) != nil {
		t.Fatal("publication workflow could not be decoded")
	}
	for _, job := range []string{"verify", "publish"} {
		t.Run(job, func(t *testing.T) {
			var script string
			for _, step := range workflow.Jobs[job].Steps {
				if step.Name == "Build static initializer" {
					script = step.Run
				}
			}
			if script == "" {
				t.Fatal("publication producer is missing")
			}
			profiles := workflow.Jobs[job].Strategy.Matrix.Include
			if len(profiles) != 2 {
				t.Fatalf("publication must cover the deployed and candidate versions, got %d profiles", len(profiles))
			}
			for i, runtime := range []struct{ version, cli, asset, image string }{
				{"0.16.27", "1.0.13", "verified admin asset fixture\n", "ghcr.io/devantler-tech/platform-private-mail-bootstrap"},
				{"1.0.3", "1.0.14", "verified 1.0 admin asset fixture\n", "ghcr.io/devantler-tech/platform-private-mail-bootstrap-candidate"},
			} {
				t.Run(runtime.version, func(t *testing.T) {
					profile := profiles[i]
					if profile.Runtime != runtime.version || !strings.HasPrefix(profile.Server, "v"+runtime.version+"@sha256:") ||
						!strings.HasPrefix(profile.CLI, runtime.cli+"@sha256:") || profile.Image != runtime.image {
						t.Fatal("runtime must use the matching pinned server, CLI and publication repository")
					}
					f := publicationFixture(t)
					if output, err := f.run(script, "STALWART_RUNTIME="+profile.Runtime,
						"STALWART_SERVER_PIN="+profile.Server, "STALWART_CLI_PIN="+profile.CLI); err != nil {
						t.Fatalf("publication producer failed: %v: %s", err, output)
					}
					buildRecipe, err := os.ReadFile(filepath.Join(f.runner, "private-mail-build", "Dockerfile"))
					if err != nil || !strings.Contains(string(buildRecipe), "FROM ghcr.io/stalwartlabs/stalwart:"+profile.Server+"\n") ||
						!strings.Contains(string(buildRecipe), "FROM ghcr.io/stalwartlabs/cli:"+profile.CLI+" AS cli\n") {
						t.Fatal("staged recipe does not select the required server and CLI")
					}
					for name, want := range map[string]string{"webui.zip": runtime.asset, "webui.LICENSE": "fixture license\n"} {
						got, err := os.ReadFile(filepath.Join(f.runner, "private-mail-build", name))
						if err != nil || string(got) != want {
							t.Fatalf("publication did not stage the selected administration artifact %s", name)
						}
					}
				})
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
