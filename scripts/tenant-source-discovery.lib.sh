# shellcheck shell=bash
# A typed Tenant creates its source inside kro. Only the complete qualified
# admission chain may attribute that source; all other instances are refused.
# A publication-evidence receipt alone never grants admission.

canonical_tenant_source_documents() (
  set -euo pipefail
  local discovery_root="$1" file resolved resource resource_file files=() kustomizations=()
  [ -d "$discovery_root/k8s" ] || return 0
  discovery_root="$(cd "$discovery_root" && pwd -P)" || return 1
  source_discovery_work="$(mktemp -d)" || return 1
  # This private-subshell variable survives return unwinding local variables.
  # A caller's unrelated work reservation must never become the cleanup path.
  trap 'rm -rf -- "$source_discovery_work"' EXIT
  # Docker declarations are local-only. Production conservation independently
  # refuses them if a production root imports one. Preserve that scope boundary.
  find "$discovery_root/k8s" -path "$discovery_root/k8s/providers/docker" -prune -o \
    \( -type l -o -type f \) -print0 >"$source_discovery_work/files" || return 1
  while IFS= read -r -d '' file; do
    resolved="$file"
    if [ -L "$file" ]; then
      resolved="$(realpath "$file" 2>/dev/null)" || return 1
      case "$resolved" in "$discovery_root/k8s/"*) ;; *) return 1 ;; esac
      # Never traverse a directory link or read an escaping/dangling target.
      [ -f "$resolved" ] || return 1
    fi
    case "$file" in
      *.[yY][aA][mM][lL] | *.[yY][mM][lL] | *.[jJ][sS][oO][nN] | */Kustomization) files+=("$resolved") ;;
    esac
    case "$file" in
      */kustomization.yaml | */kustomization.yml | */Kustomization) kustomizations+=("$file") ;;
    esac
  done <"$source_discovery_work/files"
  # Every parser invocation proves its complete filename set, including the
  # Kustomize reference pass; a partial read can never narrow the later census.
  read_source_documents() {
    local output="$1"; shift
    printf '%s\0' "$@" | jq -Rs 'split("\u0000")[:-1] | unique' >"$output.expected.json" || return 1
    # YAML also accepts JSON. Older yq releases infer one input format for the
    # entire argument list, so mixed suffixes must never select that format.
    yq -p=yaml -N -o=json -I=0 '{"file": filename, "document": explode(.)}' "$@" >"$output" || return 1
    jq -es --slurpfile expected "$output.expected.json" '
      length>0 and all(.[]; type=="object" and keys==["document","file"]) and
      ((map(.file)|unique)==$expected[0])
    ' "$output" >/dev/null || return 1
  }
  if [ "${#kustomizations[@]}" -gt 0 ]; then
    read_source_documents "$source_discovery_work/kustomizations.jsonl" "${kustomizations[@]}" || return 1
    jq -jrs '
      .[] | .file as $file | .document |
      (.resources // [], .components // [], .bases // []) |
      if type!="array" then error("invalid local resource references") else .[] end |
      if type!="string" or length==0 or test("[\u0000-\u001f]")
      then error("invalid local resource path") else $file,"\u0000",.,"\u0000" end
    ' "$source_discovery_work/kustomizations.jsonl" >"$source_discovery_work/references" || return 1
    while IFS= read -r -d '' file && IFS= read -r -d '' resource; do
      # Remote resources retain their separate reviewed render/conservation
      # boundary. This raw discovery helper never fetches a declared URL.
      case "$resource" in https://*) continue ;; esac
      resource_file="${file%/*}/$resource"
      case "$resource" in /*) resource_file="$resource" ;; esac
      resolved="$(realpath "$resource_file" 2>/dev/null)" || return 1
      case "$resolved" in "$discovery_root/k8s" | "$discovery_root/k8s/"*) ;; *) return 1 ;; esac
      if [ -f "$resolved" ]; then files+=("$resolved")
      elif [ ! -d "$resolved" ]; then return 1
      fi
    done <"$source_discovery_work/references"
  fi
  [ "${#files[@]}" -gt 0 ] || return 0
  # A resource reference can name a file already found by the suffix scan.
  # Deduplicate input paths, never Kubernetes declarations inside those files.
  printf '%s\0' "${files[@]}" | jq -jRs 'split("\u0000")[:-1] | unique[] | .+"\u0000"' >"$source_discovery_work/unique-files" || return 1
  files=()
  while IFS= read -r -d '' file; do files+=("$file"); done <"$source_discovery_work/unique-files"
  # Keep filenames in the parsed envelope so a successful partial parser read
  # cannot attest a complete inventory. Alias/merge expansion precedes classification.
  read_source_documents "$source_discovery_work/documents.jsonl" "${files[@]}" || {
    printf 'typed source discovery could not parse every candidate; inventory is UNKNOWN\n' >&2
    return 1
  }
  # Join complete schema GVKs across the whole tree before inspecting instances:
  # an RGD and the objects it creates can live in different roots. A same-kind
  # resource in a different group/version is not one of its instances.
  # Only an explicit fully qualified binding may project a source. Keep all
  # output buffered until every discovered producer and instance is attributed.
  # shellcheck source=scripts/canonical-tenant-source.lib.sh
  source "$(dirname "${BASH_SOURCE[0]}")/canonical-tenant-source.lib.sh"
  canonical_typed_source_hits "$discovery_root" "$source_discovery_work/documents.jsonl" >"$source_discovery_work/hits.json" || return 1
  canonical_tenant_inventory_projection "$discovery_root" "$source_discovery_work/documents.jsonl" "$source_discovery_work/hits.json"
)

# Validation callers receive no projection stream or partial inventory.
reject_unattributed_tenant_sources() {
  canonical_tenant_source_documents "$1" >/dev/null
}
