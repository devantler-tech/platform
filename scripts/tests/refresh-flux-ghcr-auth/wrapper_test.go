package refreshfluxghcrauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func requireWrapperExitCode(t *testing.T, result commandResult, expected int) {
	t.Helper()
	if result.exitCode != expected {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", result.exitCode, expected, result.stdout, result.stderr)
	}
}

func requireWrapperPathExists(t *testing.T, path string, expected bool) {
	t.Helper()
	if actual := pathExists(path); actual != expected {
		t.Errorf("pathExists(%q) = %t, want %t", path, actual, expected)
	}
}

func requireWrapperFileEquals(t *testing.T, path, expected string) {
	t.Helper()
	if actual := mustRead(path); actual != expected {
		t.Errorf("%s = %q, want %q", path, actual, expected)
	}
}

func requireWrapperSecretAbsent(t *testing.T, result commandResult, secret string) {
	t.Helper()
	if strings.Contains(result.stdout+result.stderr, secret) {
		t.Errorf("command output exposed secret %q", secret)
	}
}

func TestKSailLifecycleWrapperUsesOnlySOPSPullToken(t *testing.T) {
	info, err := os.Stat(ksailPullWrapperPath)
	if err != nil {
		t.Fatalf("stat production KSail wrapper: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("production KSail wrapper %q is not a regular file", ksailPullWrapperPath)
	}

	commands := [][]string{
		{"cluster", "create"},
		{"workload", "reconcile"},
		{"cluster", "update"},
	}
	for _, command := range commands {
		command := command
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			f := newFixture(t)
			result := f.runKSailPullWrapper(validConfig(), command, nil)

			requireWrapperExitCode(t, result, 0)
			requireWrapperFileEquals(t, f.ksailTokenCapture, "fixture-secret-token")
			requireWrapperFileEquals(t, f.ksailUsernameCapture, "devantler")
			wantCommand := append([]string{"--config", "ksail.prod.yaml"}, command...)
			if got := strings.Fields(strings.TrimSpace(mustRead(f.ksailCommandCapture))); !slicesEqual(got, wantCommand) {
				t.Errorf("KSail command = %q, want %q", got, wantCommand)
			}
			requireWrapperFileEquals(t, f.ksailConfigPathCapture, "ksail.prod.yaml")
			requireWrapperFileEquals(t, f.ksailRegistryCapture, "devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests")
			requireWrapperFileEquals(t, f.ksailRegistryOverrideCapture, "${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests")
			if revision := mustRead(f.ksailRevisionCapture); !sha256HexPattern.MatchString(revision) {
				t.Errorf("KSail pull revision = %q, want 64 lowercase hex characters", revision)
			}
			requireWrapperSecretAbsent(t, result, "fixture-secret-token")
			temporaryConfig := mustRead(f.outputPathLog)
			requireWrapperPathExists(t, temporaryConfig, false)
		})
	}
}

func TestKSailPublishWrapperPreservesActionsWriteToken(t *testing.T) {
	f := newFixture(t)
	result := f.runKSailPullWrapper(
		validConfig(),
		[]string{"workload", "push"},
		map[string]string{
			"GITHUB_ACTOR": "fixture-publisher",
			"GHCR_TOKEN":   "fixture-actions-write-token",
		},
	)

	requireWrapperExitCode(t, result, 0)
	requireWrapperFileEquals(t, f.ksailTokenCapture, "fixture-actions-write-token")
	requireWrapperFileEquals(t, f.ksailUsernameCapture, "fixture-publisher")
	requireWrapperFileEquals(t, f.ksailConfigPathCapture, "ksail.prod.yaml")
	requireWrapperFileEquals(t, f.ksailRegistryCapture, "devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests")
	requireWrapperFileEquals(t, f.ksailRegistryOverrideCapture, "")
	if revision := mustRead(f.ksailRevisionCapture); !sha256HexPattern.MatchString(revision) {
		t.Errorf("KSail pull revision = %q, want 64 lowercase hex characters", revision)
	}
	requireWrapperSecretAbsent(t, result, "fixture-actions-write-token")
	requireWrapperPathExists(t, f.outputPathLog, false)
}

func TestProductionConfigKeepsProtectedRegistryTemplate(t *testing.T) {
	config := readRepositoryFile(t, "ksail.prod.yaml")
	requireContains(t, config, `registry: "devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests"`)
	requireNotContains(t, config, "${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io")
}

func TestLifecyclePreservesUsernameFromSOPSDockerConfig(t *testing.T) {
	f := newFixture(t)
	config := validConfig()
	config["auths"].(map[string]any)["ghcr.io"].(map[string]any)["username"] = "pull-robot"

	result := f.runKSailPullWrapper(config, []string{"cluster", "update"}, nil)

	requireWrapperExitCode(t, result, 0)
	requireWrapperFileEquals(t, f.ksailUsernameCapture, "pull-robot")
}

func TestCiphertextRotationChangesRevisionWithoutHashingToken(t *testing.T) {
	f := newFixture(t)
	config := validConfig()
	first := f.runKSailPullWrapper(config, []string{"cluster", "update"}, nil)
	requireWrapperExitCode(t, first, 0)
	firstRevision := mustRead(f.ksailRevisionCapture)

	f.writeEncryptedSecret("ENC[AES256_GCM,data:fixture-two]")
	second := f.runKSailPullWrapper(config, []string{"cluster", "update"}, nil)
	requireWrapperExitCode(t, second, 0)
	secondRevision := mustRead(f.ksailRevisionCapture)

	normalizedPlaintext, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal normalized plaintext fixture: %v", err)
	}
	plaintextDigest := sha256.Sum256(append(normalizedPlaintext, '\n'))
	plaintextHash := fmt.Sprintf("%x", plaintextDigest)
	if firstRevision == secondRevision {
		t.Errorf("ciphertext rotation left revision unchanged at %q", firstRevision)
	}
	if firstRevision == plaintextHash {
		t.Errorf("first revision unexpectedly hashes normalized plaintext: %q", plaintextHash)
	}
	if secondRevision == plaintextHash {
		t.Errorf("second revision unexpectedly hashes normalized plaintext: %q", plaintextHash)
	}
}

func TestWrapperRejectsArbitraryCommands(t *testing.T) {
	f := newFixture(t)
	result := f.runKSailPullWrapper(validConfig(), []string{"workload", "delete"}, nil)

	requireWrapperExitCode(t, result, 64)
	requireWrapperPathExists(t, f.ksailTokenCapture, false)
}

func TestPlaintextRevisionSourceFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.writeEncryptedSecret("accidentally-plaintext")

	result := f.runKSailPullWrapper(validConfig(), []string{"cluster", "update"}, nil)

	if result.exitCode == 0 {
		t.Fatalf("plaintext revision source unexpectedly succeeded")
	}
	requireWrapperPathExists(t, f.ksailTokenCapture, false)
}

func TestAcceptsStandardAuthOnlyDockerConfig(t *testing.T) {
	f := newFixture(t)
	auth := base64.StdEncoding.EncodeToString([]byte("devantler:fixture-secret-token"))
	config := map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{"auth": auth},
		},
	}

	result := f.runHelper(config, nil, nil)

	requireWrapperExitCode(t, result, 0)
	var patch map[string]any
	if err := json.Unmarshal([]byte(mustRead(f.patchCapture)), &patch); err != nil {
		t.Fatalf("decode root credential patch: %v", err)
	}
	encoded, ok := patch["data"].(map[string]any)[".dockerconfigjson"].(string)
	if !ok {
		t.Fatalf("root credential patch does not contain encoded .dockerconfigjson: %#v", patch)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode patched Docker config: %v", err)
	}
	var patchedConfig any
	if err := json.Unmarshal(decoded, &patchedConfig); err != nil {
		t.Fatalf("decode patched Docker config JSON: %v", err)
	}
	if !reflect.DeepEqual(patchedConfig, config) {
		t.Errorf("patched Docker config = %#v, want %#v", patchedConfig, config)
	}
}

func TestAcceptsMatchingExplicitAndEncodedAuth(t *testing.T) {
	f := newFixture(t)
	config := validConfig()
	config["auths"].(map[string]any)["ghcr.io"].(map[string]any)["auth"] = base64.StdEncoding.EncodeToString([]byte("devantler:fixture-secret-token"))

	result := f.runHelper(config, nil, nil)

	requireWrapperExitCode(t, result, 0)
}

func TestRejectsUnsafeDrainTimeoutBeforeClusterAccess(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FLUX_GHCR_DRAIN_TIMEOUT": "45m --disable-eviction",
	})

	requireWrapperExitCode(t, result, 64)
	requireWrapperPathExists(t, f.kubectlCalled, false)
	requireWrapperPathExists(t, f.patchCapture, false)
	requireWrapperSecretAbsent(t, result, "fixture-secret-token")
}

func TestCheckOnlyPreflightsWithoutPatching(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), []string{"--check-only"}, nil)

	requireWrapperExitCode(t, result, 0)
	requireWrapperPathExists(t, f.kubectlCalled, false)
	requireWrapperPathExists(t, f.patchCapture, false)
}

func TestMissingOrMalformedRegistryAuthFailsClosed(t *testing.T) {
	conflictingAuth := base64.StdEncoding.EncodeToString([]byte("devantler:different-token"))
	tests := []struct {
		name   string
		config any
	}{
		{
			name: "missing password",
			config: map[string]any{
				"auths": map[string]any{"ghcr.io": map[string]any{"username": "devantler"}},
			},
		},
		{
			name: "empty username",
			config: map[string]any{
				"auths": map[string]any{"ghcr.io": map[string]any{"username": "", "password": "token"}},
			},
		},
		{
			name: "malformed encoded auth",
			config: map[string]any{
				"auths": map[string]any{"ghcr.io": map[string]any{"auth": "not-base64"}},
			},
		},
		{
			name: "contradictory explicit and encoded auth",
			config: map[string]any{
				"auths": map[string]any{
					"ghcr.io": map[string]any{
						"username": "devantler",
						"password": "fixture-secret-token",
						"auth":     conflictingAuth,
					},
				},
			},
		},
		{name: "missing registry", config: map[string]any{"auths": map[string]any{}}},
		{name: "not a Docker config", config: "not-a-docker-config"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(test.config, nil, nil)
			if result.exitCode == 0 {
				t.Errorf("invalid Docker config unexpectedly succeeded")
			}
			requireWrapperPathExists(t, f.kubectlCalled, false)
		})
	}
}

func TestRegistryDenialPreventsClusterPatch(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CURL_DENY_REPOSITORY": "devantler-tech/platform/manifests",
	})

	if result.exitCode == 0 {
		t.Fatalf("registry denial unexpectedly succeeded")
	}
	requireWrapperPathExists(t, f.kubectlCalled, false)
}

func TestTokenSuccessWithoutRegistryReadAccessPreventsClusterPatch(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CURL_DENY_REPOSITORY": "devantler-tech/wedding-app",
	})

	if result.exitCode == 0 {
		t.Fatalf("missing package read access unexpectedly succeeded")
	}
	requireWrapperPathExists(t, f.kubectlCalled, false)
}

func TestClusterPatchFailureIsNotHidden(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_KUBECTL_FAIL": "true"})

	requireWrapperExitCode(t, result, 43)
	requireWrapperPathExists(t, f.kubectlCalled, true)
}

func TestFreshClusterWithoutVariablesBaseSkipsExistingFanout(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(
		validConfig(),
		[]string{"--allow-incomplete-fanout"},
		map[string]string{"FAKE_VARIABLES_BASE_ABSENT": "true"},
	)

	requireWrapperExitCode(t, result, 0)
	requireWrapperPathExists(t, f.patchCapture, true)
	requireWrapperPathExists(t, f.variablesPatchCapture, false)
	requireWrapperPathExists(t, f.fanoutLog, false)
}

func TestMissingVariablesBaseFailsClosedWithoutBootstrapMode(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_VARIABLES_BASE_ABSENT": "true"})

	if result.exitCode == 0 {
		t.Fatalf("missing variables-base unexpectedly succeeded without bootstrap mode")
	}
	requireWrapperPathExists(t, f.patchCapture, false)
}

func TestPartialBootstrapRepairsRootWithoutForcingMissingFanout(t *testing.T) {
	missingResources := []string{
		"pushsecret/flux-system/seed-ghcr",
		"externalsecret/wedding-app/ghcr-auth",
		"externalsecret/ascoachingogvaner/ghcr-auth",
		"externalsecret/kyverno/ghcr-auth",
	}
	for _, resource := range missingResources {
		resource := resource
		t.Run(resource, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(
				validConfig(),
				[]string{"--allow-incomplete-fanout"},
				map[string]string{"FAKE_MISSING_FANOUT_RESOURCE": resource},
			)

			requireWrapperExitCode(t, result, 0)
			requireWrapperPathExists(t, f.variablesPatchCapture, true)
			requireWrapperPathExists(t, f.patchCapture, true)
			requireWrapperPathExists(t, f.fanoutLog, false)
			requireContains(t, result.stdout, "first reconcile will complete")
			operations := readLines(f.operationLog)
			if len(operations) < 3 {
				t.Fatalf("operation log has %d lines, want at least 3: %q", len(operations), operations)
			}
			gotTail := operations[len(operations)-3:]
			wantTail := []string{"root-patch", "variables-patch", "root-patch"}
			if !slicesEqual(gotTail, wantTail) {
				t.Errorf("final operations = %q, want %q", gotTail, wantTail)
			}
		})
	}
}

func TestPartialBootstrapRepairsRootBeforeStagingVariables(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(
		validConfig(),
		[]string{"--allow-incomplete-fanout"},
		map[string]string{
			"FAKE_MISSING_FANOUT_RESOURCE": "pushsecret/flux-system/seed-ghcr",
			"FAKE_KUBECTL_FAIL":            "true",
		},
	)

	requireWrapperExitCode(t, result, 43)
	requireWrapperPathExists(t, f.variablesPatchCapture, false)
}

func TestPartialFanoutFailsClosedWithoutBootstrapMode(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_MISSING_FANOUT_RESOURCE": "externalsecret/kyverno/ghcr-auth",
	})

	if result.exitCode == 0 {
		t.Fatalf("partial fanout unexpectedly succeeded without bootstrap mode")
	}
	requireWrapperPathExists(t, f.variablesPatchCapture, false)
	requireWrapperPathExists(t, f.patchCapture, false)
	requireWrapperPathExists(t, f.fanoutLog, false)
}

func TestMissingESOCRDsFailsWithoutStagingVariables(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_FANOUT_CRDS_ABSENT": "true"})

	if result.exitCode == 0 {
		t.Fatalf("missing External Secrets CRDs unexpectedly succeeded")
	}
	requireWrapperPathExists(t, f.variablesPatchCapture, false)
	requireWrapperPathExists(t, f.patchCapture, false)
}

func TestPartialBootstrapWithoutESOCRDsRepairsRoot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(
		validConfig(),
		[]string{"--allow-incomplete-fanout"},
		map[string]string{"FAKE_FANOUT_CRDS_ABSENT": "true"},
	)

	requireWrapperExitCode(t, result, 0)
	requireWrapperPathExists(t, f.variablesPatchCapture, true)
	requireWrapperPathExists(t, f.patchCapture, true)
	requireWrapperPathExists(t, f.fanoutLog, false)
}

func TestPushSecretSyncFailureIsNotHidden(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_SYNC_STALL_RESOURCE": "pushsecret/flux-system/seed-ghcr",
	})

	if result.exitCode == 0 {
		t.Fatalf("stalled PushSecret sync unexpectedly succeeded")
	}
	requireWrapperPathExists(t, f.patchCapture, false)
	requireWrapperPathExists(t, f.variablesPatchCapture, true)
	requireWrapperSecretAbsent(t, result, "fixture-secret-token")
}

func TestSameSecondSyncAcceptsControllerResourceVersionEdge(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_SYNC_SAME_REFRESH_TIME": "true"})

	requireWrapperExitCode(t, result, 0)
	requireWrapperPathExists(t, f.patchCapture, true)
}

func TestMaterialisedConsumerMismatchIsNotHidden(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CONSUMER_MISMATCH_NAMESPACE": "wedding-app",
	})

	if result.exitCode == 0 {
		t.Fatalf("stale materialised consumer unexpectedly succeeded")
	}
	requireContains(t, result.stdout+result.stderr, "wedding-app/ghcr-auth did not materialise")
	requireWrapperPathExists(t, f.patchCapture, false)
	requireWrapperSecretAbsent(t, result, "fixture-secret-token")
}
