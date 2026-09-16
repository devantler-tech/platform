#!/usr/bin/env bash
# Validate the Homepage dashboard's bookmarks (k8s/bases/apps/homepage/config-map.yaml).
#
# The ConfigMap embeds bookmarks.yaml as a block scalar, so manifest schema
# validation sees an opaque string. Every mistake below would render as a broken
# or confusing dashboard with a green pipeline, so this parses the embedded YAML
# and fails the PR instead:
#
#   1. every bookmark has an icon that is a slug (`docker`, `si-github`,
#      `mdi-console`), optionally suffixed with a `-#RRGGBB` color override;
#   2. every bookmark has an `https://` href;
#   3. no two bookmarks in one group share a name;
#   4. no bookmark group reuses a service group name — Homepage renders services
#      and bookmarks as separate sections, so a shared name paints the same
#      heading twice with different contents under each;
#   5. every bookmark group is listed in settings.yaml `layout` — an unlisted
#      group falls back to alphabetical order with a generic icon.
#
# Service groups are read from services.yaml AND from the `gethomepage.dev/group`
# annotations Homepage discovers across k8s/, never from the layout: the layout
# lists both kinds, so deriving one from it would make check 4 circular.
#
# Usage: validate-homepage-bookmarks.sh [config-map.yaml] [k8s-root]
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
config_map="${1:-${repo_root}/k8s/bases/apps/homepage/config-map.yaml}"
k8s_root="${2:-${repo_root}/k8s}"

for tool in yq jq; do
  command -v "${tool}" >/dev/null || {
    echo "::error::${tool} is required"
    exit 2
  }
done
[ -r "${config_map}" ] || {
  echo "::error::missing or unreadable: ${config_map}"
  exit 2
}
[ -d "${k8s_root}" ] || {
  echo "::error::missing k8s root: ${k8s_root}"
  exit 2
}

embedded() {
  local body
  body="$(yq -r ".data.\"$1\"" "${config_map}")"
  if [ -z "${body}" ] || [ "${body}" = "null" ]; then
    echo "::error::$1 missing from ${config_map}" >&2
    return 1
  fi
  printf '%s\n' "${body}"
}

bookmarks_json="$(embedded bookmarks.yaml | yq -o=json '.')"
settings="$(embedded settings.yaml)"
services="$(embedded services.yaml)"

# One problem per line. A group's value is a list of single-key maps (the
# bookmark name); a bookmark's value is a list of maps whose keys are merged, so
# `- icon: x` + `href: y` on one item and split items read the same.
problems="$(printf '%s\n' "${bookmarks_json}" | jq -r '
  def slug: test("^[A-Za-z0-9]+(-[A-Za-z0-9]+)*(-#[0-9A-Fa-f]{6})?$");
  if type != "array" or length == 0 then
    "no bookmark groups parsed — bookmarks.yaml is empty or not a list"
  else
    .[] | to_entries[0] as $group
    | if ($group.value | type) != "array" or ($group.value | length) == 0 then
        "\($group.key): group has no bookmarks"
      else
        ([$group.value[] | to_entries[0].key] | group_by(.) | map(select(length > 1) | .[0]) | .[]
          | "\($group.key) -> \(.): duplicate bookmark name in this group"),
        ($group.value[] | to_entries[0] as $b
          | (if ($b.value | type) == "array" then ($b.value | add // {}) else {} end) as $f
          | "\($group.key) -> \($b.key)" as $where
          | (if ($f.icon // "") == "" then "\($where): missing icon"
             elif ($f.icon | tostring | slug | not) then "\($where): icon \"\($f.icon)\" does not match <slug>[-#RRGGBB]"
             else empty end),
            (if ($f.href // "") == "" then "\($where): missing href"
             elif ($f.href | tostring | test("^https://\\S+$") | not) then "\($where): href \"\($f.href)\" must be an https:// URL"
             else empty end))
      end
  end')"

bookmark_groups="$(printf '%s\n' "${bookmarks_json}" | jq -r 'if type == "array" then .[] | keys[0] else empty end' | sort -u)"
layout_groups="$(printf '%s\n' "${settings}" | yq -r '.layout // {} | keys | .[]' | sort -u)"
service_groups="$(
  {
    printf '%s\n' "${services}" | yq -r '.[] | keys | .[0]'
    grep -rhoE 'gethomepage\.dev/group:[[:space:]]*.+' "${k8s_root}" |
      sed -E 's/^gethomepage\.dev\/group:[[:space:]]*//; s/^["'\'']//; s/["'\''][[:space:]]*$//; s/[[:space:]]+$//' || true
  } | { grep . || true; } | sort -u
)"

# Proof-of-life: an empty side would make checks 4 and 5 pass vacuously.
if [ -z "${service_groups}" ]; then
  problems="${problems:+${problems}$'\n'}no service groups found — check 4 would pass vacuously"
fi
if [ -z "${layout_groups}" ]; then
  problems="${problems:+${problems}$'\n'}settings.yaml has no layout groups — check 5 would pass vacuously"
fi

while IFS= read -r group; do
  [ -n "${group}" ] || continue
  if printf '%s\n' "${service_groups}" | grep -qxF -- "${group}"; then
    problems="${problems:+${problems}$'\n'}${group}: bookmark group reuses a service group name"
  fi
  if ! printf '%s\n' "${layout_groups}" | grep -qxF -- "${group}"; then
    problems="${problems:+${problems}$'\n'}${group}: bookmark group is not listed in settings.yaml layout"
  fi
done <<EOF
${bookmark_groups}
EOF

if [ -n "${problems}" ]; then
  count="$(printf '%s\n' "${problems}" | grep -c .)"
  echo "::error::${count} homepage bookmark violation(s):"
  printf '%s\n' "${problems}" | sort | sed 's/^/  /'
  exit 1
fi

entries="$(printf '%s\n' "${bookmarks_json}" | jq '[.[] | to_entries[0].value[]] | length')"
groups="$(printf '%s\n' "${bookmark_groups}" | grep -c .)"
echo "✓ ${entries} homepage bookmark(s) in ${groups} group(s) valid."
