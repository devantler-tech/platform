#!/usr/bin/env bash
# Exercise the real filename-to-schema boundary: this valid CNPG fixture must
# validate, and the same path with an invalid instances type must be rejected.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
v8r_bin="${V8R_BIN:-v8r}"
fixture_path=scripts/tests/fixtures/wedding-database-priority/cluster.yaml
schema_path=scripts/tests/schemas/cloudnative-pg-cluster-v1.json
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/v8r-cnpg-schema.XXXXXX")"
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

command -v "$v8r_bin" >/dev/null || fail "v8r 6.1.0 is required"
test "$("$v8r_bin" --version)" = 6.1.0 || fail "expected v8r 6.1.0"
command -v jq >/dev/null || fail "jq is required"

mkdir -p "$work_dir/$(dirname "$fixture_path")" "$work_dir/$(dirname "$schema_path")"
cp "$repo_root/$fixture_path" "$work_dir/$fixture_path"
if test -f "$repo_root/.v8rrc.json"; then
  cp "$repo_root/.v8rrc.json" "$work_dir/.v8rrc.json"
fi
if test -f "$repo_root/$schema_path"; then
  cp "$repo_root/$schema_path" "$work_dir/$schema_path"
fi

# v8r resolves paths from its working directory; use the committed exact fixture
# path in an isolated directory so negative controls never edit repository input.
unset V8R_CONFIG_FILE
cd "$work_dir"
if ! "$v8r_bin" --output-format json "$fixture_path" >valid.json 2>valid.log; then
  cat valid.log valid.json >&2
  fail "the valid fixture must pass its actual CloudNativePG schema"
fi
jq -e --arg fixture "$fixture_path" --arg schema "$schema_path" '
  .results | length == 1 and
  .[0].fileLocation == $fixture and .[0].schemaLocation == $schema and
  .[0].valid == true and .[0].code == 0
' valid.json >/dev/null || fail "v8r did not select the committed CNPG schema"

sed 's/instances: 3/instances: invalid/' "$repo_root/$fixture_path" >"$fixture_path"
cmp -s "$repo_root/$fixture_path" "$fixture_path" && fail "instances mutation changed nothing"
if "$v8r_bin" --output-format json "$fixture_path" >invalid.json 2>invalid.log; then
  fail "v8r accepted an invalid instances type"
else
  invalid_status=$?
fi
test "$invalid_status" = 99 || fail "invalid fixture did not fail schema validation (exit $invalid_status)"
jq -e --arg fixture "$fixture_path" --arg schema "$schema_path" '
  .results | length == 1 and
  .[0].fileLocation == $fixture and .[0].schemaLocation == $schema and
  .[0].valid == false and .[0].code == 99 and
  any(.[0].errors[]; .instancePath == "/spec/instances" and .keyword == "type")
' invalid.json >/dev/null || fail "v8r did not reject instances through the CNPG schema"

echo 'PASS: native v8r 6.1.0 selects the pinned CNPG schema; valid fixture passes and invalid instances fails'
