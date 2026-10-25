# shellcheck shell=bash
# A typed Tenant creates its source inside kro. Until the complete admission
# chain can attribute that source, raw discovery must refuse it, not omit it.
# No publication receipt or approval record grants an exception here.

reject_unattributed_tenant_sources() (
  set -euo pipefail
  local root="$1" work file resolved resource resource_file files=() kustomizations=()
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
      *.[yY][aA][mM][lL] | *.[yY][mM][lL] | *.[jJ][sS][oO][nN] | */Kustomization) files+=("$resolved") ;;
    esac
    case "$file" in
      */kustomization.yaml | */kustomization.yml | */Kustomization) kustomizations+=("$file") ;;
    esac
  done <"$work/files"
  # Every parser invocation proves its complete filename set, including the
  # Kustomize reference pass; a partial read can never narrow the later census.
  read_source_documents() {
    local output="$1"; shift
    printf '%s\0' "$@" | jq -Rs 'split("\u0000")[:-1] | unique' >"$output.expected.json" || return 1
    yq -N -o=json -I=0 '{"file": filename, "document": explode(.)}' "$@" >"$output" || return 1
    jq -es --slurpfile expected "$output.expected.json" '
      length>0 and all(.[]; type=="object" and keys==["document","file"]) and
      ((map(.file)|unique)==$expected[0])
    ' "$output" >/dev/null || return 1
  }
  if [ "${#kustomizations[@]}" -gt 0 ]; then
    read_source_documents "$work/kustomizations.jsonl" "${kustomizations[@]}" || return 1
    jq -jrs '
      .[] | .file as $file | .document |
      (.resources // [], .components // [], .bases // []) |
      if type!="array" then error("invalid local resource references") else .[] end |
      if type!="string" or length==0 or test("[\u0000-\u001f]")
      then error("invalid local resource path") else $file,"\u0000",.,"\u0000" end
    ' "$work/kustomizations.jsonl" >"$work/references" || return 1
    while IFS= read -r -d '' file && IFS= read -r -d '' resource; do
      # Remote resources retain their separate reviewed render/conservation
      # boundary. This raw discovery helper never fetches a declared URL.
      case "$resource" in https://*) continue ;; esac
      resource_file="${file%/*}/$resource"
      case "$resource" in /*) resource_file="$resource" ;; esac
      resolved="$(realpath "$resource_file" 2>/dev/null)" || return 1
      case "$resolved" in "$root/k8s" | "$root/k8s/"*) ;; *) return 1 ;; esac
      if [ -f "$resolved" ]; then files+=("$resolved")
      elif [ ! -d "$resolved" ]; then return 1
      fi
    done <"$work/references"
  fi
  [ "${#files[@]}" -gt 0 ] || return 0
  # Keep filenames in the parsed envelope so a successful partial parser read
  # cannot attest a complete inventory. Alias/merge expansion precedes classification.
  read_source_documents "$work/documents.jsonl" "${files[@]}" || {
    printf 'typed source discovery could not parse every candidate; inventory is UNKNOWN\n' >&2
    return 1
  }
  # Join complete schema GVKs across the whole tree before inspecting instances:
  # an RGD and the objects it creates can live in different roots. A same-kind
  # resource in a different group/version is not one of its instances.
  jq -es --arg prefix "$root/" '
    [.[] | .file as $file | .document | .. | objects | {file:$file,resource:.}] as $nodes |
    [$nodes[].resource | select(.kind=="ResourceGraphDefinition") |
      select(any(.. | objects; .kind=="OCIRepository" and has("spec"))) |
      .spec.schema |
      if (.kind|type=="string" and test("^[A-Za-z][A-Za-z0-9]*$")) and
         ((.group // "kro.run")|type=="string" and test("^[a-z0-9]+([.-][a-z0-9]+)*$")) and
         (.apiVersion|type=="string" and test("^[a-z][a-z0-9]*$"))
      then {kind,apiVersion:((.group // "kro.run")+"/"+.apiVersion)}
      else error("source-producing schema GVK is unknown") end] as $schemas |
    [$nodes[] | .resource as $node |
      select(any($schemas[]; .kind==$node.kind and .apiVersion==$node.apiVersion)) |
      {file:(.file|ltrimstr($prefix)),
       name:(.resource.metadata | if type=="object" then (.name // "?") else "?" end)}] as $hits |
    if ($hits|length)>0
    then error("typed source needs complete admission; inventory is UNKNOWN: " +
      ($hits | map((.file|@json) + " (" + (.name|tojson) + ")") | join(", ")))
    else true end
  ' "$work/documents.jsonl" >/dev/null || return 1
)
