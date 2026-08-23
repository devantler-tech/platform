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
if [ -n "${RGD_TEST_REMOTE_BASELINE:-}" ] && [ "$SOURCE_ROOT" = "$REPO_ROOT" ]; then
  echo "RGD_TEST_REMOTE_BASELINE is only permitted for an isolated fixture source" >&2
  exit 2
fi
readonly REMOTE_BASELINE="${RGD_TEST_REMOTE_BASELINE:-${REPO_ROOT}/scripts/rgd-template-static-scan-remote-resources.tsv}"
readonly REQUIRED_TRIVY_VERSION="0.74.0"
readonly MAX_REMOTE_RESOURCE_BYTES=1048576
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
command -v curl >/dev/null 2>&1 || fail "curl is required to verify pinned remote resources"
[ -d "$CONFIG_DATA" ] || fail "Trivy policy data is not readable: $CONFIG_DATA"
[ -r "$REMOTE_BASELINE" ] || fail "remote-resource baseline is not readable: $REMOTE_BASELINE"

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
k8s_root="${SOURCE_ROOT}/k8s"
[ -d "$k8s_root" ] || fail "Kubernetes source directory is missing: $k8s_root"
WORK="$(mktemp -d)"
REMOTE_WORK="$(mktemp -d)"
trap 'rm -rf "$WORK" "$REMOTE_WORK"' EXIT

KUSTOMIZATION_PATHS=()
while IFS= read -r kustomization_file; do
  KUSTOMIZATION_PATHS+=("$kustomization_file")
done < <(
  find "$k8s_root" -type f \
    \( -name 'kustomization.yaml' -o -name 'kustomization.yml' -o -name 'Kustomization' \) \
    -print | LC_ALL=C sort
)
readonly KUSTOMIZATION_PATHS

# This scanner proves committed source inputs. An intentional remote resource therefore needs an
# exact URL and content digest in the reviewed baseline; every other non-local entry fails closed.
# Download in the main shell so a fetch/digest error cannot be lost inside the aggregation subshell.
REMOTE_RESOURCE_URLS=()
REMOTE_RESOURCE_PATHS=()
remote_resource_index=0
if [ "${#KUSTOMIZATION_PATHS[@]}" -gt 0 ]; then
  for kustomization_file in "${KUSTOMIZATION_PATHS[@]}"; do
    while IFS= read -r resource_path; do
      [ -n "$resource_path" ] || continue
      resource_file="${kustomization_file%/*}/${resource_path}"
      [ -e "$resource_file" ] && continue
      expected_remote_sha="$(awk -F '\t' -v url="$resource_path" '
        $1 == url { print $2 }
      ' "$REMOTE_BASELINE")"
      [[ "$expected_remote_sha" =~ ^[0-9a-f]{64}$ ]] || fail \
        "Kustomization resource is not a local path or reviewed remote ($resource_path): $kustomization_file"
      remote_resource_index=$((remote_resource_index + 1))
      remote_resource_file="${REMOTE_WORK}/remote-resource-${remote_resource_index}.yaml"
      curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
        --connect-timeout 10 --max-time 60 \
        --max-filesize "$MAX_REMOTE_RESOURCE_BYTES" \
        --retry 3 --retry-all-errors "$resource_path" --output "$remote_resource_file" || fail \
        "could not fetch reviewed remote Kustomize resource: $resource_path"
      actual_remote_sha="$(sha256_file "$remote_resource_file")"
      [ "$actual_remote_sha" = "$expected_remote_sha" ] || fail \
        "remote Kustomize resource digest changed ($resource_path): expected $expected_remote_sha, got $actual_remote_sha"
      REMOTE_RESOURCE_URLS+=("$resource_path")
      REMOTE_RESOURCE_PATHS+=("$remote_resource_file")
    done < <(yq -o=json -I=0 '(.resources[]?, .components[]?, .bases[]?)' "$kustomization_file" |
      jq -r 'select(type == "string")')
  done
fi
readonly REMOTE_RESOURCE_URLS
readonly REMOTE_RESOURCE_PATHS

# YAML suffixes cover the committed tree today, while Kustomize also accepts JSON and extensionless
# resources. Add every readable file referenced by a build file so direct-main/manual-CD validation
# cannot deploy a resource that discovery omitted merely because of its presentation or filename.
K8S_RESOURCE_PATHS=()
while IFS= read -r candidate; do
  K8S_RESOURCE_PATHS+=("$candidate")
done < <(
  {
    find "$k8s_root" -type f \( -name '*.yaml' -o -name '*.yml' \) -print
    if [ "${#KUSTOMIZATION_PATHS[@]}" -gt 0 ]; then
      for kustomization_file in "${KUSTOMIZATION_PATHS[@]}"; do
        while IFS= read -r resource_path; do
          [ -n "$resource_path" ] || continue
          resource_file="${kustomization_file%/*}/${resource_path}"
          [ -f "$resource_file" ] || continue
          resource_dir="$(cd "$(dirname "$resource_file")" && pwd)"
          printf '%s/%s\n' "$resource_dir" "$(basename "$resource_file")"
        done < <(yq -o=json -I=0 '(.resources[]?, .components[]?, .bases[]?)' "$kustomization_file" |
          jq -r 'select(type == "string")')
      done
    fi
  } | LC_ALL=C sort -u
)
readonly K8S_RESOURCE_PATHS

# Discover by resource kind across every provider as well as the shared base. The tree contains
# sibling resources, so a filename glob alone would scan unrelated manifests; conversely, rooting
# discovery only in the base would leave a provider-specific RGD and its nested workloads unseen.
while IFS= read -r candidate; do
  candidate_documents="$(yq -o=json -I=0 '.' "$candidate" | jq -cs '.')"
  nested_rgd_count="$(jq '
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    [
      .[]
      | select(type == "object" and (.kind // "") == "List")
      | .items[]?
      | resources
      | select(type == "object" and (.kind // "") == "ResourceGraphDefinition")
    ] | length
  ' <<<"$candidate_documents")"
  [ "$nested_rgd_count" -eq 0 ] || fail \
    "Kubernetes List contains a ResourceGraphDefinition that cannot be baselined safely: $candidate"
  rgd_document_count="$(jq '[.[] | select(type == "object" and (.kind // "") ==
    "ResourceGraphDefinition")] | length' <<<"$candidate_documents")"
  [ "$rgd_document_count" -gt 0 ] || continue
  encrypted_rgd_count="$(jq '[
    .[]
    | select(type == "object" and (.kind // "") == "ResourceGraphDefinition")
    | select(
        (.sops != null)
        or ([.. | strings | select(startswith("ENC["))] | length > 0))
  ] | length' <<<"$candidate_documents")"
  [ "$encrypted_rgd_count" -eq 0 ] || fail \
    "SOPS-encrypted ResourceGraphDefinitions cannot be validated from ciphertext: $candidate"
  document_count="$(jq 'length' <<<"$candidate_documents")"
  if [ "$document_count" -ne 1 ] || [ "$rgd_document_count" -ne 1 ]; then
    fail "ResourceGraphDefinition must be the only YAML document in its file: $candidate"
  fi
  substitution_mode="$(jq -r '.[0].metadata.annotations[
    "kustomize.toolkit.fluxcd.io/substitute"] // ""' <<<"$candidate_documents")"
  [ "$substitution_mode" = "disabled" ] || fail \
    "Flux postBuild substitution must be disabled for every ResourceGraphDefinition: $candidate"
  RGD_PATHS+=("${candidate#"$SOURCE_ROOT"/}")
  definition_api_version="$(yq -r '.apiVersion // ""' "$candidate")"
  [[ "$definition_api_version" == */* ]] ||
    fail "RGD does not declare a grouped apiVersion: $candidate"
  schema_api_version="$(yq -r '.spec.schema.apiVersion // ""' "$candidate")"
  [ -n "$schema_api_version" ] || fail "RGD does not declare spec.schema.apiVersion: $candidate"
  if [[ "$schema_api_version" == */* ]]; then
    generated_api_version="$schema_api_version"
  else
    definition_name="$(yq -r '.metadata.name // ""' "$candidate")"
    generated_api_group="${definition_name#*.}"
    if [ -z "$definition_name" ] || [ "$generated_api_group" = "$definition_name" ] ||
      [ -z "$generated_api_group" ]; then
      fail "RGD metadata.name does not encode a generated API group: $candidate"
    fi
    generated_api_version="${generated_api_group}/${schema_api_version}"
  fi
  RGD_API_VERSIONS+=("$generated_api_version")
  schema_kind="$(yq -r '.spec.schema.kind // ""' "$candidate")"
  [ -n "$schema_kind" ] || fail "RGD does not declare spec.schema.kind: $candidate"
  RGD_KINDS+=("$schema_kind")
done < <(printf '%s\n' "${K8S_RESOURCE_PATHS[@]}")

[ "${#RGD_PATHS[@]}" -gt 0 ] || fail "no ResourceGraphDefinitions found below $k8s_root"
readonly RGD_PATHS
readonly RGD_API_VERSIONS
readonly RGD_KINDS

rgd_selectors_json='[]'
for rgd_index in "${!RGD_KINDS[@]}"; do
  generated_api_group="${RGD_API_VERSIONS[$rgd_index]%/*}"
  generated_api_version="${RGD_API_VERSIONS[$rgd_index]#*/}"
  rgd_selectors_json="$(jq -c \
    --arg group "$generated_api_group" \
    --arg version "$generated_api_version" \
    --arg kind "${RGD_KINDS[$rgd_index]}" \
    '. + [{group: $group, version: $version, kind: $kind}]' <<<"$rgd_selectors_json")"
done
readonly rgd_selectors_json

# Approved remote manifests remain ordinary upstream resources only. A remote RGD or generated
# instance has no committed source path for this scanner's hashes and baseline, so reject it even
# when its URL and bytes match the reviewed remote-resource baseline.
if [ "${#REMOTE_RESOURCE_PATHS[@]}" -gt 0 ]; then
  for remote_index in "${!REMOTE_RESOURCE_PATHS[@]}"; do
    remote_documents="$(yq -o=json -I=0 '.' "${REMOTE_RESOURCE_PATHS[$remote_index]}" | jq -cs '.')"
    jq -e 'length > 0 and all(.[]; type == "object")' <<<"$remote_documents" >/dev/null ||
      fail "remote Kustomize resource does not contain only mapping documents: ${REMOTE_RESOURCE_URLS[$remote_index]}"
    remote_flux_kustomization_count="$(jq '
      def resources:
        if type == "object" and (.kind // "") == "List"
        then .items[]? | resources
        else .
        end;
      [
        .[]
        | resources
        | select(type == "object"
          and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
          and (.kind // "") == "Kustomization")
      ] | length
    ' <<<"$remote_documents")"
    [ "$remote_flux_kustomization_count" -eq 0 ] || fail \
      "remote Kustomize resource declares a Flux Kustomization whose rendered controls cannot be validated: ${REMOTE_RESOURCE_URLS[$remote_index]}"
    protected_remote_kinds="$(jq -r --argjson generated "$rgd_selectors_json" '
      def resources:
        if type == "object" and (.kind // "") == "List"
        then .items[]? | resources
        else .
        end;
      [
        .[] | resources | select(type == "object") as $document |
        if ($document.kind // "") == "ResourceGraphDefinition" then
          "ResourceGraphDefinition"
        elif any($generated[];
          .kind == ($document.kind // "")
          and ((.group + "/" + .version) == ($document.apiVersion // ""))) then
          $document.kind
        else empty end
      ] | unique | join(", ")
    ' <<<"$remote_documents")"
    [ -z "$protected_remote_kinds" ] || fail \
      "remote Kustomize resource declares an RGD definition or generated instance ($protected_remote_kinds): ${REMOTE_RESOURCE_URLS[$remote_index]}"
  done
fi

# Kustomize expands Kubernetes List items before applying transformers. A generated instance nested
# in a local List has no stable source path for the instance baseline, so reject it just as remote
# protected resources are rejected rather than letting the top-level List kind hide it.
while IFS= read -r candidate; do
  candidate_documents="$(yq -o=json -I=0 '.' "$candidate" | jq -cs '.')"
  protected_list_kinds="$(jq -r --argjson generated "$rgd_selectors_json" '
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    [
      .[]
      | select(type == "object" and (.kind // "") == "List")
      | .items[]?
      | resources
      | select(type == "object") as $document
      | if any($generated[];
          .kind == ($document.kind // "")
          and ((.group + "/" + .version) == ($document.apiVersion // "")))
        then $document.kind
        else empty
        end
    ] | unique | join(", ")
  ' <<<"$candidate_documents")"
  [ -z "$protected_list_kinds" ] || fail \
    "Kubernetes List contains a generated RGD instance that cannot be baselined safely ($protected_list_kinds): $candidate"
done < <(printf '%s\n' "${K8S_RESOURCE_PATHS[@]}")

PROTECTED_RESOURCE_PATHS=()
for relative_path in "${RGD_PATHS[@]}"; do
  PROTECTED_RESOURCE_PATHS+=("${SOURCE_ROOT}/${relative_path}")
done
while IFS= read -r candidate; do
  candidate_documents="$(yq -o=json -I=0 '.' "$candidate" | jq -cs '.')"
  generated_document_count="$(jq --argjson generated "$rgd_selectors_json" '
    [
      .[]
      | select(type == "object") as $document
      | select(any($generated[];
          .kind == ($document.kind // "")
          and ((.group + "/" + .version) == ($document.apiVersion // ""))))
    ] | length
  ' <<<"$candidate_documents")"
  [ "$generated_document_count" -eq 0 ] || PROTECTED_RESOURCE_PATHS+=("$candidate")
done < <(printf '%s\n' "${K8S_RESOURCE_PATHS[@]}")
readonly PROTECTED_RESOURCE_PATHS

normalize_resource_path() {
  local resource_path="$1"
  if [ -d "$resource_path" ]; then
    (cd "$resource_path" && pwd)
  else
    printf '%s/%s\n' "$(cd "$(dirname "$resource_path")" && pwd)" "$(basename "$resource_path")"
  fi
}

flux_selector_may_match() {
  local selector_json="$1"
  jq -e '
    def selector_matches($value; $pattern):
      ($pattern == "")
      or (try ($value | test("^(" + $pattern + ")$")) catch false);
    type == "object"
    and selector_matches("Kustomization"; (.kind // ""))
    and selector_matches("kustomize.toolkit.fluxcd.io"; (.group // ""))
  ' <<<"$selector_json" >/dev/null
}

protected_flux_patch_targets() {
  local patch_entries_json="$1" targeted_flux flux_patch_entry flux_patch_body
  local flux_patch_documents protected_flux_patch_kinds
  targeted_flux="$(jq -r --argjson generated "$rgd_selectors_json" '
    def selector_matches($value; $pattern):
      ($pattern == "")
      or (try ($value | test("^(" + $pattern + ")$")) catch false);
    [
      .[]?
      | select(type == "object" and (.target | type) == "object")
      | .target
    ]
    | .[] as $target
    | select(
        ($target.kind // "") == ""
        or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
        or any($generated[];
          selector_matches(.kind; ($target.kind // ""))
          and selector_matches(.group; ($target.group // ""))
          and selector_matches(.version; ($target.version // ""))))
    | $target.kind // "missing kind"
  ' <<<"$patch_entries_json")"
  if [ -n "$targeted_flux" ]; then
    printf '%s\n' "$targeted_flux"
    return 0
  fi

  while IFS= read -r flux_patch_entry; do
    flux_patch_body="$(jq -r '
      if type == "object" and (.patch | type) == "string" then .patch else "" end
    ' <<<"$flux_patch_entry")"
    if [ -z "$flux_patch_body" ]; then
      printf '%s\n' "invalid targetless patch"
      return 0
    fi
    flux_patch_documents="$(yq -o=json -I=0 '.' <<<"$flux_patch_body" 2>/dev/null | jq -cs '.')" || {
      printf '%s\n' "invalid targetless patch"
      return 0
    }
    protected_flux_patch_kinds="$(jq -r --argjson generated "$rgd_selectors_json" '
      def resources:
        if type == "object" and (.kind // "") == "List"
        then .items[]? | resources
        else .
        end;
      [
        .[] | resources | select(type == "object") as $document
        | if ($document.kind // "") == "ResourceGraphDefinition" then
            "ResourceGraphDefinition"
          elif any($generated[];
            .kind == ($document.kind // "")
            and ((.group + "/" + .version) == ($document.apiVersion // ""))) then
            $document.kind
          else empty
          end
      ] | unique | join(", ")
    ' <<<"$flux_patch_documents")"
    if [ -n "$protected_flux_patch_kinds" ]; then
      printf '%s\n' "$protected_flux_patch_kinds"
      return 0
    fi
  done < <(jq -c '.[]? | select(type == "object" and (.target // null) == null)' \
    <<<"$patch_entries_json")
}

protected_flux_patch_fields() {
  local patch_documents_json="$1" patch_document protected_target
  local operation path from value flux_patch_entries encrypted_patch substitution_override
  local protected_fields=()
  while IFS= read -r patch_document; do
    if [ "$(jq -r 'type' <<<"$patch_document")" = "array" ]; then
      while IFS= read -r operation; do
        path="$(jq -r '.path // ""' <<<"$operation")"
        from="$(jq -r '.from // ""' <<<"$operation")"
        if [ "$path" = "" ] || [ "$path" = "/" ] || [ "$path" = "/spec" ] ||
          [ "$from" = "/spec" ]; then
          protected_fields+=("spec")
          continue
        fi
        if [[ "$path" == /sops* || "$from" == /sops* ]]; then
          protected_fields+=("sops")
          continue
        fi
        if [[ "$path" == /spec/commonMetadata* || "$from" == /spec/commonMetadata* ]]; then
          protected_fields+=("spec.commonMetadata")
          continue
        fi
        if [[ "$path" == /spec/patches || "$path" == /spec/patches/- ||
          "$path" =~ ^/spec/patches/[0-9]+$ ]]; then
          value="$(jq -c '.value // null' <<<"$operation")"
          if [ "$value" = "null" ]; then
            protected_fields+=("spec.patches")
            continue
          fi
          if [ "$(jq -r 'type' <<<"$value")" = "array" ]; then
            flux_patch_entries="$value"
          else
            flux_patch_entries="[$value]"
          fi
          protected_target="$(protected_flux_patch_targets "$flux_patch_entries")"
          [ -z "$protected_target" ] || protected_fields+=("spec.patches ($protected_target)")
        elif [[ "$path" == /spec/patches/* || "$from" == /spec/patches* ]]; then
          protected_fields+=("spec.patches")
        fi
      done < <(jq -c '.[]? | select(type == "object")' <<<"$patch_document")
      continue
    fi

    if [ "$(jq -r 'type' <<<"$patch_document")" != "object" ]; then
      protected_fields+=("invalid patch")
      continue
    fi
    encrypted_patch="$(jq '
      (.sops != null)
      or ([.. | strings | select(startswith("ENC["))] | length > 0)
    ' <<<"$patch_document")"
    if [ "$encrypted_patch" = "true" ]; then
      protected_fields+=("sops")
    fi
    if jq -e 'has("spec") and (.spec | type) != "object"' <<<"$patch_document" >/dev/null; then
      protected_fields+=("spec")
      continue
    fi
    substitution_override="$(jq -r '
      .spec.commonMetadata.annotations["kustomize.toolkit.fluxcd.io/substitute"] // "disabled"
    ' <<<"$patch_document")"
    [ "$substitution_override" = "disabled" ] || protected_fields+=("spec.commonMetadata")
    flux_patch_entries="$(jq -c '.spec.patches // []' <<<"$patch_document")"
    if [ "$(jq -r 'type' <<<"$flux_patch_entries")" != "array" ]; then
      protected_fields+=("spec.patches")
      continue
    fi
    protected_target="$(protected_flux_patch_targets "$flux_patch_entries")"
    [ -z "$protected_target" ] || protected_fields+=("spec.patches ($protected_target)")
  done < <(jq -c '.[]' <<<"$patch_documents_json")

  if [ "${#protected_fields[@]}" -gt 0 ]; then
    printf '%s\n' "${protected_fields[@]}" | LC_ALL=C sort -u | paste -sd ', ' -
  fi
}

kustomization_consumes_protected() {
  local kustomization_file="$1" ancestry="${2:-}"
  local resource_path resource_file protected_path child_kustomization candidate_name

  if printf '%s\n' "$ancestry" | grep -Fxq "$kustomization_file"; then
    fail "Kustomization resource graph contains a cycle: $kustomization_file"
  fi
  ancestry="${ancestry}${ancestry:+$'\n'}${kustomization_file}"

  while IFS= read -r resource_path; do
    [ -n "$resource_path" ] || continue
    resource_file="${kustomization_file%/*}/${resource_path}"
    [ -e "$resource_file" ] || continue
    resource_file="$(normalize_resource_path "$resource_file")"
    if [ -f "$resource_file" ]; then
      for protected_path in "${PROTECTED_RESOURCE_PATHS[@]}"; do
        [ "$resource_file" = "$protected_path" ] && return 0
      done
      continue
    fi

    child_kustomization=""
    for candidate_name in kustomization.yaml kustomization.yml Kustomization; do
      if [ -f "${resource_file}/${candidate_name}" ]; then
        child_kustomization="${resource_file}/${candidate_name}"
        break
      fi
    done
    # Kustomize itself rejects a resource directory without a build file. It cannot contribute a
    # protected object, and ordinary manifest validation owns that malformed-build diagnostic.
    [ -n "$child_kustomization" ] || continue
    kustomization_consumes_protected "$child_kustomization" "$ancestry" && return 0
  done < <(yq -o=json -I=0 '(.resources[]?, .components[]?, .bases[]?)' "$kustomization_file" |
    jq -r 'select(type == "string")')
  return 1
}

PROTECTED_KUSTOMIZATION_PATHS=()
kustomization_has_protected_context() {
  local candidate="$1" protected_kustomization
  [ "${#PROTECTED_KUSTOMIZATION_PATHS[@]}" -gt 0 ] || return 1
  for protected_kustomization in "${PROTECTED_KUSTOMIZATION_PATHS[@]}"; do
    [ "$candidate" = "$protected_kustomization" ] && return 0
  done
  return 1
}

if [ "${#KUSTOMIZATION_PATHS[@]}" -gt 0 ]; then
  for kustomization_file in "${KUSTOMIZATION_PATHS[@]}"; do
    if kustomization_consumes_protected "$kustomization_file"; then
      PROTECTED_KUSTOMIZATION_PATHS+=("$kustomization_file")
    fi
  done
fi

# Components apply their transformers to the including parent's accumulated resources, even when
# the component declares no resources itself. Propagate protected context from every protected
# includer through arbitrarily nested component chains.
protected_context_changed=true
while "$protected_context_changed" && [ "${#PROTECTED_KUSTOMIZATION_PATHS[@]}" -gt 0 ]; do
  protected_context_changed=false
  for protected_kustomization in "${PROTECTED_KUSTOMIZATION_PATHS[@]}"; do
    while IFS= read -r component_path; do
      [ -n "$component_path" ] || continue
      component_resource="${protected_kustomization%/*}/${component_path}"
      [ -e "$component_resource" ] || continue
      component_resource="$(normalize_resource_path "$component_resource")"
      component_kustomization=""
      if [ -f "$component_resource" ]; then
        component_kustomization="$component_resource"
      else
        for candidate_name in kustomization.yaml kustomization.yml Kustomization; do
          if [ -f "${component_resource}/${candidate_name}" ]; then
            component_kustomization="${component_resource}/${candidate_name}"
            break
          fi
        done
      fi
      [ -n "$component_kustomization" ] || continue
      if ! kustomization_has_protected_context "$component_kustomization"; then
        PROTECTED_KUSTOMIZATION_PATHS+=("$component_kustomization")
        protected_context_changed=true
      fi
    done < <(yq -o=json -I=0 '.components[]?' "$protected_kustomization" |
      jq -r 'select(type == "string")')
  done
done
readonly PROTECTED_KUSTOMIZATION_PATHS

# Kustomizations may consume a reviewed graph, but no overlay may patch, transform, or replace an RGD
# after the raw definition has been extracted. Enforce that across the complete Kubernetes tree: a
# mutation in a shared base is as invisible to this scan as one in a provider. Graph variants belong
# in a separately scanned definition rather than an invisible per-consumer mutation.
while IFS= read -r kustomization_file; do
  consumes_protected_resources=false
  if kustomization_has_protected_context "$kustomization_file"; then
    consumes_protected_resources=true
  fi
  targeted_builtin_transformers="$(yq -o=json -I=0 '.' "$kustomization_file" | jq -r '
    . as $document
    | [
        ["images", "namespace", "commonLabels", "labels", "namePrefix", "nameSuffix", "replicas"][] as $field
        | select($document | has($field))
        | $field
      ]
    | join(", ")
  ')"
  if "$consumes_protected_resources" && [ -n "$targeted_builtin_transformers" ]; then
    fail "Kustomization uses a built-in transformer that can mutate protected resources ($targeted_builtin_transformers): $kustomization_file"
  fi

  substitution_override_present="$(yq -o=json -I=0 '.' "$kustomization_file" | jq -r '
    (.commonAnnotations | type) == "object"
    and (.commonAnnotations | has("kustomize.toolkit.fluxcd.io/substitute"))
  ')"
  if "$consumes_protected_resources" && [ "$substitution_override_present" = "true" ]; then
    substitution_override="$(yq -r \
      '.commonAnnotations."kustomize.toolkit.fluxcd.io/substitute"' "$kustomization_file")"
    [ "$substitution_override" = "disabled" ] ||
      fail "Kustomization must not override the Flux substitution opt-out: $kustomization_file"
  fi

  # Ordinary Kustomize patches can rewrite the Flux Kustomization CR after its raw source document
  # has been checked below. Permit operational fields such as timeout and path, but reject the
  # controls that can mutate or substitute the sealed RGD graph during Flux reconciliation.
  while IFS= read -r flux_overlay_patch; do
    flux_overlay_target="$(jq -c '
      if type == "object" and (.target | type) == "object" then .target else null end
    ' <<<"$flux_overlay_patch")"
    flux_overlay_path="$(jq -r '
      if type == "object" and (.path | type) == "string" then .path else "" end
    ' <<<"$flux_overlay_patch")"
    flux_overlay_body="$(jq -r '
      if type == "object" and (.patch | type) == "string" then .patch else "" end
    ' <<<"$flux_overlay_patch")"
    if [ -n "$flux_overlay_path" ]; then
      flux_overlay_file="${kustomization_file%/*}/${flux_overlay_path}"
      [ -r "$flux_overlay_file" ] || fail "Kustomization patch is not readable: $flux_overlay_file"
      flux_overlay_documents="$(yq -o=json -I=0 '.' "$flux_overlay_file" | jq -cs '.')"
    elif [ -n "$flux_overlay_body" ]; then
      flux_overlay_documents="$(yq -o=json -I=0 '.' <<<"$flux_overlay_body" 2>/dev/null | jq -cs '.')" ||
        fail "Kustomization inline patch is not valid YAML: $kustomization_file"
    else
      continue
    fi
    jq -e 'length > 0' <<<"$flux_overlay_documents" >/dev/null ||
      fail "Kustomization patch is empty: $kustomization_file"

    if [ "$flux_overlay_target" != "null" ]; then
      if flux_selector_may_match "$flux_overlay_target"; then
        protected_flux_documents="$flux_overlay_documents"
      else
        protected_flux_documents='[]'
      fi
    else
      protected_flux_documents="$(jq '[
        .[]
        | select(type == "object"
          and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
          and (.kind // "") == "Kustomization")
      ]' <<<"$flux_overlay_documents")"
    fi
    protected_flux_fields="$(protected_flux_patch_fields "$protected_flux_documents")"
    [ -z "$protected_flux_fields" ] || fail \
      "Kustomization patch mutates protected Flux controls ($protected_flux_fields): $kustomization_file"
  done < <(yq -o=json -I=0 '(.patches[]?, .patchesJson6902[]?)' "$kustomization_file" | jq -c '.')

  while IFS= read -r configuration_entry; do
    [ "$(jq -r 'type' <<<"$configuration_entry")" = "string" ] ||
      fail "Kustomization configuration entry is not a path: $kustomization_file"
    configuration_path="$(jq -r '.' <<<"$configuration_entry")"
    configuration_file="${kustomization_file%/*}/${configuration_path}"
    [ -r "$configuration_file" ] ||
      fail "Kustomization configuration is not readable: $configuration_file"
    configuration_documents="$(yq -o=json -I=0 '.' "$configuration_file" | jq -cs '.')"
    jq -e 'length > 0 and all(.[]; type == "object")' <<<"$configuration_documents" >/dev/null ||
      fail "Kustomization configuration does not contain only mapping documents: $configuration_file"
    targeted_configuration="$(jq -r --argjson generated "$rgd_selectors_json" '
      def selector_matches($value; $pattern):
        ($pattern == "")
        or (try ($value | test("^(" + $pattern + ")$")) catch false);
      [
        .[]
        | (
            .namePrefix[]?,
            .nameSuffix[]?,
            .namespace[]?,
            .commonLabels[]?,
            .labels[]?,
            .templateLabels[]?,
            .commonAnnotations[]?,
            .varReference[]?,
            .images[]?,
            .replicas[]?,
            .nameReference[]?.fieldSpecs[]?
          )
      ]
      | .[] as $target
      | select(
          ($target | type) != "object"
          or ($target.kind // "") == ""
          or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
          or any($generated[];
            selector_matches(.kind; ($target.kind // ""))
            and selector_matches(.group; ($target.group // ""))
            and selector_matches(.version; ($target.version // ""))))
      | if ($target | type) == "object" then $target.kind // "missing kind"
        else "invalid selector" end
    ' <<<"$configuration_documents")"
    [ -z "$targeted_configuration" ] || fail \
      "Kustomization configuration targets or ambiguously selects a ResourceGraphDefinition/generated instance ($targeted_configuration): $configuration_file"
  done < <(yq -o=json -I=0 '.configurations[]?' "$kustomization_file" | jq -c '.')

  targeted_rgd="$(yq -o=json -I=0 '.' "$kustomization_file" |
    jq -r --argjson generated "$rgd_selectors_json" '
    def selector_matches($value; $pattern):
      ($pattern == "")
      or (try ($value | test("^(" + $pattern + ")$")) catch false);
    [
      (.patches[]? |
        select(type == "object" and .target != null) |
        .target),
      (.patchesJson6902[]? |
        select(type == "object" and .target != null) |
        .target),
      (.replacements[]? |
        select(type == "object") | .targets[]? |
        select(type == "object" and .select != null) |
        .select)
    ]
    | .[] as $target
    | select(
        ($target | type) != "object"
        or ($target.kind // "") == ""
        or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
        or any($generated[];
          selector_matches(.kind; ($target.kind // ""))
          and selector_matches(.group; ($target.group // ""))
          and selector_matches(.version; ($target.version // ""))))
    | if ($target | type) == "object" then $target.kind // "missing kind"
      else "invalid selector" end
  ')"
  [ -z "$targeted_rgd" ] || fail \
    "Kustomization targets or ambiguously selects a ResourceGraphDefinition/generated instance ($targeted_rgd): $kustomization_file"

  while IFS= read -r replacement_entry; do
    replacement_path="$(jq -r 'if type == "object" and (.path | type) == "string" then .path else "" end' \
      <<<"$replacement_entry")"
    [ -n "$replacement_path" ] || continue
    replacement_file="${kustomization_file%/*}/${replacement_path}"
    [ -r "$replacement_file" ] || fail "Kustomization replacement is not readable: $replacement_file"
    replacement_documents="$(yq -o=json -I=0 '.' "$replacement_file" | jq -cs '.')"
    jq -e 'length > 0 and all(.[]; type == "object")' <<<"$replacement_documents" >/dev/null ||
      fail "Kustomization replacement does not contain only mapping documents: $replacement_file"
    targeted_replacement="$(jq -r --argjson generated "$rgd_selectors_json" '
      def selector_matches($value; $pattern):
        ($pattern == "")
        or (try ($value | test("^(" + $pattern + ")$")) catch false);
      [ .[] | .targets[]?.select? // empty ]
      | .[] as $target
      | select(
          ($target | type) != "object"
          or ($target.kind // "") == ""
          or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
          or any($generated[];
            selector_matches(.kind; ($target.kind // ""))
            and selector_matches(.group; ($target.group // ""))
            and selector_matches(.version; ($target.version // ""))))
      | if ($target | type) == "object" then $target.kind // "missing kind"
        else "invalid selector" end
    ' <<<"$replacement_documents")"
    [ -z "$targeted_replacement" ] || fail \
      "Kustomization replacement targets or ambiguously selects a ResourceGraphDefinition/generated instance ($targeted_replacement): $replacement_file"
  done < <(yq -o=json -I=0 '.replacements[]?' "$kustomization_file" | jq -c '.')

  while IFS= read -r transformer_entry; do
    [ "$(jq -r 'type' <<<"$transformer_entry")" = "string" ] ||
      fail "Kustomization transformer entry is not a path: $kustomization_file"
    transformer_path="$(jq -r '.' <<<"$transformer_entry")"
    [ -n "$transformer_path" ] || continue
    transformer_file="${kustomization_file%/*}/${transformer_path}"
    [ -r "$transformer_file" ] || fail "Kustomization transformer is not readable: $transformer_file"
    transformer_documents="$(yq -o=json -I=0 '.' "$transformer_file" | jq -cs '.')"
    jq -e 'length > 0 and all(.[]; type == "object")' <<<"$transformer_documents" >/dev/null ||
      fail "Kustomization transformer does not contain only mapping documents: $transformer_file"

    targeted_transformer="$(jq -r --argjson generated "$rgd_selectors_json" '
      def selector_matches($value; $pattern):
        ($pattern == "")
        or (try ($value | test("^(" + $pattern + ")$")) catch false);
      [
        .[] as $document |
        ([
          ($document.target? // empty),
          ($document.fieldSpecs[]?),
          ($document.replacements[]?.targets[]?.select? // empty)
        ] | if length == 0 then [{}] else . end)[]
      ]
      | .[] as $target
      | select(
          ($target | type) != "object"
          or ($target.kind // "") == ""
          or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
          or any($generated[];
            selector_matches(.kind; ($target.kind // ""))
            and selector_matches(.group; ($target.group // ""))
            and selector_matches(.version; ($target.version // ""))))
      | if ($target | type) == "object" then $target.kind // "missing kind"
        else "invalid selector" end
    ' <<<"$transformer_documents")"
    [ -z "$targeted_transformer" ] || fail \
      "Kustomization transformer targets or ambiguously selects a ResourceGraphDefinition/generated instance ($targeted_transformer): $transformer_file"
  done < <(yq -o=json -I=0 '.transformers[]?' "$kustomization_file" | jq -c '.')

  while IFS= read -r patch_entry; do
    patch_path="$(jq -r 'if type == "object" then .path // "" else "" end' <<<"$patch_entry")"
    inline_patch="$(jq -r 'if type == "object" then .patch // "" else "" end' <<<"$patch_entry")"
    if [ -n "$patch_path" ]; then
      patch_file="${kustomization_file%/*}/${patch_path}"
      [ -r "$patch_file" ] || fail "Kustomization patch is not readable: $patch_file"
      if yq -o=json -I=0 '.' "$patch_file" |
        jq -e -s --argjson generated "$rgd_selectors_json" '
          any(.[];
            type == "object"
            and ((.kind // "") == "ResourceGraphDefinition"
              or (. as $document | any($generated[];
                .kind == ($document.kind // "")
                and ((.group + "/" + .version) == ($document.apiVersion // ""))))))
        ' >/dev/null; then
        fail "Kustomization patch declares an RGD definition or generated instance: $patch_file"
      fi
    fi
    if [ -n "$inline_patch" ]; then
      if yq -o=json -I=0 '.' <<<"$inline_patch" |
        jq -e -s --argjson generated "$rgd_selectors_json" '
          any(.[];
            type == "object"
            and ((.kind // "") == "ResourceGraphDefinition"
              or (. as $document | any($generated[];
                .kind == ($document.kind // "")
                and ((.group + "/" + .version) == ($document.apiVersion // ""))))))
        ' >/dev/null; then
        fail "Kustomization inline patch declares an RGD definition or generated instance: $kustomization_file"
      fi
    fi
  done < <(yq -o=json -I=0 '.patches[]?' "$kustomization_file" | jq -c '.')

  while IFS= read -r legacy_patch_entry; do
    [ "$(jq -r 'type' <<<"$legacy_patch_entry")" = "string" ] ||
      fail "Kustomization legacy patch entry is not a path or inline YAML: $kustomization_file"
    legacy_patch="$(jq -r '.' <<<"$legacy_patch_entry")"
    [ -n "$legacy_patch" ] || continue
    patch_file="${kustomization_file%/*}/${legacy_patch}"
    if [ -r "$patch_file" ]; then
      legacy_patch_json="$(yq -o=json -I=0 '.' "$patch_file")"
      legacy_patch_failure="Kustomization patch declares an RGD definition or generated instance: $patch_file"
    else
      legacy_patch_json="$(yq -o=json -I=0 '.' <<<"$legacy_patch" 2>/dev/null)" ||
        fail "Kustomization patch is not readable and is not valid inline YAML: $patch_file"
      jq -e -s 'length > 0 and all(.[]; type == "object")' <<<"$legacy_patch_json" >/dev/null ||
        fail "Kustomization patch is not readable and is not inline YAML: $patch_file"
      legacy_patch_failure="Kustomization inline legacy patch declares an RGD definition or generated instance: $kustomization_file"
    fi
    protected_legacy_kinds="$(jq -r -s --argjson generated "$rgd_selectors_json" '
      [
        .[] | select(type == "object") as $document |
        if ($document.kind // "") == "ResourceGraphDefinition" then
          "ResourceGraphDefinition"
        elif any($generated[];
          .kind == ($document.kind // "")
          and ((.group + "/" + .version) == ($document.apiVersion // ""))) then
          $document.kind
        else empty end
      ] | unique | join(", ")
    ' <<<"$legacy_patch_json")"
    if [ -n "$protected_legacy_kinds" ]; then
      fail "$legacy_patch_failure ($protected_legacy_kinds)"
    fi
  done < <(yq -o=json -I=0 '.patchesStrategicMerge[]?' "$kustomization_file" | jq -c '.')
done < <(
  if [ "${#KUSTOMIZATION_PATHS[@]}" -gt 0 ]; then
    printf '%s\n' "${KUSTOMIZATION_PATHS[@]}"
  fi
)

# Flux applies spec.patches after the ordinary Kustomize build. Inspect those CRs separately from
# build files so they cannot mutate a reviewed RGD or generated instance after its evidence is sealed.
while IFS= read -r candidate; do
  candidate_documents="$(yq -o=json -I=0 '.' "$candidate" | jq -cs '.')"
  encrypted_flux_count="$(jq '
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    [
      .[]
      | resources
      | select(type == "object"
        and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
        and (.kind // "") == "Kustomization")
      | select(
          (.sops != null)
          or ([.. | strings | select(startswith("ENC["))] | length > 0))
    ] | length
  ' <<<"$candidate_documents")"
  [ "$encrypted_flux_count" -eq 0 ] || fail \
    "SOPS-encrypted Flux Kustomizations cannot be validated from ciphertext: $candidate"

  flux_substitution_override_count="$(jq '
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    [
    .[]
    | resources
    | select(type == "object"
      and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
      and (.kind // "") == "Kustomization")
    | .spec.commonMetadata.annotations
    | select(type == "object" and has("kustomize.toolkit.fluxcd.io/substitute"))
    | .["kustomize.toolkit.fluxcd.io/substitute"]
    | select(. != "disabled")
  ] | length' <<<"$candidate_documents")"
  [ "$flux_substitution_override_count" -eq 0 ] || fail \
    "Flux Kustomization must not override the substitution opt-out: $candidate"

  targeted_flux="$(jq -r --argjson generated "$rgd_selectors_json" '
    def selector_matches($value; $pattern):
      ($pattern == "")
      or (try ($value | test("^(" + $pattern + ")$")) catch false);
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    [
      .[] |
      resources |
      select(type == "object"
        and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
        and (.kind // "") == "Kustomization") |
      .spec.patches[]? |
      select(type == "object" and .target != null) |
      .target
    ]
    | .[] as $target
    | select(
        ($target | type) != "object"
        or ($target.kind // "") == ""
        or selector_matches("ResourceGraphDefinition"; ($target.kind // ""))
        or any($generated[];
          selector_matches(.kind; ($target.kind // ""))
          and selector_matches(.group; ($target.group // ""))
          and selector_matches(.version; ($target.version // ""))))
    | if ($target | type) == "object" then $target.kind // "missing kind"
      else "invalid selector" end
  ' <<<"$candidate_documents")"
  [ -z "$targeted_flux" ] || fail \
    "Flux Kustomization targets or ambiguously selects a ResourceGraphDefinition/generated instance ($targeted_flux): $candidate"

  while IFS= read -r flux_patch_entry; do
    flux_patch_body="$(jq -r '
      if type == "object" and (.patch | type) == "string" then .patch else "" end
    ' <<<"$flux_patch_entry")"
    [ -n "$flux_patch_body" ] || fail \
      "Flux Kustomization targetless patch does not contain inline YAML: $candidate"
    flux_patch_documents="$(yq -o=json -I=0 '.' <<<"$flux_patch_body" 2>/dev/null | jq -cs '.')" ||
      fail "Flux Kustomization targetless patch is not valid YAML: $candidate"
    jq -e 'length > 0 and all(.[]; type == "object")' <<<"$flux_patch_documents" >/dev/null ||
      fail "Flux Kustomization targetless patch does not contain only mapping documents: $candidate"
    protected_flux_patch_kinds="$(jq -r --argjson generated "$rgd_selectors_json" '
      def resources:
        if type == "object" and (.kind // "") == "List"
        then .items[]? | resources
        else .
        end;
      [
        .[] | resources | select(type == "object") as $document
        | if ($document.kind // "") == "ResourceGraphDefinition" then
            "ResourceGraphDefinition"
          elif any($generated[];
            .kind == ($document.kind // "")
            and ((.group + "/" + .version) == ($document.apiVersion // ""))) then
            $document.kind
          else empty
          end
      ] | unique | join(", ")
    ' <<<"$flux_patch_documents")"
    [ -z "$protected_flux_patch_kinds" ] || fail \
      "Flux Kustomization targetless patch declares an RGD definition or generated instance ($protected_flux_patch_kinds): $candidate"
  done < <(jq -c '
    def resources:
      if type == "object" and (.kind // "") == "List"
      then .items[]? | resources
      else .
      end;
    .[]
    | resources
    | select(type == "object"
      and ((.apiVersion // "") | startswith("kustomize.toolkit.fluxcd.io/"))
      and (.kind // "") == "Kustomization")
    | .spec.patches[]?
    | select(type == "object" and (.target // null) == null)
  ' <<<"$candidate_documents")
done < <(printf '%s\n' "${K8S_RESOURCE_PATHS[@]}")

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
    instance_name="$(jq -r '
      if (.spec | type) == "object" then .spec.name // "" else "" end
    ' <<<"$instance_json")"
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
    encrypted_instance="$(jq '
      (.sops != null)
      or ([.spec | .. | strings | select(startswith("ENC["))] | length > 0)
    ' <<<"$instance_json")"
    [ "$encrypted_instance" = "false" ] || fail \
      "SOPS-encrypted generated instances cannot be validated from ciphertext: $candidate"
    substitution_path="$(jq -r '
      .spec as $spec
      | if (($spec | type) == "string" and ($spec | contains("${"))) then
          "spec"
        else
          [
            $spec
            | paths(scalars) as $path
            | select(
                ($spec | getpath($path) | type) == "string"
                and ($spec | getpath($path) | contains("${")))
            | "spec." + ($path | map(tostring) | join("."))
          ][0] // ""
        end
    ' <<<"$instance_json")"
    [ -z "$substitution_path" ] || fail \
      "generated instance spec must not contain Flux substitution expressions ($substitution_path): $candidate"
    [ "$instance_name" != "kube-system" ] || fail \
      "KSV-0037: committed $instance_kind instance $candidate would generate resources in kube-system"
    instance_image="$(jq -r '
      if (.spec | type) == "object" then .spec.image // "" else "" end
    ' <<<"$instance_json")"
    [ -z "$instance_image" ] || validate_image_registry "$instance_image"
    printf '1\t%s\tINSTANCE-CONTENT\tSHA256\t%s\n' \
      "${candidate#"$SOURCE_ROOT"/}" "$(sha256_text "$instance_json")" >>"$WORK/content.tsv"
  done < <(
    yq -o=json -I=0 'select(tag == "!!map")' "$candidate" |
      jq -cS 'select(.kind != null)'
  )
done < <(printf '%s\n' "${K8S_RESOURCE_PATHS[@]}")

template_count=0
for relative_path in "${RGD_PATHS[@]}"; do
  source_file="${SOURCE_ROOT}/${relative_path}"
  [ -r "$source_file" ] || fail "RGD source is not readable: $source_file"

  count="$(yq -r '.spec.resources | length' "$source_file")"
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
  done < <(yq -r '.spec.resources[].id // ""' "$source_file")
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
  # Trivy ranges are one-based; yq's line operator is zero-based. Remove both that index offset and
  # the serializer-only leading blanks before comparing source coordinates.
  semantic_start_line=$((finding_start_line - leading_blank_lines - 1))
  semantic_end_line=$((finding_end_line - leading_blank_lines - 1))

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
