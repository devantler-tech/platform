#!/usr/bin/env bash
# Verify one first publication without claiming that Flux has applied it.
# Approval bootstrap is scoped to Data Products; business-site has evidence-only mode.
set -euo pipefail
BOOTSTRAP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/generate-publish-workflow-approved-revisions.sh
source "$BOOTSTRAP_DIR/generate-publish-workflow-approved-revisions.sh"
# shellcheck source=scripts/tenant-publication-binding.lib.sh
source "$BOOTSTRAP_DIR/tenant-publication-binding.lib.sh"

bootstrap_tenant_source() (
  if [ "${1:-}" = --consumer ]; then
    business_site_manifest_evidence "$@" || {
      echo 'business manifest publication could not be verified; no approval created' >&2
      return 1
    }
    return 0
  fi
  local source_root="$REPO_ROOT" source_file='' tag='' output="$REPO_ROOT/scripts/publish-workflow-bootstrap-publications.json"
  while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || {
      echo 'expected --source-file, --tag, --source-root or --output and a value' >&2
      return 2
    }
    case "$1" in
    --source-file) source_file="$2" ;;
    --tag) tag="$2" ;;
    --source-root) source_root="$2" ;;
    --output) output="$2" ;;
    *)
      echo 'unknown bootstrap argument' >&2
      return 2
      ;;
    esac
    shift 2
  done
  [[ "$tag" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] || return 2
  [ -n "$source_file" ] && [ -n "${PUBLISH_KUBE_CONTEXT:-}" ] || return 2
  # Validate the path before even parsing the digest. Reject symlink escapes and
  # relative traversal rather than reading arbitrary files as a side effect.
  [[ "$source_file" =~ ^k8s/[a-zA-Z0-9_./-]+[.]ya?ml$ ]] || return 2
  case "/$source_file/" in *'/../'* | *'/./'* | *'//'*) return 2 ;; esac
  source_root="$(cd "$source_root" && pwd -P)" || return 2
  [ "$(realpath "$source_root/$source_file" 2>/dev/null)" = "$source_root/$source_file" ] || return 2
  [ ! -L "$output" ] || return 2
  # This is a FIRST-consumer path, never an alternative to ordinary observation.
  [ -r "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || return 1
  [ "$(head -n1 "$OUTPUT")" = "$HEADER" ] || return 1
  awk -F'\t' 'NR>1 {if(NF!=8 || seen[$1]++) exit 1; for(i=1;i<=8;i++) if($i=="") exit 1}' "$OUTPUT" || return 1
  if awk -F'\t' '$1=="data-product-controller" {found=1} END {exit !found}' "$OUTPUT"; then
    echo 'consumer already has applied evidence; use ordinary regeneration' >&2
    return 1
  fi
  local work candidate digest inventory count source_sha signer declared pin release_sha verification previous next
  work="$(mktemp -d)" || return 1
  trap 'rm -rf "$work"' EXIT
  # Function-local cleanup preserves the caller shell and removes verification receipts.
  candidate="$work/candidate.json"
  digest="$(yq eval -r '.spec.ref.digest // ""' "$source_root/$source_file" 2>/dev/null)" || {
    rm -rf "$work"
    return 1
  }
  # A complete provisional record lets the shared reader validate the exact source
  # identity and path BEFORE any cluster or registry call.
  jq -n --arg path "$source_file" --arg digest "$digest" --arg tag "$tag" '{
    schemaVersion:1,publications:[{
      kind:"published-bootstrap",consumer:"data-product-controller",workflow:"publish-app",
      sourcePath:$path,namespace:"data-product-controller",name:"khoist-documentation",
      artifactURL:"oci://ghcr.io/devantler-tech/data-product-controller/manifests",
      gitTag:$tag,digest:$digest,sourceSHA:("0"*40),signerSHA:("0"*40),
      mainPinSHA:("0"*40),candidateSHA:("0"*40),verifiedOn:"1970-01-01"
    }]}' >"$candidate"
  read_bound_publications "$source_root" "$candidate" >/dev/null || {
    rm -rf "$work"
    return 1
  }
  inventory="$(kubectl --context="$PUBLISH_KUBE_CONTEXT" get ocirepositories.source.toolkit.fluxcd.io --all-namespaces -o json)" || {
    rm -rf "$work"
    return 1
  }
  printf '%s\n' "$inventory" | jq -es 'length == 1 and (.[0] | type == "object" and (.items | type == "array") and ((.metadata.continue // "") == ""))' >/dev/null || {
    rm -rf "$work"
    return 1
  }
  count="$(printf '%s\n' "$inventory" | jq '[.items[] | select(.spec.url == "oci://ghcr.io/devantler-tech/data-product-controller/manifests")] | length')"
  case "$count" in
  0) ;;
  1)
    printf '%s\n' "$inventory" | jq -e --arg digest "$digest" '
        [.items[] | select(.spec.url == "oci://ghcr.io/devantler-tech/data-product-controller/manifests")][0] |
        .metadata.namespace == "data-product-controller" and .metadata.name == "khoist-documentation" and
        .spec.ref.digest == $digest and
        ((.status.artifact.revision // "") == "") and
        all(.status.conditions[]?; .type != "Ready" or .status != "True")
      ' >/dev/null || {
      rm -rf "$work"
      return 1
    }
    ;;
  *)
    rm -rf "$work"
    return 1
    ;;
  esac
  source_sha="$(tag_commit data-product-controller "$tag")" || {
    rm -rf "$work"
    return 1
  }
  tag_was_published data-product-controller "$tag" "$source_sha" || {
    rm -rf "$work"
    return 1
  }
  signer="$(signer_for_tag data-product-controller publish-app "$tag")" || {
    rm -rf "$work"
    return 1
  }
  is_sha "$signer" || {
    rm -rf "$work"
    return 1
  }
  declared="$(report_publisher_at_ref data-product-controller publish-app "$source_sha")" || {
    rm -rf "$work"
    return 1
  }
  [ "$declared" = "devantler-tech/actions/.github/workflows/publish-app.yaml@$signer" ] || {
    rm -rf "$work"
    return 1
  }
  verification="$work/verified.json"
  cosign verify \
    --certificate-identity "https://github.com/$declared" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-github-workflow-repository devantler-tech/data-product-controller \
    --certificate-github-workflow-ref "refs/tags/$tag" \
    --certificate-github-workflow-sha "$source_sha" \
    "ghcr.io/devantler-tech/data-product-controller/manifests@$digest" >"$verification" 2>"$work/verify.log" || {
    echo 'immutable publication signature verification failed' >&2
    rm -rf "$work"
    return 1
  }
  jq -es --arg digest "$digest" '
    length == 1 and (.[0] | type == "array" and length > 0 and
      all(.[]; .critical.image["docker-manifest-digest"] == $digest and
        .critical.identity["docker-reference"] == ("ghcr.io/devantler-tech/data-product-controller/manifests@" + $digest)))
  ' "$verification" >/dev/null || {
    rm -rf "$work"
    return 1
  }
  pin="$(default_pin_resolver data-product-controller publish-app)" || {
    rm -rf "$work"
    return 1
  }
  release_sha="$(default_release_resolver publish-app)" || {
    rm -rf "$work"
    return 1
  }
  if ! is_sha "$pin" || ! is_sha "$release_sha"; then
    rm -rf "$work"
    return 1
  fi
  [ "$(tag_commit data-product-controller "$tag")" = "$source_sha" ] || {
    rm -rf "$work"
    return 1
  }
  next="$(jq --arg source "$source_sha" --arg signer "$signer" --arg pin "$pin" --arg release "$release_sha" --arg date "$(date -u +%Y-%m-%d)" '
    .publications[0] |= (.sourceSHA=$source | .signerSHA=$signer | .mainPinSHA=$pin |
      .candidateSHA=$release | .verifiedOn=$date)
  ' "$candidate")"
  printf '%s\n' "$next" >"$candidate"
  read_bound_publications "$source_root" "$candidate" >/dev/null || {
    rm -rf "$work"
    return 1
  }
  if [ -e "$output" ]; then
    previous="$(read_bound_publications "$source_root" "$output")" || {
      rm -rf "$work"
      return 1
    }
    # Do not replace a different approval or regress an observed source to bootstrap.
    if ! diff -q <(printf '%s\n' "$previous" | jq -S 'del(.publications[0].verifiedOn)') \
      <(printf '%s\n' "$next" | jq -S 'del(.publications[0].verifiedOn)') >/dev/null; then
      echo 'a different publication binding already exists; no output changed' >&2
      rm -rf "$work"
      return 1
    fi
    rm -rf "$work"
    return 0
  fi
  local staged
  staged="$(mktemp "${output}.tmp.XXXXXX")" || {
    rm -rf "$work"
    return 1
  }
  printf '%s\n' "$next" >"$staged" || {
    rm -f "$staged"
    return 1
  }
  chmod 0644 "$staged"
  # POSIX link creates this exact destination atomically. Unlike ln, it cannot
  # interpret a concurrently created directory (or directory symlink) as a target.
  link "$staged" "$output" || {
    rm -f "$staged"
    return 1
  }
  rm -f "$staged"
  rm -rf "$work"
)

if [ "${BASH_SOURCE[0]:-}" = "$0" ]; then
  bootstrap_tenant_source "$@"
fi
