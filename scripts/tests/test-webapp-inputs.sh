#!/usr/bin/env bash
# Exercise the real publication input boundary; never print candidate values.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
check="$root/scripts/validate-webapp-inputs.sh"
valid='{"apiVersion":"kro.run/v1alpha1","kind":"WebApp","metadata":{"name":"fixture"},"spec":{"name":"fixture","image":"registry.k8s.io/pause:3.10.2","host":"fixture.invalid"}}'
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
passed=0
expect() {
  local wanted="$1" input="$2" rc=0
  bash "$check" <<<"$input" >"$work/out" 2>"$work/error" || rc=$?
  if { [ "$wanted" = valid ] && [ "$rc" -ne 0 ]; } || { [ "$wanted" = invalid ] && [ "$rc" -eq 0 ]; }; then
    echo "FAIL: input-boundary control $passed expected $wanted (exit $rc)" >&2
    exit 1
  fi
  # Refusals reveal no payload, even for malformed input.
  ! grep -q 'PRIVATE-FIXTURE-VALUE' "$work/out" "$work/error" || exit 1
  passed=$((passed + 1))
}
expect_mutation() {
  local wanted="$1" expression="$2" input="$3" candidate
  if ! candidate="$(jq "$expression" <<<"$input")"; then
    echo 'FAIL: input-boundary control mutation did not evaluate' >&2
    exit 1
  fi
  expect "$wanted" "$candidate"
}
expect valid "$valid"
enabled="$(jq '.spec += {args:["--public","/app/public","--public"],runAsUser:65532,runAsGroup:65532,readOnlyRootFilesystem:true,secretProjections:[{secretName:"public-disclosure",items:[{key:"address",path:"company-address"}]}]}' <<<"$valid")"
expect valid "$enabled"
for mutation in \
  '.spec.args = null' \
  '.spec.args = [true]' \
  '.spec.args = ["\u0024(PRIVATE-FIXTURE-VALUE)"]' \
  '.spec.args = ["before\u0000after"]' \
  '.spec.args = [range(0;33)|"a"]' \
  '.spec.args = ["x" * 257]' \
  '.spec.args = ["é" * 129]' \
  '.spec.runAsUser = 0' \
  '.spec.runAsGroup = -1' \
  '.spec.runAsUser = 1.5' \
  '.spec.runAsGroup = 2147483648' \
  '.spec.readOnlyRootFilesystem = "true"' \
  '.spec.command = ["sh"]' \
  '.spec.secretProjections += .spec.secretProjections' \
  '.spec.secretProjections[0].secretName = "../private"' \
  '.spec.secretProjections[0].namespace = "other"' \
  '.spec.secretProjections[0].optional = true' \
  '.spec.secretProjections[0].items = []' \
  '.spec.secretProjections[0].items += .spec.secretProjections[0].items' \
  '.spec.secretProjections[0].items += [{key:"address",path:"different"}]' \
  '.spec.secretProjections[0].items += [{key:"different",path:"company-address"}]' \
  '.spec.secretProjections[0].items[0] |= del(.key)' \
  '.spec.secretProjections[0].items[0] |= del(.path)' \
  '.spec.secretProjections[0].items[0].path = "../private"' \
  '.spec.secretProjections[0].items[0].path = ".hidden"' \
  '.spec.secretProjections[0].items[0].mode = 511' \
  '.spec.secretProjections[0].items[0].key = "a/b"' \
  '.spec.secretProjections[0].items = [range(0;9)|{key:("k"+tostring),path:("p"+tostring)}]' \
  '.spec.secretProjections[0].items[0].path = ("x" * 65)' \
  '.spec.secretProjections[0] |= del(.items)' \
  '.spec.secretProjections[0] |= del(.secretName)' \
  '.spec.secretProjections = null'; do
  expect_mutation invalid "$mutation" "$enabled"
done
expect_mutation valid '.spec.args=["é" * 128]' "$enabled"
expect_mutation valid '.spec.args=[range(0;32)|("x" * 256)]' "$enabled"
expect invalid "$valid $enabled"
expect invalid "$valid {PRIVATE-FIXTURE-VALUE"
expect invalid '{PRIVATE-FIXTURE-VALUE'
expect_mutation invalid '.unexpected="PRIVATE-FIXTURE-VALUE"' "$valid"
expect invalid "${valid%\}} ,\"spec\":{\"name\":\"fixture\",\"image\":\"registry.k8s.io/pause:3.10.2\",\"host\":\"fixture.invalid\"}}"
expect invalid '{"apiVersion":"kro.run/v1alpha1","kind":"WebApp","metadata":{"name":"fixture"},"spec":{"name":"fixture","image":"registry.k8s.io/pause:3.10.2","host":"fixture.invalid","runAsUser":0,"runAsUser":65532}}'
# Exercise the real Go launcher from a different physical working directory.
caller="$work/caller with spaces"
mkdir "$caller"
printf '%s\n' "$valid" >"$caller/fixture.json"
printf '%s\n' "$valid" >"$caller/input with spaces.json"
printf '%s\n' "$valid" >"$caller/-"
expect_path() {
  local wanted="$1" input="$2" rc=0
  (cd -- "$caller" && bash "$check" "$input" <<<"$valid") >"$work/out" 2>"$work/error" || rc=$?
  if { [ "$wanted" = valid ] && [ "$rc" -ne 0 ]; } || { [ "$wanted" = invalid ] && [ "$rc" -eq 0 ]; }; then
    echo "FAIL: caller-path control $passed expected $wanted (exit $rc)" >&2
    exit 1
  fi
  passed=$((passed + 1))
}
expect_path valid fixture.json
expect_path valid ./fixture.json
expect_path valid 'input with spaces.json'
expect_path valid "$caller/fixture.json"
expect_path valid ./-
expect_path valid -
expect_path invalid missing.json
echo "WebApp strict source inputs: $passed actual boundary controls passed."
