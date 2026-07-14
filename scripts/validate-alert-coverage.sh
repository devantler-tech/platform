#!/usr/bin/env bash
# Fail if a HelmRelease lives in a namespace the reconciliation Alert does not watch.
#
# notification-controller has NO namespace wildcard: it defaults an omitted
# eventSources[].namespace to the Alert's own namespace and then matches on strict
# equality (internal/server/event_handlers.go):
#
#   if source.Namespace == "" { source.Namespace = alert.Namespace }
#   if event.InvolvedObject.Namespace != source.Namespace || ... { return false }
#
# `name: "*"` wildcards the NAME only. So every namespace holding a HelmRelease has
# to be listed explicitly in the Alert, and that list drifts the moment someone adds
# a HelmRelease in a new namespace.
#
# An uncovered namespace is invisible: the HelmRelease reconciles, fails, rolls back,
# reports Ready — and no alert ever fires. That silent-rollback blindness is exactly
# what the Alert exists to prevent (2026-07-14: ksail-operator sat four releases
# behind for over a day with prod looking green), so drift is a CI failure, not a
# comment.

set -euo pipefail

readonly ALERT_FILE="k8s/providers/hetzner/infrastructure/flux-notifications/alert.yaml"
readonly LAYERS=(
  "k8s/providers/hetzner/bootstrap"
  "k8s/providers/hetzner/infrastructure/controllers"
  "k8s/providers/hetzner/infrastructure"
  "k8s/providers/hetzner/apps"
)

for tool in kubectl yq; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "::error::${tool} is required to validate Alert coverage."
    exit 64
  }
done
[[ -f "${ALERT_FILE}" ]] || {
  echo "::error::${ALERT_FILE} not found (run from the repository root)."
  exit 64
}

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
declared="${work_dir}/declared.txt"
watched="${work_dir}/watched.txt"

# Every namespace that actually contains a HelmRelease, rendered from Git.
for layer in "${LAYERS[@]}"; do
  [[ -f "${layer}/kustomization.yaml" ]] || continue
  kubectl kustomize "${layer}" 2>/dev/null \
    | yq ea 'select(.kind == "HelmRelease") | .metadata.namespace' - 2>/dev/null
done | grep -vE '^null$|^---$|^$' | LC_ALL=C sort -u > "${declared}"

if [[ ! -s "${declared}" ]]; then
  echo "::error::Rendered no HelmReleases at all — the overlays failed to build, so coverage cannot be proven. Failing closed."
  exit 1
fi

# Every namespace the Alert watches for HelmRelease events.
yq e '.spec.eventSources[] | select(.kind == "HelmRelease") | .namespace' "${ALERT_FILE}" \
  | grep -vE '^null$|^$' | LC_ALL=C sort -u > "${watched}"

uncovered="$(comm -23 "${declared}" "${watched}")"
if [[ -n "${uncovered}" ]]; then
  echo "::error::The reconciliation Alert does not watch every namespace holding a HelmRelease."
  echo "A HelmRelease in an unwatched namespace can fail, roll back and report Ready — silently, with no alert."
  echo "Add a HelmRelease eventSource for each namespace below to ${ALERT_FILE}:"
  while read -r ns; do
    [[ -n "${ns}" ]] && printf '    - kind: HelmRelease\n      name: "*"\n      namespace: %s\n' "${ns}"
  done <<< "${uncovered}"
  exit 1
fi

# A listed-but-empty namespace never matches anything: harmless, but it is dead
# config, so say so without failing the build.
stale="$(comm -13 "${declared}" "${watched}" || true)"
if [[ -n "${stale}" ]]; then
  echo "::warning::The Alert watches namespaces that hold no HelmRelease (dead entries): $(echo "${stale}" | tr '\n' ' ')"
fi

echo "✅ Alert covers all $(wc -l < "${declared}" | tr -d ' ') namespaces holding a HelmRelease."
