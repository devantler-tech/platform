#!/usr/bin/env bash
# Verify publications and prepare bounded source-only admission/upgrade candidates.
# Generation never claims that Flux has applied the proposed publication.
set -euo pipefail
BOOTSTRAP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/generate-publish-workflow-approved-revisions.sh
source "$BOOTSTRAP_DIR/generate-publish-workflow-approved-revisions.sh"
# shellcheck source=scripts/tenant-publication-binding.lib.sh
source "$BOOTSTRAP_DIR/tenant-publication-binding.lib.sh"

canonical_bootstrap_admission() (
  set -euo pipefail
  trap - EXIT
  local admission_root="$REPO_ROOT" tag='' source_sha='' digest='' seen=':' option output receipt graph_hash previous record state date previous_exists=0 current_record
  while [ "$#" -gt 0 ]; do
    option="$1"
    case "$seen" in *":$option:"*) return 2 ;; esac
    seen="$seen$option:"
    [ "$#" -ge 2 ] || return 2
    case "$option" in
      --source-root) admission_root="$2" ;;
      --tag) tag="$2" ;;
      --source-sha) source_sha="$2" ;;
      --digest) digest="$2" ;;
      *) return 2 ;;
    esac
    shift 2
  done
  [[ "$tag" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] && is_sha "$source_sha" && is_digest "$digest" || return 2
  [ -n "${PUBLISH_KUBE_CONTEXT:-}" ] || return 2
  admission_root="$(cd "$admission_root" && pwd -P)" || return 2
  output="$admission_root/scripts/canonical-tenant-source-bindings.json"
  [ ! -L "$output" ] || return 1
  [ -z "${CANONICAL_TENANT_BINDINGS_FILE:-}" ] || [ "$CANONICAL_TENANT_BINDINGS_FILE" = "$output" ] || return 2
  [ ! -e "$output" ] || previous_exists=1
  previous="$(unset CANONICAL_TENANT_BINDINGS_FILE; canonical_tenant_bindings "$admission_root")" || return 1
  if [ "$previous_exists" = 1 ] && [ "$(printf '%s\n' "$previous" | jq '.bindings|length')" = 0 ]; then
    echo 'existing empty registry is not an admission; refusing to claim unchanged output' >&2
    return 1
  fi
  if [ "$(printf '%s\n' "$previous" | jq '.bindings|length')" -gt 0 ]; then
    [ "$(printf '%s\n' "$previous" | jq -r '.bindings[0].kind')" = published-bootstrap ] || {
      echo 'canonical source already observed; use ordinary regeneration' >&2; return 1;
    }
  fi
  receipt="$(business_site_manifest_evidence --consumer business-site --verify-only --tag "$tag" --source-sha "$source_sha" --digest "$digest")" || return 1
  graph_hash="$(yq -o=json -I=0 '.' "$admission_root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml" |
    jq -cSs 'select(length==1)|.[0].spec' | shasum -a 256 | awk '{print $1}')" || return 1
  date="${APPROVED_REVISIONS_OBSERVED_ON:-$(date -u +%Y-%m-%d)}"
  is_date "$date" || return 1
  canonical_bootstrap_work="$(mktemp -d)" || return 1
  trap 'rm -rf "$canonical_bootstrap_work"' EXIT
  printf '%s\n' "$receipt" | jq -es --arg graph "$graph_hash" --arg date "$date" '
    select(length==1) | .[0] |
    select(.schemaVersion==1 and .kind=="manifest-publication-evidence" and .consumer=="business-site" and .deploymentAuthorized==false) |
    {schemaVersion:1,bindings:[{
      consumer:.consumer,consumerRepository:"devantler-tech/business-site",workflow:"publish-app",kind:"published-bootstrap",
      sourcePath:"k8s/providers/hetzner/apps/business-site/tenant.yaml",constructorSpecSha256:$graph,
      tenantIdentity:{apiVersion:"kro.run/v1alpha1",kind:"Tenant",namespace:"flux-system",name:"business-site"},
      sourceIdentity:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",namespace:"business-site",name:"business-site",
        artifactURL:"oci://ghcr.io/devantler-tech/business-site/manifests",digest:.digest},
      publication:{gitTag:.gitTag,sourceSHA:.sourceSHA,publisherIdentity:.publisherIdentity,callerWorkflow:.callerWorkflow,event:.event,
        workflowRunId:.workflowRunId,workflowRunAttempt:.workflowRunAttempt,verifiedOn:$date}
    }]}' >"$canonical_bootstrap_work/candidate.json" || return 1
  record="$(CANONICAL_TENANT_BINDINGS_FILE="$canonical_bootstrap_work/candidate.json" canonical_tenant_bindings "$admission_root" | jq -ce '.bindings[0]')" || return 1
  # A fresh verification cannot replace a conflicting existing admission.
  if [ "$(printf '%s\n' "$previous" | jq '.bindings|length')" -gt 0 ]; then
    [ "$(printf '%s\n' "$previous" | jq -cS '.bindings[0]|del(.publication.verifiedOn)')" = "$(printf '%s\n' "$record" | jq -cS 'del(.publication.verifiedOn)')" ] || return 1
    printf '%s\n' "$previous" >"$canonical_bootstrap_work/candidate.json"
    record="$(printf '%s\n' "$previous" | jq -c '.bindings[0]')"
  fi
  canonical_observation_consistent "$record" "${APPROVED_IDENTITIES_FILE:-$admission_root/scripts/publish-workflow-approved-revisions.identities.tsv}" || return 1
  CANONICAL_TENANT_BINDINGS_FILE="$canonical_bootstrap_work/candidate.json" discover_consumers "$admission_root" >/dev/null || return 1
  kubectl --context "$PUBLISH_KUBE_CONTEXT" get ocirepositories.source.toolkit.fluxcd.io -A -o json >"$canonical_bootstrap_work/inventory.json" || return 1
  state="$(canonical_source_inventory_state "$record" "$canonical_bootstrap_work/inventory.json")" || return 1
  case "$state" in absent | pending) ;; *) echo 'canonical source already has an artifact; use observed regeneration' >&2; return 1 ;; esac
  # An existing identical admission is a revalidated no-op. Never replace a
  # concurrently changed/observed record, or move a temporary file into a
  # destination that became a directory.
  if [ "$previous_exists" = 1 ]; then
    current_record="$(CANONICAL_TENANT_BINDINGS_FILE="$output" canonical_tenant_bindings "$admission_root")" || return 1
    [ "$current_record" = "$previous" ] || return 1
    echo 'canonical source admission unchanged after fresh verification; application reconciliation remains disabled'
    return 0
  fi
  # Exclusive same-filesystem creation: link(1) never follows a destination
  # directory or overwrites any conflicting file/symlink appearing after the read.
  canonical_bootstrap_output="$(mktemp "${output}.XXXXXX")" || return 1
  trap 'rm -rf "$canonical_bootstrap_work"; rm -f "${canonical_bootstrap_output:-}"' EXIT
  cp "$canonical_bootstrap_work/candidate.json" "$canonical_bootstrap_output"
  chmod 0644 "$canonical_bootstrap_output"
  link "$canonical_bootstrap_output" "$output" || return 1
  rm "$canonical_bootstrap_output"
  echo 'canonical source admission candidate generated; review required; application reconciliation remains disabled'
)

canonical_upgrade_preparation() (
  set -euo pipefail
  trap - EXIT
  local upgrade_root="$REPO_ROOT" upgrade_output='' tag='' source_sha='' digest='' seen=':' option
  local original record receipt first_snapshot second_snapshot today uid_hash snapshot_hash prepared_root prior_node
  while [ "$#" -gt 0 ]; do
    option="$1"
    case "$seen" in *":$option:"*) return 2 ;; esac
    seen="$seen$option:"
    [ "$#" -ge 2 ] || return 2
    case "$option" in
      --source-root) upgrade_root="$2" ;;
      --output) upgrade_output="$2" ;;
      --tag) tag="$2" ;;
      --source-sha) source_sha="$2" ;;
      --digest) digest="$2" ;;
      *) return 2 ;;
    esac
    shift 2
  done
  [[ "$tag" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] && is_sha "$source_sha" && is_digest "$digest" || return 2
  [ -n "${PUBLISH_KUBE_CONTEXT:-}" ] && [[ "$upgrade_output" = /* ]] || return 2
  local output_parent output_name upgrade_tenant graph_path
  output_parent="$(dirname "$upgrade_output")"; output_name="$(basename "$upgrade_output")"
  [[ "$output_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || return 2
  output_parent="$(cd "$output_parent" && pwd -P)" || return 2
  upgrade_output="$output_parent/$output_name"
  [ ! -e "$upgrade_output" ] && [ ! -L "$upgrade_output" ] || return 1
  upgrade_root="$(cd "$upgrade_root" && pwd -P)" || return 2
  upgrade_tenant=k8s/providers/hetzner/apps/business-site/tenant.yaml
  graph_path=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
  [ -z "${CANONICAL_TENANT_BINDINGS_FILE:-}" ] || return 2
  original="$(canonical_tenant_bindings "$upgrade_root")" || return 1
  record="$(printf '%s\n' "$original" | jq -cer '.bindings|select(length==1)|.[0]|select(.kind!="published-upgrade")')" || return 1
  canonical_observation_consistent "$record" "${APPROVED_IDENTITIES_FILE:-$upgrade_root/scripts/publish-workflow-approved-revisions.identities.tsv}" || return 1
  prior_node="$(yq -o=json -I=0 '.' "$upgrade_root/$upgrade_tenant" | jq -ces 'select(length==1)|.[0]')" || return 1
  canonical_tenant_projection "$upgrade_root" "$prior_node" >/dev/null || return 1
  discover_consumers "$upgrade_root" >/dev/null || return 1
  prepared_root="$(mktemp -d "$output_parent/.canonical-upgrade.XXXXXX")" || return 1
  trap 'rm -rf "$prepared_root"' EXIT
  kubectl --context "$PUBLISH_KUBE_CONTEXT" get ocirepositories.source.toolkit.fluxcd.io -A -o json >"$prepared_root/first-inventory.json" || return 1
  first_snapshot="$(canonical_source_observation "$record" "$prepared_root/first-inventory.json")" || return 1
  receipt="$(business_site_manifest_evidence --consumer business-site --verify-only --tag "$tag" --source-sha "$source_sha" --digest "$digest")" || return 1
  today="${APPROVED_REVISIONS_OBSERVED_ON:-$(date -u +%Y-%m-%d)}"
  is_date "$today" || return 1
  uid_hash="$(printf '%s' "$(printf '%s\n' "$first_snapshot" | jq -r .metadata.uid)" | shasum -a 256 | awk '{print $1}')" || return 1
  snapshot_hash="$(printf '%s\n' "$first_snapshot" | shasum -a 256 | awk '{print $1}')" || return 1
  mkdir -p "$prepared_root/scripts" "$prepared_root/$(dirname "$upgrade_tenant")" "$prepared_root/$(dirname "$graph_path")"
  printf '%s\n' "$receipt" | jq -es --argjson old "$record" --arg tag "$tag" --arg source "$source_sha" \
    --arg digest "$digest" --arg date "$today" --arg uid "$uid_hash" --arg snapshot "$snapshot_hash" --argjson observed "$first_snapshot" '
    select(length==1)|.[0]|select(.schemaVersion==1 and .kind=="manifest-publication-evidence" and
      .consumer=="business-site" and .deploymentAuthorized==false and .gitTag==$tag and .sourceSHA==$source and .digest==$digest) |
    . as $evidence | {schemaVersion:1,bindings:[($old|.kind="published-upgrade"|
      .previousBinding=$old|.previousObservation={uidSha256:$uid,generation:$observed.metadata.generation,snapshotSha256:$snapshot,observedOn:$date}|
      .sourceIdentity.digest=$digest|.publication={gitTag:$tag,sourceSHA:$source,publisherIdentity:$evidence.publisherIdentity,
        callerWorkflow:$evidence.callerWorkflow,event:$evidence.event,workflowRunId:$evidence.workflowRunId,
        workflowRunAttempt:$evidence.workflowRunAttempt,verifiedOn:$date})]}' >"$prepared_root/scripts/canonical-tenant-source-bindings.json" || return 1
  cp "$upgrade_root/$graph_path" "$prepared_root/$graph_path"
  cp "$upgrade_root/scripts/publish-workflow-canonical-approvals.tsv" "$prepared_root/scripts/"
  cp "${APPROVED_IDENTITIES_FILE:-$upgrade_root/scripts/publish-workflow-approved-revisions.identities.tsv}" "$prepared_root/scripts/"
  UPGRADE_PREPARED_DIGEST="$digest" yq '.spec.source.digest=strenv(UPGRADE_PREPARED_DIGEST)' "$upgrade_root/$upgrade_tenant" >"$prepared_root/$upgrade_tenant" || return 1
  # The same strict reader and raw discovery used by CI must accept generated output.
  APPROVED_IDENTITIES_FILE="$prepared_root/scripts/publish-workflow-approved-revisions.identities.tsv" \
    CANONICAL_APPROVALS_FILE="$prepared_root/scripts/publish-workflow-canonical-approvals.tsv" \
    discover_consumers "$prepared_root" >/dev/null || return 1
  kubectl --context "$PUBLISH_KUBE_CONTEXT" get ocirepositories.source.toolkit.fluxcd.io -A -o json >"$prepared_root/second-inventory.json" || return 1
  second_snapshot="$(canonical_source_observation "$record" "$prepared_root/second-inventory.json")" || return 1
  [ "$first_snapshot" = "$second_snapshot" ] || return 1
  [ "$(canonical_tenant_bindings "$upgrade_root")" = "$original" ] || return 1
  [ "$(yq -o=json -I=0 '.' "$upgrade_root/$upgrade_tenant" | jq -ces 'select(length==1)|.[0]')" = "$prior_node" ] || return 1
  canonical_tenant_projection "$upgrade_root" "$prior_node" >/dev/null || return 1
  discover_consumers "$upgrade_root" >/dev/null || return 1
  # Reserve a new private output directory only after every input has been validated.
  # Exclusive links cannot overwrite a conflicting file or follow its final symlink.
  (umask 077; mkdir "$upgrade_output") || return 1
  mkdir -p "$upgrade_output/scripts" "$upgrade_output/$(dirname "$upgrade_tenant")" || return 1
  link "$prepared_root/scripts/canonical-tenant-source-bindings.json" "$upgrade_output/scripts/canonical-tenant-source-bindings.json" || return 1
  link "$prepared_root/$upgrade_tenant" "$upgrade_output/$upgrade_tenant" || return 1
  printf 'Canonical source upgrade prepared for review; existing observations and disabled application flags preserved.\n'
)

bootstrap_tenant_source() (
  if [ "${1:-}" = --prepare-canonical-upgrade ]; then
    shift
    canonical_upgrade_preparation "$@"
    return
  fi
  if [ "${1:-}" = --admit-canonical-source ]; then
    shift
    canonical_bootstrap_admission "$@"
    return
  fi
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
