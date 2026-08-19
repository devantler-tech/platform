#!/usr/bin/env bash

# Contract for the minio-create-bucket Job's credential handling (#3018).
#
# checkov CKV_K8S_35 flagged this Job for taking the MinIO root credential as
# two `secretKeyRef` environment variables. The remediation is only real if the
# credential reaches `mc` from a mounted file AND does not reappear in argv — a
# fix that moves it from /proc/self/environ to /proc/<pid>/cmdline has relocated
# the exposure, not removed it. Both are asserted here.
#
# The route is `mc alias import ALIAS <file>`, which reads a JSON credential
# document. It was deferred once because it restructures the Job's control flow:
# `alias import` never contacts the server, so the `until mc alias set ...` loop
# that waited for MinIO to come up cannot stay where it was. That restructure is
# what makes the wait assertions below load-bearing rather than cosmetic — a
# revert to `alias set` would still create the bucket in a warm cluster and fail
# only on a cold start, which is exactly when this Job runs.
#
# Assertions are made against the manifest rather than a transcription of it, so
# this cannot drift into testing a stale copy.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/providers/docker/infrastructure/controllers/minio/job.yaml"
readonly secret_manifest="${root_dir}/k8s/providers/docker/infrastructure/controllers/minio/secret.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'ok — %s\n' "$1"
}

command -v yq >/dev/null 2>&1 || {
  printf '::error::yq is required to run this contract test.\n' >&2
  exit 64
}
[ -f "${manifest}" ] || fail "manifest not found: ${manifest} (run from the repository root)"
[ -f "${secret_manifest}" ] || fail "secret manifest not found: ${secret_manifest}"

container_path='.spec.template.spec.containers[0]'
pod_path='.spec.template.spec'

env_block="$(yq eval "${container_path}.env" "${manifest}")"
script_body="$(yq eval "${container_path}.command[2]" "${manifest}")"
volumes="$(yq eval "${pod_path}.volumes" "${manifest}")"
annotations="$(yq eval '.metadata.annotations' "${manifest}")"

[ -n "${script_body}" ] && [ "${script_body}" != "null" ] ||
  fail "could not extract the container script from ${manifest}"

# Assertions below are about CODE, not prose. The manifest's inline
# comments legitimately name `mc alias set` to explain what this Job
# stopped doing, and a bare match would read that explanation as the
# defect it describes.
script_code="$(grep -vE '^[[:space:]]*#' <<<"${script_body}")"

# --- CKV_K8S_35: the credential must not arrive as an environment variable ----
if grep -q 'secretKeyRef' <<<"${env_block}"; then
  fail "CKV_K8S_35: container env still carries a secretKeyRef; the credential must arrive as a mounted file"
fi
pass "no secretKeyRef in the container environment"

# The check above is also satisfied by simply deleting the credential, which
# would break the Job instead of hardening it. Assert the Secret still reaches
# the pod as a volume.
grep -q 'minio-root-credentials' <<<"${volumes}" ||
  fail "no volume sourced from the minio-root-credentials Secret"
pass "minio-root-credentials is mounted as a volume"

creds_mount_path="$(yq eval \
  "${container_path}.volumeMounts[] | select(.name == \"minio-credentials\") | .mountPath" "${manifest}")"
[ -n "${creds_mount_path}" ] && [ "${creds_mount_path}" != "null" ] ||
  fail "the minio-credentials volume is not mounted into the container"
pass "credentials volume is mounted at ${creds_mount_path}"

# ...and that the script actually reads it, or the mount is decorative.
grep -q -- "${creds_mount_path}" <<<"${script_code}" ||
  fail "the script never references ${creds_mount_path}; the mounted credential is unused"
pass "the script reads the credential from its mount path"

# --- the credential must not reappear in argv --------------------------------
# `mc alias set <alias> <url> <user> <password>` puts both halves on the command
# line, where /proc/<pid>/cmdline exposes them to anything in the pod's PID
# namespace. `alias import` takes a file path instead.
if grep -qE 'mc[[:space:]]+alias[[:space:]]+set' <<<"${script_code}"; then
  fail "CKV_K8S_35: 'mc alias set' places the credential in argv; use 'mc alias import <file>'"
fi
pass "no 'mc alias set' — the credential never enters argv"

grep -qE 'mc[[:space:]]+alias[[:space:]]+import' <<<"${script_code}" ||
  fail "the script does not use 'mc alias import'; the file-based route is what removes the exposure"
pass "the script imports its alias from the mounted file"

# --- the wait loop must have moved off 'alias set' ---------------------------
# Measured against the pinned mc release: `alias import` returns success with no
# server reachable at all, so it cannot serve as the readiness gate. Whatever
# waits for MinIO has to be a command that contacts it.
if ! grep -qE 'until[[:space:]]+mc[[:space:]]+mb' <<<"${script_code}"; then
  fail "no 'until mc mb' wait loop; 'alias import' never contacts the server, so nothing waits for MinIO to come up"
fi
pass "the readiness wait is on bucket creation, which does contact the server"

# --- the disposition is now a fix, not an exception --------------------------
if grep -q 'CKV_K8S_35' <<<"${annotations}"; then
  fail "a CKV_K8S_35 checkov.io/skip annotation is still present; the finding is fixed, so the exception must go"
fi
pass "no CKV_K8S_35 skip annotation remains"

# --- the Secret must actually supply the document the Job imports ------------
creds_key="$(basename "$(yq eval "${container_path}.command[2]" "${manifest}" |
  grep -oE '/[A-Za-z0-9._/-]*credentials[A-Za-z0-9._/-]*\.json' | head -1)")"
[ -n "${creds_key}" ] || fail "could not determine the credential filename the script imports"
yq eval ".stringData | has(\"${creds_key}\")" "${secret_manifest}" | grep -qx 'true' ||
  fail "the minio-root-credentials Secret does not define a '${creds_key}' key for the Job to import"
pass "the Secret supplies ${creds_key}"

printf '\nAll minio-create-bucket credential assertions passed.\n'
