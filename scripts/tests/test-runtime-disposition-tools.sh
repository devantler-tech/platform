#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
helper="$root/scripts/with-runtime-disposition-tools.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin"
original_path="$PATH"
printf 'retained\n' >"$work/github-path"
export GITHUB_PATH="$work/github-path"
fail() {
  echo "FAIL: $*" >&2
  exit 1
}
expect_refusal() {
  local expected="$1" status=0
  shift
  bash "$helper" "$@" >"$work/stdout" 2>"$work/stderr" || status=$?
  [ "$status" -eq 2 ] || fail "expected exit2, got $status"
  grep -Fq "$expected" "$work/stderr" || fail "wrong refusal: $(cat "$work/stderr")"
  [ ! -e "$work/executed" ] || fail 'an unverified tool or child was executed'
  [ "$PATH" = "$original_path" ] || fail 'parent PATH changed'
  [ "$(cat "$work/github-path")" = retained ] || fail 'global Actions PATH changed'
}
expect_refusal 'command required'
expect_refusal 'unsupported option' --unknown -- true
cat >"$work/bin/uname" <<'SH'
#!/usr/bin/env bash
case "$1" in -s) echo Darwin ;; -m) echo x86_64 ;; *) exit 1 ;; esac
SH
chmod +x "$work/bin/uname"
PATH="$work/bin:$original_path" bash "$helper" -- true >"$work/stdout" 2>"$work/stderr" && fail 'unsupported platform accepted'
grep -Fq 'unsupported platform' "$work/stderr" || fail 'unsupported platform did not refuse'
cat >"$work/bin/uname" <<'SH'
#!/usr/bin/env bash
case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 1 ;; esac
SH
cat >"$work/bin/kubectl" <<'SH'
#!/usr/bin/env bash
touch "$TOOLS_TEST_EXECUTED"
echo '{"clientVersion":{"gitVersion":"v1.36.2"}}'
SH
cat >"$work/bin/kyverno" <<'SH'
#!/usr/bin/env bash
touch "$TOOLS_TEST_EXECUTED"
echo 'Version: 1.19.2'
SH
cat >"$work/bin/curl" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" >>"$TOOLS_TEST_REQUESTS"
exit 22
SH
chmod +x "$work/bin/"*
export TOOLS_TEST_EXECUTED="$work/executed" TOOLS_TEST_REQUESTS="$work/requests"
status=0
PATH="$work/bin:$original_path" bash "$helper" --kubectl-only -- touch "$work/executed" >"$work/stdout" 2>"$work/stderr" || status=$?
[ "$status" -eq 2 ] || fail 'failed download did not refuse'
[ ! -e "$work/executed" ] || fail 'version-spoofing installed tool or child executed'
grep -Fq 'https://dl.k8s.io/release/v1.36.2/bin/linux/amd64/kubectl' "$work/requests" || fail 'bootstrap URL/pin changed'
cat >"$work/bin/curl" <<'SH'
#!/usr/bin/env bash
output=''
while [ "$#" -gt 0 ]; do
  case "$1" in --output) shift; output="$1" ;; esac
  shift
done
[ -n "$output" ] || exit 2
printf 'unverified-download\n' >"$output"
SH
status=0
PATH="$work/bin:$original_path" bash "$helper" --kubectl-only -- touch "$work/executed" >"$work/stdout" 2>"$work/stderr" || status=$?
[ "$status" -eq 2 ] || fail 'tampered binary did not refuse'
grep -Fq 'checksum mismatch' "$work/stderr" || fail 'tampered binary refused for wrong reason'
[ ! -e "$work/executed" ] || fail 'tampered binary or child executed'
if [ "${RUNTIME_TOOLS_NATIVE_TESTS:-0}" = 1 ]; then
  # The mandatory native boundary runs this test inside the verified helper.
  # Reuse its genuine renderer to reach (and corrupt) the Kyverno archive path.
  native_kubectl="$(PATH="$original_path" command -v kubectl)"
  cp "$native_kubectl" "$work/bin/kubectl"
  rm "$work/bin/uname"
  status=0
  PATH="$work/bin:$original_path" bash "$helper" -- touch "$work/executed" >"$work/stdout" 2>"$work/stderr" || status=$?
  [ "$status" -eq 2 ] || fail 'tampered Kyverno archive did not refuse'
  grep -Fq 'checksum mismatch for kyverno.tar.gz' "$work/stderr" || fail 'Kyverno corruption refused for wrong reason'
  [ ! -e "$work/executed" ] || fail 'tampered Kyverno or child executed'
  # Child status propagates and its private verified tool path is cleaned.
  status=0
  PATH="$work/bin:$original_path" bash "$helper" --kubectl-only -- bash -c 'command -v kubectl >"$1"; exit 7' _ "$work/child-path" || status=$?
  [ "$status" -eq 7 ] || fail 'child status was lost'
  child_tool="$(cat "$work/child-path")"
  [ "$child_tool" != "$native_kubectl" ] && [ ! -e "$child_tool" ] || fail 'private tool cleanup failed'
fi
[ "$PATH" = "$original_path" ] || fail 'parent PATH changed'
[ "$(cat "$work/github-path")" = retained ] || fail 'global Actions PATH changed'
echo 'PASS: invalid invocation/platform, spoofed versions and failed/tampered downloads refuse; native mode also proves Kyverno archive refusal, child status and cleanup; no global PATH mutation.'
