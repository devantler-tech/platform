# shellcheck shell=bash
# A typed Tenant creates its source inside kro. Until the complete admission
# chain can attribute that source, raw discovery must refuse it, not omit it.
# No publication receipt or approval record grants an exception here.

reject_unattributed_tenant_sources() (
  set -euo pipefail
  local root="$1" work file resolved files=()
  [ -d "$root/k8s" ] || return 0
  root="$(cd "$root" && pwd -P)" || return 1
  work="$(mktemp -d)" || return 1
  trap 'rm -rf "$work"' EXIT
  # Docker declarations are local-only. Production conservation independently
  # refuses them if a production root imports one. Preserve that scope boundary.
  find "$root/k8s" -path "$root/k8s/providers/docker" -prune -o \
    \( -type l -o -type f \) -print0 >"$work/files" || return 1
  while IFS= read -r -d '' file; do
    resolved="$file"
    if [ -L "$file" ]; then
      resolved="$(realpath "$file" 2>/dev/null)" || return 1
      case "$resolved" in "$root/k8s/"*) ;; *) return 1 ;; esac
      # Never traverse a directory link or read an escaping/dangling target.
      [ -f "$resolved" ] || return 1
    fi
    case "$file" in
      *.[yY][aA][mM][lL] | *.[yY][mM][lL] | *.[jJ][sS][oO][nN]) files+=("$resolved") ;;
    esac
  done <"$work/files"
  [ "${#files[@]}" -gt 0 ] || return 0
  # Keep filenames in the parsed envelope so a successful partial parser read
  # cannot attest a complete inventory. Alias/merge expansion precedes classification.
  printf '%s\0' "${files[@]}" | jq -Rs 'split("\u0000")[:-1] | unique' >"$work/expected.json" || return 1
  yq -N -o=json -I=0 '{"file": filename, "document": explode(.)}' "${files[@]}" >"$work/documents.jsonl" || {
    printf 'typed source discovery could not parse every candidate; inventory is UNKNOWN\n' >&2
    return 1
  }
  # Join complete schema GVKs across the whole tree before inspecting instances:
  # an RGD and the objects it creates can live in different roots. A same-kind
  # resource in a different group/version is not one of its instances.
  jq -es --slurpfile expected "$work/expected.json" '
    select(length>0 and all(.[]; type=="object" and keys==["document","file"])) |
    select((map(.file)|unique)==$expected[0]) |
    [.[].document | .. | objects] as $nodes |
    [$nodes[] | select(.kind=="ResourceGraphDefinition") |
      select(any(.. | objects; .kind=="OCIRepository" and has("spec"))) |
      .spec.schema |
      if (.kind|type=="string" and test("^[A-Za-z][A-Za-z0-9]*$")) and
         ((.group // "kro.run")|type=="string" and test("^[a-z0-9]+([.-][a-z0-9]+)*$")) and
         (.apiVersion|type=="string" and test("^[a-z][a-z0-9]*$"))
      then {kind,apiVersion:((.group // "kro.run")+"/"+.apiVersion)}
      else error("source-producing schema GVK is unknown") end] as $schemas |
    if any($nodes[]; . as $node |
      any($schemas[]; .kind==$node.kind and .apiVersion==$node.apiVersion))
    then error("typed source (including business-site) needs complete admission; inventory is UNKNOWN")
    else true end
  ' "$work/documents.jsonl" >/dev/null || return 1
)
