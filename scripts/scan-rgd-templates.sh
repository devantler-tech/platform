#!/usr/bin/env bash
# Extract the Kubernetes resources nested in KRO ResourceGraphDefinitions and scan those resources
# as first-class manifests. Trivy otherwise sees only the outer custom resource and skips the
# workload-level checks that protect ordinary committed manifests.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [source-root]" >&2
  exit 2
fi

SOURCE_ROOT="$(cd "${1:-$REPO_ROOT}" && pwd)"
readonly SOURCE_ROOT
readonly CONFIG_DATA="${REPO_ROOT}/.trivy/data"
readonly BASELINE="${REPO_ROOT}/scripts/rgd-template-static-scan-baseline.tsv"
readonly REQUIRED_TRIVY_VERSION="0.74.0"
readonly RGD_ROOT_PATH="k8s/bases/infrastructure/resource-graph-definitions"

RGD_PATHS=()
RGD_API_VERSIONS=()
RGD_KINDS=()

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail "yq is required to extract RGD templates"
command -v trivy >/dev/null 2>&1 || fail "trivy is required to scan RGD templates"
command -v jq >/dev/null 2>&1 || fail "jq is required to compare RGD template findings"
[ -d "$CONFIG_DATA" ] || fail "Trivy policy data is not readable: $CONFIG_DATA"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "sha256sum or shasum is required to preserve RGD template cause evidence"
  fi
}

sha256_text() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s\n' "$1" | sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s\n' "$1" | shasum -a 256 | awk '{print $1}'
  else
    fail "sha256sum or shasum is required to preserve RGD instance evidence"
  fi
}

TRUSTED_REGISTRIES=()
while IFS= read -r trusted_registry; do
  [ -n "$trusted_registry" ] && TRUSTED_REGISTRIES+=("$trusted_registry")
done < <(yq '.ksv0125.trusted_registries[]' "${CONFIG_DATA}/trusted-registries.yaml")
[ "${#TRUSTED_REGISTRIES[@]}" -gt 0 ] || fail "no trusted registries are configured"
readonly TRUSTED_REGISTRIES

validate_image_registry() {
  local image="$1" first_segment registry trusted_registry
  first_segment="${image%%/*}"
  if [[ "$image" != */* || ("$first_segment" != *.* && "$first_segment" != *:* && "$first_segment" != "localhost") ]]; then
    registry="docker.io"
  else
    registry="$first_segment"
  fi

  for trusted_registry in "${TRUSTED_REGISTRIES[@]}"; do
    # Trivy KSV-0125 uses a suffix match, but committed RGD inputs need an exact host boundary so a
    # look-alike registry cannot inherit trust. Legitimate aliases belong in the reviewed data list.
    [ "$registry" = "$trusted_registry" ] && return 0
  done
  fail "KSV-0125: committed RGD instance image $image uses untrusted registry $registry"
}

installed_trivy_version="$(trivy --version | sed -n 's/^Version: //p' | head -n 1)"
[ "$installed_trivy_version" = "$REQUIRED_TRIVY_VERSION" ] || fail \
  "Trivy $REQUIRED_TRIVY_VERSION is required by the finding baseline; found $installed_trivy_version"

rgd_root="${SOURCE_ROOT}/${RGD_ROOT_PATH}"
[ -d "$rgd_root" ] || fail "RGD source directory is missing: $rgd_root"

# Discover by resource kind rather than by today's two directory names. The tree also contains
# sibling ClusterRoles, so a filename glob alone would scan unrelated manifests; conversely, a
# fixed allow-list would silently recreate this blind spot when the next RGD is added.
while IFS= read -r candidate; do
  [ "$(yq '.kind // ""' "$candidate")" = "ResourceGraphDefinition" ] || continue
  RGD_PATHS+=("${candidate#"$SOURCE_ROOT"/}")
  definition_api_version="$(yq '.apiVersion // ""' "$candidate")"
  [[ "$definition_api_version" == */* ]] ||
    fail "RGD does not declare a grouped apiVersion: $candidate"
  schema_api_version="$(yq '.spec.schema.apiVersion // ""' "$candidate")"
  [ -n "$schema_api_version" ] || fail "RGD does not declare spec.schema.apiVersion: $candidate"
  if [[ "$schema_api_version" == */* ]]; then
    generated_api_version="$schema_api_version"
  else
    definition_name="$(yq '.metadata.name // ""' "$candidate")"
    generated_api_group="${definition_name#*.}"
    if [ -z "$definition_name" ] || [ "$generated_api_group" = "$definition_name" ] ||
      [ -z "$generated_api_group" ]; then
      fail "RGD metadata.name does not encode a generated API group: $candidate"
    fi
    generated_api_version="${generated_api_group}/${schema_api_version}"
  fi
  RGD_API_VERSIONS+=("$generated_api_version")
  schema_kind="$(yq '.spec.schema.kind // ""' "$candidate")"
  [ -n "$schema_kind" ] || fail "RGD does not declare spec.schema.kind: $candidate"
  RGD_KINDS+=("$schema_kind")
done < <(find "$rgd_root" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

[ "${#RGD_PATHS[@]}" -gt 0 ] || fail "no ResourceGraphDefinitions found below $rgd_root"
readonly RGD_PATHS
readonly RGD_API_VERSIONS
readonly RGD_KINDS

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
: >"$WORK/content.tsv"

# The current RGD contracts use spec.name as the namespace-driving input (directly in WebApp,
# through the generated Namespace in Tenant). Trivy cannot substitute committed custom-resource
# values into the extracted templates, so reject explicit policy violations and ratchet the complete
# parsed instance to keep every substituted value review-visible.
committed_instance_count=0
while IFS= read -r candidate; do
  # Parse the kind instead of grepping its serialized spelling: quotes, comments, and other valid
  # YAML presentation choices must not bypass the instance guard. Non-mapping fixture documents are
  # ignored here and remain owned by the repository's ordinary YAML validation.
  while IFS= read -r instance_json; do
    instance_api_version="$(jq -r '.apiVersion // ""' <<<"$instance_json")"
    instance_kind="$(jq -r '.kind // ""' <<<"$instance_json")"
    instance_name="$(jq -r '.spec.name // ""' <<<"$instance_json")"
    is_rgd_instance=false
    for rgd_index in "${!RGD_KINDS[@]}"; do
      if [ "$instance_api_version" = "${RGD_API_VERSIONS[$rgd_index]}" ] &&
        [ "$instance_kind" = "${RGD_KINDS[$rgd_index]}" ]; then
        is_rgd_instance=true
        break
      fi
    done
    "$is_rgd_instance" || continue
    committed_instance_count=$((committed_instance_count + 1))
    [ "$instance_name" != "kube-system" ] || fail \
      "KSV-0037: committed $instance_kind instance $candidate would generate resources in kube-system"
    instance_image="$(jq -r '.spec.image // ""' <<<"$instance_json")"
    [ -z "$instance_image" ] || validate_image_registry "$instance_image"
    printf '1\t%s\tINSTANCE-CONTENT\tSHA256\t%s\n' \
      "${candidate#"$SOURCE_ROOT"/}" "$(sha256_text "$instance_json")" >>"$WORK/content.tsv"
  done < <(
    yq -o=json -I=0 'select(tag == "!!map")' "$candidate" |
      jq -cS 'select(.kind != null)'
  )
done < <(find "${SOURCE_ROOT}/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

template_count=0
for relative_path in "${RGD_PATHS[@]}"; do
  source_file="${SOURCE_ROOT}/${relative_path}"
  [ -r "$source_file" ] || fail "RGD source is not readable: $source_file"

  count="$(yq '.spec.resources | length' "$source_file")"
  case "$count" in
    '' | *[!0-9]*) fail "could not count templates in $source_file" ;;
    0) fail "RGD contains no templates: $source_file" ;;
  esac

  resource_count=0
  while IFS= read -r resource_id; do
    [ -n "$resource_id" ] || fail "RGD resource does not declare an id: $source_file"
    [[ "$resource_id" =~ ^[A-Za-z0-9._-]+$ ]] ||
      fail "RGD resource id cannot be represented as a finding target: $resource_id"
    output_file="${WORK}/${relative_path}.resources/${resource_id}.yaml"
    [ ! -e "$output_file" ] || fail "RGD resource id is duplicated: $resource_id in $source_file"
    mkdir -p "$(dirname "$output_file")"
    # One file per stable graph resource ID keeps findings attributable without depending on yq's
    # emitted line positions. The original RGD path remains the prefix for source ownership.
    RESOURCE_ID="$resource_id" yq \
      '(.spec.resources[] | select(.id == strenv(RESOURCE_ID))).template' \
      "$source_file" >"$output_file"
    resource_count=$((resource_count + 1))
  done < <(yq '.spec.resources[].id // ""' "$source_file")
  [ "$resource_count" -eq "$count" ] ||
    fail "RGD resource IDs do not map one-to-one with templates: $source_file"

  # Hash the complete graph spec: schema defaults plus every resource definition, including
  # includeWhen/forEach controls. jq owns stable key ordering and a compact representation, so a
  # runner yq update that changes only quoting or whitespace cannot invalidate the baseline.
  content_file="${WORK}/${relative_path}.content.json"
  mkdir -p "$(dirname "$content_file")"
  yq -o=json -I=0 '.spec' "$source_file" |
    jq -cS '.' >"$content_file"
  printf '1\t%s\tRGD-CONTENT\tSHA256\t%s\n' \
    "$relative_path" "$(sha256_file "$content_file")" >>"$WORK/content.tsv"
  template_count=$((template_count + count))
done

# Do not load the repository ignorefile here. Some of its existing path-scoped infrastructure
# dispositions predate this extraction and would hide generated tenant workloads merely because
# their source RGD lives below k8s/bases/infrastructure. Instead, run every Trivy check and ratchet
# the exact finding counts below: known findings remain visible, while any new finding fails. Use
# the policies embedded in the pinned binary rather than an independently moving remote bundle.
(
  cd "$WORK"
  if ! TRIVY_CACHE_DIR="$WORK/trivy-cache" trivy fs \
    --scanners misconfig \
    --exit-code 0 \
    --format json \
    --output "$WORK/results.json" \
    --skip-check-update \
    --config-data "$CONFIG_DATA" \
    . 2>"$WORK/trivy.log"; then
    sed 's/^/  /' "$WORK/trivy.log" >&2
    fail "Trivy could not scan the extracted RGD templates"
  fi
)

# An empty per-run cache plus --skip-check-update forces Trivy to its versioned embedded policies.
# Assert that path explicitly: silently using another source would make the baseline non-reproducible.
grep -Fq 'Falling back to embedded checks' "$WORK/trivy.log" || fail \
  "Trivy did not confirm that it used the v${REQUIRED_TRIVY_VERSION} embedded policy set"

jq -r '
  .Results[]? as $result
  | $result.Misconfigurations[]?
  | [
      $result.Target,
      .ID,
      .Severity,
      (.CauseMetadata.StartLine | tostring),
      (.CauseMetadata.EndLine | tostring)
    ]
  | @tsv
' "$WORK/results.json" >"$WORK/raw-findings.tsv"

: >"$WORK/cause-findings.tsv"
while IFS=$'\t' read -r finding_target finding_id finding_severity \
  finding_start_line finding_end_line; do
  case "$finding_target" in
    /* | ../* | */../*) fail "Trivy returned an unsafe finding target: $finding_target" ;;
  esac
  case "$finding_start_line" in
    '' | *[!0-9]*) fail "Trivy finding $finding_id on $finding_target has no source range" ;;
  esac
  case "$finding_end_line" in
    '' | *[!0-9]*) fail "Trivy finding $finding_id on $finding_target has no source range" ;;
  esac
  [ "$finding_start_line" -le "$finding_end_line" ] || fail \
    "Trivy finding $finding_id on $finding_target has an inverted source range"
  finding_file="${WORK}/${finding_target}"
  [ -r "$finding_file" ] || fail "Trivy finding target is not readable: $finding_target"

  first_content_line="$(awk 'NF { print NR; exit }' "$finding_file")"
  [ -n "$first_content_line" ] || fail "Trivy finding target is empty: $finding_target"
  leading_blank_lines=$((first_content_line - 1))
  [ "$finding_start_line" -gt "$leading_blank_lines" ] || fail \
    "Trivy finding $finding_id on $finding_target starts before its YAML document"
  semantic_start_line=$((finding_start_line - leading_blank_lines))
  semantic_end_line=$((finding_end_line - leading_blank_lines))

  # Trivy v0.74.0 reports a source range, not a JSON path. Resolve every scalar path in that range
  # and keep their common semantic parent. This distinguishes byte-identical findings on
  # containers[0] and containers[1]. Leading serializer blanks move Trivy's physical line while yq
  # normalizes them, so remove that measured offset before resolving the path.
  cause_paths="$(CAUSE_START="$semantic_start_line" CAUSE_END="$semantic_end_line" \
    yq -o=json -I=0 \
    '[.. |
      select(line >= (env(CAUSE_START) | tonumber) and
        line <= (env(CAUSE_END) | tonumber) and tag != "!!map" and tag != "!!seq") |
      path]' "$finding_file")"
  cause_path="$(jq -c '
    . as $paths
    | if length == 0 then empty
      else
        (map(length) | min) as $limit
        | ([range(0; $limit) as $index
            | select(($paths | map(.[$index]) | unique | length) > 1)
            | $index] | first // $limit) as $cut
        | $paths[0][0:$cut]
      end
  ' <<<"$cause_paths")"
  if [ -z "$cause_path" ] || [ "$cause_path" = "null" ]; then
    fail "Trivy finding $finding_id on $finding_target has no semantic path in its source range"
  fi

  # Hash parsed semantics, never Trivy's serializer-rendered Code.Lines. jq owns stable key order,
  # so quoting or scalar-presentation changes cannot move an unchanged finding baseline.
  canonical_cause="$(
    yq -o=json -I=0 '.' "$finding_file" |
      jq -cS --argjson cause_path "$cause_path" 'getpath($cause_path)'
  )"
  if [ -z "$canonical_cause" ] || [ "$canonical_cause" = "null" ]; then
    fail "Trivy finding $finding_id on $finding_target has no canonical cause node"
  fi
  cause_identity="${cause_path}"$'\n'"${canonical_cause}"
  printf '%s\t%s\t%s\tCAUSE-SHA256:%s\n' \
    "$finding_target" "$finding_id" "$finding_severity" \
    "$(sha256_text "$cause_identity")" >>"$WORK/cause-findings.tsv"
done <"$WORK/raw-findings.tsv"

jq -R -s -r '
  [
    split("\n")[]
    | select(length > 0)
    | split("\t")
    | {target: .[0], id: .[1], severity: .[2], cause: .[3]}
  ]
  | group_by([.target, .id, .severity, .cause])[]
  | [length, .[0].target, .[0].id, .[0].severity, .[0].cause]
  | @tsv
' "$WORK/cause-findings.tsv" >"$WORK/findings.tsv"

{
  cat "$WORK/findings.tsv"
  cat "$WORK/content.tsv"
} | LC_ALL=C sort -t $'\t' -k2,2 -k3,3 -k4,4 -k5,5 >"$WORK/actual.tsv"

if [ ! -r "$BASELINE" ]; then
  echo "The RGD template finding baseline is missing. Current findings:" >&2
  sed 's/^/  /' "$WORK/actual.tsv" >&2
  fail "baseline is not readable: $BASELINE"
fi

grep -v '^[[:space:]]*#' "$BASELINE" | sed '/^[[:space:]]*$/d' >"$WORK/expected.tsv"
if ! diff -u "$WORK/expected.tsv" "$WORK/actual.tsv"; then
  fail "RGD graph, instance, or finding evidence changed; fix regressions and update the baseline for reviewed changes"
fi

echo "PASS: Trivy scanned ${template_count} templates from ${#RGD_PATHS[@]} RGDs and checked ${committed_instance_count} committed instances."
