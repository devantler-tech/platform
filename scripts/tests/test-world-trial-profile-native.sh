#!/usr/bin/env bash
# Fresh signed public-artifact proof. No image/program execution or cluster use.
set -euo pipefail
root="$(cd "${1:-$(dirname "${BASH_SOURCE[0]}")/../..}" && pwd)"
umask 077
if [[ -n "${WORLD_TRIAL_EVIDENCE_DIR:-}" ]]; then
  mkdir -p "${WORLD_TRIAL_EVIDENCE_DIR}"
  work="$(mktemp -d "${WORLD_TRIAL_EVIDENCE_DIR}/proof.XXXXXX")"
else
  work="$(mktemp -d)"
  trap 'rm -rf -- "${work}"' EXIT
fi
export COSIGN_BIN="${COSIGN_BIN:-$(command -v cosign || true)}"
export ORAS_BIN="${ORAS_BIN:-$(command -v oras || true)}"
export KUBECTL_BIN="${KUBECTL_BIN:-$(command -v kubectl || true)}"
cd "${root}"
# Adverse offline/source/archive controls exercise the same post-verification
# boundary with exact audited bytes. They never execute artifact contents.
GOTOOLCHAIN=go1.27.3 go test -v ./internal/worldtrialprofile >"${work}/boundary-tests.stdout" 2>"${work}/boundary-tests.stderr"
GOTOOLCHAIN=go1.27.3 go build -o "${work}/guard-world-trial-profile" ./scripts/guard-world-trial-profile
"${work}/guard-world-trial-profile" -root "${root}" -native -evidence "${work}/native"
printf 'PASS: fresh World native proof; retained evidence: %s\n' "${work}"
