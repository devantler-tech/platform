# shellcheck shell=bash
# A typed Tenant creates its source inside kro. Only the exact own-release Tenant
# constructor may create one (ADR 0019); all other instances are refused.

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
    local output="$1"
    shift
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
      if [ -f "$resolved" ]; then
        files+=("$resolved")
      elif [ ! -d "$resolved" ]; then
        return 1
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
  # an RGD and the objects it creates can live in different roots.
  tenant_source_inventory_check "$discovery_root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml" \
    "$source_discovery_work/documents.jsonl"
)

# A typed source exists only inside the cluster: kro creates it from a schema instance.
# The Tenant graph's source names only the tenant's own repository's release workflow
# (ADR 0019), so a Tenant is never a shared-publisher consumer and projects nothing.
# Every instance of any other source-producing schema, a cloned Tenant schema, or a
# Tenant graph whose source differs from the reviewed constructor is UNKNOWN and refused.
#   $1  the reviewed Tenant graph file the constructor reader checks
#   $2  JSONL of {file, document} for every document observed (raw tree or render)
tenant_source_inventory_check() (
  set -euo pipefail
  local graph_file="$1" inventory="$2" census graphs
  census="$(jq -cse '
    [.[] | .document | .. | objects] as $nodes |
    [$nodes[] | select(.kind=="ResourceGraphDefinition") |
      select(any(.. | objects; .kind=="OCIRepository" and has("spec")))] as $producers |
    [$producers[] | .spec.schema |
      if (.kind|type=="string" and test("^[A-Za-z][A-Za-z0-9]*$")) and
         ((.group // "kro.run")|type=="string" and test("^[a-z0-9]+([.-][a-z0-9]+)*$")) and
         (.apiVersion|type=="string" and test("^[a-z][a-z0-9]*$"))
      then {kind,apiVersion:((.group // "kro.run")+"/"+.apiVersion)}
      else error("source-producing schema GVK is unknown") end] as $schemas |
    {
      # A Tenant instance counts whether or not its graph was observed: an omitted graph
      # would otherwise leave the instance on whatever constructor the cluster holds.
      instances: [$nodes[] | . as $node |
        select((.apiVersion=="kro.run/v1alpha1" and .kind=="Tenant") or
          any($schemas[]; .kind==$node.kind and .apiVersion==$node.apiVersion)) |
        {apiVersion, kind, name:(.metadata | if type=="object" then (.name // "?") else "?" end)}],
      tenantGraphs: [$producers[] | select(.spec.schema.kind=="Tenant" and
        ((.spec.schema.group // "kro.run")+"/"+.spec.schema.apiVersion)=="kro.run/v1alpha1")]
    }' "$inventory")" || {
    printf 'typed source discovery could not classify every source-producing schema; inventory is UNKNOWN\n' >&2
    return 1
  }
  [ "$(printf '%s\n' "$census" | jq -r '.instances|length')" != 0 ] || return 0
  if [ "$(printf '%s\n' "$census" | jq -r '[.instances[]|select(.apiVersion!="kro.run/v1alpha1" or .kind!="Tenant")]|length')" != 0 ]; then
    printf 'typed source of a schema other than the Tenant graph; inventory is UNKNOWN: %s\n' \
      "$(printf '%s\n' "$census" | jq -c .instances)" >&2
    return 1
  fi
  # A cloned schema is ambiguous even when its CEL happens to be identical.
  if [ "$(printf '%s\n' "$census" | jq -r '[.tenantGraphs[]|select(.metadata.name=="tenant.kro.run")]|length')" != 1 ] ||
    [ "$(printf '%s\n' "$census" | jq -r '.tenantGraphs|length')" != 1 ]; then
    printf 'Tenant instances need exactly one Tenant graph; inventory is UNKNOWN\n' >&2
    return 1
  fi
  # The observed graph itself, and the reviewed file, must both be the exact constructor.
  graphs="$(mktemp)" || return 1
  printf '%s\n' "$census" | jq -c '.tenantGraphs[0]' >"$graphs" || {
    rm -f -- "$graphs"
    return 1
  }
  # shellcheck source=scripts/tenant-source-constructors.lib.sh
  source "$(dirname "${BASH_SOURCE[0]}")/tenant-source-constructors.lib.sh"
  if ! tenant_source_default_subject "$graphs" >/dev/null || ! tenant_source_default_subject "$graph_file" >/dev/null; then
    rm -f -- "$graphs"
    printf 'Tenant instances need the exact own-release source constructor; inventory is UNKNOWN\n' >&2
    return 1
  fi
  rm -f -- "$graphs"
)

# Validation callers receive no projection stream or partial inventory.
reject_unattributed_tenant_sources() {
  canonical_tenant_source_documents "$1" >/dev/null
}
