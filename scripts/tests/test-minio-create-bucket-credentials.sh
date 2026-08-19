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
# what makes the wait assertion below load-bearing rather than cosmetic — a
# revert to `alias set` would still create the bucket in a warm cluster and fail
# only on a cold start, which is exactly when this Job runs.
#
# THE ASSERTIONS ARE BOUND TO EACH OTHER ON PURPOSE. Checking the parts
# separately leaves gaps that each individual check reports as fine:
#   - reading only `.env` misses `envFrom[].secretRef`, which injects every key
#     of a Secret into the environment — the same exposure, one field over;
#   - "the script mentions the mount path" and "the script calls alias import"
#     can both hold while it imports something else entirely;
#   - a Secret key, a volume item path and the imported filename can each exist
#     and still name three different files.
# So the imported operand is extracted and required to equal the mount path, and
# the volume item and Secret key are required to match its basename.
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

# BOTH environment surfaces. `envFrom` is the one an `.env`-only check misses.
env_block="$(yq eval "${container_path}.env, ${container_path}.envFrom" "${manifest}")"
script_body="$(yq eval "${container_path}.command[2]" "${manifest}")"
volumes="$(yq eval "${pod_path}.volumes" "${manifest}")"
annotations="$(yq eval '.metadata.annotations' "${manifest}")"

[ -n "${script_body}" ] && [ "${script_body}" != "null" ] ||
  fail "could not extract the container script from ${manifest}"

# Assertions below are about CODE, not prose. The manifest's inline comments
# legitimately name `mc alias set` to explain what this Job stopped doing, and a
# bare match would read that explanation as the defect it describes.
script_code="$(grep -vE '^[[:space:]]*#' <<<"${script_body}")"

# --- CKV_K8S_35: the credential must not arrive through the environment ------
# secretKeyRef is the `.env` shape; secretRef is the `envFrom` shape, which
# injects EVERY key of the Secret and which checkov's CKV_K8S_35 does not flag.
for ref in secretKeyRef secretRef; do
  if grep -q "${ref}" <<<"${env_block}"; then
    fail "CKV_K8S_35: container environment carries a ${ref}; the credential must arrive as a mounted file"
  fi
done
pass "no secretKeyRef or secretRef on either env surface"

# mc also honours an undocumented MC_HOST_<alias> variable carrying the whole
# credential inline. checkov looks for a secretKeyRef, so it would NOT flag
# that -- the same exposure in a shape the scanner cannot see, which makes it
# the more likely way for this fix to be quietly undone.
if grep -qE 'MC_HOST' <<<"${env_block}"; then
  fail "CKV_K8S_35: an MC_HOST_* variable carries the credential in the environment; import it from the mounted file instead"
fi
pass "no MC_HOST_* variable in the container environment"

# --- the credential must actually reach the pod ------------------------------
# The checks above are also satisfied by deleting the credential outright, which
# would break the Job rather than harden it.
grep -q 'minio-root-credentials' <<<"${volumes}" ||
  fail "no volume sourced from the minio-root-credentials Secret"
pass "minio-root-credentials is mounted as a volume"

creds_mount_path="$(yq eval \
  "${container_path}.volumeMounts[] | select(.name == \"minio-credentials\") | .mountPath" "${manifest}")"
[ -n "${creds_mount_path}" ] && [ "${creds_mount_path}" != "null" ] ||
  fail "the minio-credentials volume is not mounted into the container"
pass "credentials volume is mounted at ${creds_mount_path}"

# --- the credential must not reappear in argv --------------------------------
# `mc alias set <alias> <url> <user> <password>` puts both halves on the command
# line, where /proc/<pid>/cmdline exposes them to anything in the pod's PID
# namespace. `alias import` takes a file path instead.
if grep -qE 'mc[[:space:]]+alias[[:space:]]+set' <<<"${script_code}"; then
  fail "CKV_K8S_35: 'mc alias set' places the credential in argv; use 'mc alias import <file>'"
fi
pass "no 'mc alias set' — the credential never enters argv"

# --- the imported file must BE the mounted credential ------------------------
# Extract the operand rather than merely confirming both strings appear
# somewhere: "mentions the mount path" and "calls alias import" can both hold
# while the import reads an entirely different file.
import_line="$(grep -E 'mc[[:space:]]+alias[[:space:]]+import' <<<"${script_code}" || true)"
[ -n "${import_line}" ] ||
  fail "the script does not use 'mc alias import'; the file-based route is what removes the exposure"
[ "$(grep -cE 'mc[[:space:]]+alias[[:space:]]+import' <<<"${script_code}")" -eq 1 ] ||
  fail "more than one 'mc alias import' call; this test can only bind a single import to the mount"

imported_path="$(awk '{ for (i = 1; i < NF; i++) if ($i == "import") { print $(i + 2); exit } }' <<<"${import_line}")"
[ -n "${imported_path}" ] ||
  fail "could not extract the file operand of 'mc alias import' from: ${import_line}"

case "${imported_path}" in
  "${creds_mount_path}"/*) ;;
  *) fail "'mc alias import' reads ${imported_path}, which is not under the mounted credential path ${creds_mount_path}" ;;
esac
pass "the imported file (${imported_path}) is under the mounted credential path"

imported_file="${imported_path##*/}"

# --- the volume item and the Secret key must name that same file -------------
item_path="$(yq eval \
  "${pod_path}.volumes[] | select(.name == \"minio-credentials\") | .secret.items[] | .path" "${manifest}")"
[ "${item_path}" = "${imported_file}" ] ||
  fail "the Secret volume projects '${item_path}' but the script imports '${imported_file}'; nothing would be at that path"
pass "the volume projects exactly ${imported_file}"

yq eval ".stringData | has(\"${imported_file}\")" "${secret_manifest}" | grep -qx 'true' ||
  fail "the minio-root-credentials Secret does not define a '${imported_file}' key for the Job to import"
pass "the Secret supplies ${imported_file}"

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

printf '\nAll minio-create-bucket credential assertions passed.\n'
