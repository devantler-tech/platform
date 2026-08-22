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
  schema_kind="$(yq '.spec.schema.kind // ""' "$candidate")"
  [ -n "$schema_kind" ] || fail "RGD does not declare spec.schema.kind: $candidate"
  RGD_KINDS+=("$schema_kind")
done < <(find "$rgd_root" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

[ "${#RGD_PATHS[@]}" -gt 0 ] || fail "no ResourceGraphDefinitions found below $rgd_root"
readonly RGD_PATHS
readonly RGD_KINDS

# The current RGD contracts use spec.name as the namespace-driving input (directly in WebApp,
# through the generated Namespace in Tenant). Trivy cannot substitute committed custom-resource
# values into the extracted templates, so reject the protected kube-system target separately.
committed_instance_count=0
while IFS= read -r candidate; do
  # Parse the kind instead of grepping its serialized spelling: quotes, comments, and other valid
  # YAML presentation choices must not bypass the instance guard. Non-mapping fixture documents are
  # ignored here and remain owned by the repository's ordinary YAML validation.
  while IFS=$'\t' read -r instance_kind instance_name; do
    is_rgd_instance=false
    for rgd_kind in "${RGD_KINDS[@]}"; do
      if [ "$instance_kind" = "$rgd_kind" ]; then
        is_rgd_instance=true
        break
      fi
    done
    "$is_rgd_instance" || continue
    committed_instance_count=$((committed_instance_count + 1))
    [ "$instance_name" != "kube-system" ] || fail \
      "KSV-0037: committed $instance_kind instance $candidate would generate resources in kube-system"
  done < <(
    yq -o=json -I=0 \
      'select(tag == "!!map") | {"kind": (.kind // ""), "name": (.spec.name // "")}' \
      "$candidate" |
      jq -r 'select(.kind != "") | [.kind, .name] | @tsv'
  )
done < <(find "${SOURCE_ROOT}/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
: >"$WORK/content.tsv"

template_count=0
for relative_path in "${RGD_PATHS[@]}"; do
  source_file="${SOURCE_ROOT}/${relative_path}"
  output_file="${WORK}/${relative_path}"
  [ -r "$source_file" ] || fail "RGD source is not readable: $source_file"

  count="$(yq '.spec.resources | length' "$source_file")"
  case "$count" in
    '' | *[!0-9]*) fail "could not count templates in $source_file" ;;
    0) fail "RGD contains no templates: $source_file" ;;
  esac

  mkdir -p "$(dirname "$output_file")"
  # yq emits one YAML document per selected template. Keeping the original repository-relative
  # path makes each finding attributable to the RGD that introduced it.
  yq '.spec.resources[].template | split_doc' "$source_file" >"$output_file"
  # Hash parsed semantics rather than yq's YAML serialization. jq owns the stable key ordering and
  # compact representation, so a runner yq update that changes only quoting or whitespace cannot
  # invalidate the baseline while a value-sensitive template change still does.
  yq -o=json -I=0 '[.spec.resources[].template]' "$source_file" |
    jq -cS '.' >"$output_file.content.json"
  printf '1\t%s\tTEMPLATE-CONTENT\tSHA256\t%s\n' \
    "$relative_path" "$(sha256_file "$output_file.content.json")" >>"$WORK/content.tsv"
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
  [
    .Results[]? as $result
    | $result.Misconfigurations[]?
    | {
        target: $result.Target,
        id: .ID,
        severity: .Severity,
        location: "\(.CauseMetadata.StartLine // 0)-\(.CauseMetadata.EndLine // 0)"
      }
  ]
  | group_by([.target, .id, .severity, .location])[]
  | [length, .[0].target, .[0].id, .[0].severity, .[0].location]
  | @tsv
' "$WORK/results.json" >"$WORK/findings.tsv"

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
  fail "RGD template content or findings changed; fix regressions and update the baseline for reviewed changes"
fi

echo "PASS: Trivy scanned ${template_count} templates from ${#RGD_PATHS[@]} RGDs and checked ${committed_instance_count} committed instances."
