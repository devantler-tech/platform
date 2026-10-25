# shellcheck shell=bash
# A typed Tenant creates its source inside kro. Until the complete admission
# chain can attribute that source, raw discovery must refuse it, not omit it.
# No publication receipt or approval record grants an exception here.

reject_unattributed_tenant_documents() (
  set -euo pipefail
  [ "$#" -gt 0 ] || return 0
  local matches
  # Structural parsing handles block/flow YAML, quoted keys, aliases and JSON.
  # Nested mappings count too: a controller carrier can create the same source.
  matches="$(yq -N -r 'explode(.) | .. | select(type == "!!map" and
    .kind == "Tenant" and .apiVersion == "kro.run/v1alpha1") | filename' "$@")" || {
    if [ "$#" -eq 1 ]; then
      printf 'could not parse %s as YAML, so the consumer set is UNKNOWN\n' "$1" >&2
    else
      printf 'typed Tenant discovery could not parse every candidate; consumer inventory is UNKNOWN\n' >&2
    fi
    return 1
  }
  if [ -n "$matches" ]; then
    printf 'typed Tenant source is not admitted to consumer attribution (including business-site); consumer inventory is UNKNOWN\n' >&2
    return 1
  fi
)

reject_unattributed_tenant_sources() (
  set -euo pipefail
  local root="$1" work file files=()
  [ -d "$root/k8s" ] || return 0
  work="$(mktemp -d)" || return 1
  trap 'rm -rf "$work"' EXIT
  # Docker declarations are local-only. Production conservation independently
  # refuses them if a production root imports one. Preserve that scope boundary.
  find "$root/k8s" -path "$root/k8s/providers/docker" -prune -o \
    -type f \( -name '*.yaml' -o -name '*.yml' \) -print0 >"$work/files" || return 1
  while IFS= read -r -d '' file; do
    files+=("$file")
  done <"$work/files"
  [ "${#files[@]}" -gt 0 ] || return 0
  # Read all candidate files in one parser invocation; a syntax-dependent grep
  # would silently omit quoted-key and JSON Tenant declarations.
  reject_unattributed_tenant_documents "${files[@]}"
)
