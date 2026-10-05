#!/usr/bin/env bash
# Run the real pod script with the pinned images' shell/toolbox, classifying the real client
# refusals against a local S3 stub. No cluster, credentials or network
# access is available to the test containers; only the image build needs pulls.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
work_dir="$(mktemp -d)"
readonly work_dir
image="wedding-denial-runtime-test:$$"
readonly image
# shellcheck disable=SC2317,SC2329 # Invoked by EXIT.
cleanup() {
  docker image rm "${image}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}"
}
trap cleanup EXIT

read_image() {
  sed -n "s/^readonly $1='\([^']*\)'$/\1/p" "${root_dir}/scripts/verify-wedding-backup-denial.sh"
}
mc_image="$(read_image mc_image)"
tools_image="$(read_image tools_image)"
for ref in "${mc_image}" "${tools_image}"; do
  [[ "${ref}" =~ ^[A-Za-z0-9./:_-]+@sha256:[a-f0-9]{64}$ ]] || {
    printf 'FAIL: runtime image must have exactly one pinned digest\n' >&2
    exit 1
  }
done

printf 'FROM %s AS toolbox\nFROM %s\nUSER 0\nCOPY --from=toolbox /bin/busybox /tools/busybox\nRUN ["/tools/busybox", "--install", "-s", "/tools"]\nENV PATH=/tools:/usr/local/bin:/usr/bin:/bin\n' \
  "${tools_image}" "${mc_image}" >"${work_dir}/Dockerfile"
docker build --network none --tag "${image}" "${work_dir}"
docker run --rm --network none --read-only --user 65532:65532 \
  --cap-drop ALL --security-opt no-new-privileges --entrypoint /tools/sh \
  "${image}" -ec 'mc --version; for tool in sed grep awk sort tail sha256sum cut date cat sleep mkdir rm; do command -v "$tool" >/dev/null; done'

# Run the denial proof and the pinned mc against a refusing stub. Require that case to have run: it is skipped outside this image.
denial_out="$(DENIAL_POD_RUNTIME_IMAGE="${image}" bash "${root_dir}/scripts/tests/test-verify-wedding-backup-denial.sh" 2>&1)" || {
  printf '%s\n' "${denial_out}" >&2
  exit 1
}
printf '%s\n' "${denial_out}"
grep -qxF "PASS: the pinned mc's refusals classify as denials" <<<"${denial_out}" || {
  printf 'FAIL: the pinned mc was never run against the refusing stub\n' >&2
  exit 1
}
