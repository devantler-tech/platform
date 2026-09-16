#!/usr/bin/env bash
# Exercises scripts/validate-homepage-bookmarks.sh against the real ConfigMap and
# against copies of it with one invariant broken each, so every check is proven
# to fire for the reason it names rather than merely to exit non-zero.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
validator="${repo_root}/scripts/validate-homepage-bookmarks.sh"
config_map="${repo_root}/k8s/bases/apps/homepage/config-map.yaml"
k8s_root="${repo_root}/k8s"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

# mutate <name> <yq expression over the parsed bookmarks list> [settings expression]
mutate() {
  local out="${work}/$1.yaml"
  cp "${config_map}" "${out}"
  yq -i ".data.\"bookmarks.yaml\" |= (from_yaml | $2 | to_yaml)" "${out}"
  if [ -n "${3:-}" ]; then
    yq -i ".data.\"settings.yaml\" |= (from_yaml | $3 | to_yaml)" "${out}"
  fi
  printf '%s\n' "${out}"
}

# expect_pass <name> <config-map>
expect_pass() {
  local output
  if ! output="$(bash "${validator}" "$2" "${k8s_root}" 2>&1)"; then
    fail "$1: expected success, got:"$'\n'"${output}"
    return
  fi
  echo "ok: $1"
}

# expect_fail <name> <config-map> <message fragment>
expect_fail() {
  local output rc=0
  output="$(bash "${validator}" "$2" "${k8s_root}" 2>&1)" || rc=$?
  if [ "${rc}" -ne 1 ]; then
    fail "$1: expected exit 1, got ${rc}:"$'\n'"${output}"
    return
  fi
  if ! printf '%s\n' "${output}" | grep -qF -- "$3"; then
    fail "$1: exit 1 but missing \"$3\":"$'\n'"${output}"
    return
  fi
  echo "ok: $1"
}

first='.[0] | keys | .[0]'
g="$(yq -r '.data."bookmarks.yaml"' "${config_map}" | yq -r "${first}")"
b="$(yq -r '.data."bookmarks.yaml"' "${config_map}" | yq -r '.[0][] | .[0] | keys | .[0]')"
if [ -z "${g}" ] || [ "${g}" = "null" ] || [ -z "${b}" ] || [ "${b}" = "null" ]; then
  echo "::error::could not read the first bookmark group and entry from ${config_map}"
  exit 1
fi
sel=".[0].\"${g}\"[0].\"${b}\""

expect_pass "real config map" "${config_map}"
expect_pass "round-tripped config map" "$(mutate identity '.')"
expect_pass "mdi icon" "$(mutate mdi "${sel}[0].icon = \"mdi-console\"")"
expect_pass "lowercase hex color" "$(mutate hex "${sel}[0].icon = \"si-github-#aabbcc\"")"
expect_pass "icon and href split across items" \
  "$(mutate split "${sel} = [{\"icon\": \"docker\"}, {\"href\": \"https://example.com\"}]")"

expect_fail "missing icon" "$(mutate no-icon "del(${sel}[0].icon)")" "${g} -> ${b}: missing icon"
expect_fail "icon with a space" "$(mutate bad-icon "${sel}[0].icon = \"not a slug\"")" "does not match <slug>[-#RRGGBB]"
expect_fail "icon with a short color" "$(mutate short-hex "${sel}[0].icon = \"si-github-#fff\"")" "does not match <slug>[-#RRGGBB]"
expect_fail "missing href" "$(mutate no-href "del(${sel}[0].href)")" "${g} -> ${b}: missing href"
expect_fail "http href" "$(mutate http "${sel}[0].href = \"http://example.com\"")" "must be an https:// URL"
expect_fail "href without a scheme" "$(mutate no-scheme "${sel}[0].href = \"example.com\"")" "must be an https:// URL"
expect_fail "duplicate name in a group" \
  "$(mutate dup ".[0].\"${g}\" += [.[0].\"${g}\"[0]]")" "${g} -> ${b}: duplicate bookmark name in this group"
# shellcheck disable=SC2016 # $e is a yq variable, not a shell one
expect_pass "same name in different groups" \
  "$(mutate cross-dup '(.[0] | to_entries[0].value[0]) as $e | .[1] |= with_entries(.value += [$e])')"
expect_fail "group reuses a service group name" \
  "$(mutate collide ".[0] |= with_entries(.key = \"Security\")" '.layout.Security = (.layout.Security // {})')" \
  "Security: bookmark group reuses a service group name"
expect_fail "group missing from the layout" \
  "$(mutate unlisted ".[0] |= with_entries(.key = \"Unlisted Group\")")" \
  "Unlisted Group: bookmark group is not listed in settings.yaml layout"
expect_fail "group item with two group names" \
  "$(mutate two-groups ".[0].Extra = .[0].\"${g}\"")" "bookmark group item 1 must map exactly one group name"
expect_fail "bookmark item with two bookmark names" \
  "$(mutate two-names ".[0].\"${g}\"[0].Extra = [{\"href\": \"http://bad\"}]")" \
  "${g}: every bookmark item must map exactly one bookmark name"
expect_fail "bookmark fields that are not mappings" \
  "$(mutate scalar-fields "${sel} = [\"docker\"]")" "${g} -> ${b}: missing icon"
expect_fail "empty bookmarks" "$(mutate empty '[]')" "no bookmark groups parsed"
expect_fail "group without bookmarks" "$(mutate hollow ".[0].\"${g}\" = []")" "${g}: group has no bookmarks"
# With services.yaml emptied the k8s annotations still name service groups, so the
# proof-of-life only fires when both sources are empty.
cp "${config_map}" "${work}/no-services.yaml"
yq -i '.data."services.yaml" = "[]"' "${work}/no-services.yaml"
empty_root="${work}/empty-k8s"
mkdir -p "${empty_root}"
rc=0
output="$(bash "${validator}" "${work}/no-services.yaml" "${empty_root}" 2>&1)" || rc=$?
if [ "${rc}" -eq 1 ] && printf '%s\n' "${output}" | grep -qF "no service groups found"; then
  echo "ok: no service groups anywhere"
else
  fail "no service groups anywhere: rc=${rc}"$'\n'"${output}"
fi

if [ "${failures}" -ne 0 ]; then
  echo "::error::${failures} homepage bookmark validator test(s) failed"
  exit 1
fi
echo "All homepage bookmark validator tests passed."
