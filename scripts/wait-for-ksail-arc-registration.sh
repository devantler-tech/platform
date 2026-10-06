#!/usr/bin/env bash
# Read-only convergence polling; the caller also imposes a 660-second hard deadline.
set -euo pipefail
[[ "$#" -eq 1 && "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 2
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || exit 1
readonly revision="latest@$1"
readonly deadline=$((SECONDS + 600))
kc() { timeout 35s kubectl --context admin@prod --request-timeout=30s "$@" 2>/dev/null; }

while ((SECONDS < deadline)); do
  ready=true
  for layer in infrastructure apps; do
    if ! state=$(kc -n flux-system get kustomization "$layer" -o json) ||
      ! jq -e --arg revision "$revision" '
        .status.lastAppliedRevision == $revision and
        .status.observedGeneration == .metadata.generation and
        any(.status.conditions[]; .type == "Ready" and .status == "True")
      ' <<<"$state" >/dev/null 2>&1; then
      ready=false
    fi
  done
  # The pinned provider caches by reference name, across namespaces and kinds.
  # Bind this distinct name to exactly one native configuration and Secret.
  if ! state=$(kc get providerconfigs.github.m.upbound.io --all-namespaces -o json) ||
    ! jq -e '
      [.items[] | select(.metadata.name == "arc-runtime-platform-app")] as $configs |
      ($configs | length) == 1 and
      $configs[0].apiVersion == "github.m.upbound.io/v1beta1" and
      $configs[0].kind == "ProviderConfig" and
      $configs[0].metadata.namespace == "arc-runners" and
      $configs[0].metadata.deletionTimestamp == null and
      $configs[0].spec.credentials == {"source":"Secret","secretRef":{
        "namespace":"arc-runners","name":"arc-github-app","key":"provider-credentials"}}
    ' <<<"$state" >/dev/null 2>&1; then
    ready=false
  fi
  for resource in clusterproviderconfigs.github.m.upbound.io providerconfigs.github.upbound.io; do
    if ! state=$(kc get "$resource" -o json) ||
      ! jq -e '[.items[] | select(.metadata.name == "arc-runtime-platform-app")] | length == 0' \
        <<<"$state" >/dev/null 2>&1; then
      ready=false
    fi
  done
  # The provider observation comes from the existing organization's App.
  # Desired selection alone cannot establish the remote access boundary.
  if ! state=$(kc -n arc-runners get runnergroups.actions.github.m.upbound.io platform -o json) ||
    ! jq -e '
      .metadata.generation as $generation |
      .status.atProvider.id as $id |
      .apiVersion == "actions.github.m.upbound.io/v1alpha1" and .kind == "RunnerGroup" and
      .metadata.name == "platform" and .metadata.namespace == "arc-runners" and
      (.metadata.uid | type == "string" and length > 0) and .metadata.deletionTimestamp == null and
      ($generation | type == "number" and . > 0) and
      ($id | type == "string" and test("^[1-9][0-9]{0,18}$")) and
      .metadata.annotations["crossplane.io/external-name"] == $id and
      .spec.providerConfigRef == {"name":"arc-runtime-platform-app","kind":"ProviderConfig"} and
      .spec.forProvider.name == "platform" and .spec.forProvider.visibility == "selected" and
      .spec.forProvider.selectedRepositoryIds == [737584922] and
      .spec.forProvider.allowsPublicRepositories == true and
      .spec.forProvider.restrictedToWorkflows == true and
      .spec.forProvider.selectedWorkflows == ["devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"] and
      .status.atProvider.name == "platform" and .status.atProvider.visibility == "selected" and
      .status.atProvider.selectedRepositoryIds == [737584922] and
      .status.atProvider.allowsPublicRepositories == true and
      .status.atProvider.restrictedToWorkflows == true and
      .status.atProvider.selectedWorkflows == ["devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"] and
      .status.atProvider.default == false and .status.atProvider.inherited == false and
      .status.atProvider.runnersUrl == ("https://api.github.com/orgs/devantler-tech/actions/runner-groups/" + $id + "/runners") and
      .status.atProvider.selectedRepositoriesUrl == ("https://api.github.com/orgs/devantler-tech/actions/runner-groups/" + $id + "/repositories") and
      any(.status.conditions[]; .type == "Ready" and .status == "True" and .observedGeneration == $generation) and
      any(.status.conditions[]; .type == "Synced" and .status == "True" and .observedGeneration == $generation)
    ' <<<"$state" >/dev/null 2>&1; then
    ready=false
  fi
  if ! state=$(kc -n arc-runners get autoscalingrunnerset.actions.github.com platform-linux -o json) ||
    ! jq -e '
      .status.phase == "Running" and .status.observedGeneration == .metadata.generation and
      ((.metadata.annotations["runner-scale-set-id"] | tonumber) > 0) and
      .spec.githubConfigUrl == "https://github.com/devantler-tech" and
      .spec.runnerGroup == "platform" and .spec.runnerScaleSetName == "platform-linux" and
      .metadata.annotations["actions.github.com/runner-group-name"] == "platform" and
      .metadata.annotations["actions.github.com/runner-scale-set-name"] == "platform-linux"
    ' <<<"$state" >/dev/null 2>&1; then
    ready=false
  fi
  if "$ready"; then
    printf 'ARC layers and registration converged at the deployed revision\n'
    exit 0
  fi
  sleep 10
done
printf 'ARC layers or registration did not converge before the deadline\n' >&2
exit 1
