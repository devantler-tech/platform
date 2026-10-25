# shellcheck shell=bash
# Qualified canonical-only admission stays separate from legacy observations.
# shellcheck source=scripts/tenant-publication-binding.lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/tenant-publication-binding.lib.sh"

canonical_tenant_bindings() (
  set -euo pipefail
  trap - EXIT
  local binding_root="$1" file
  file="${CANONICAL_TENANT_BINDINGS_FILE:-$binding_root/scripts/canonical-tenant-source-bindings.json}"
  if [ ! -e "$file" ] && [ ! -L "$file" ] && [ -z "${CANONICAL_TENANT_BINDINGS_FILE:-}" ]; then
    printf '{"schemaVersion":1,"bindings":[]}\n'
    return 0
  fi
  [ -f "$file" ] && [ ! -L "$file" ] || return 1
  jq -ces '
    def bounded: type=="string" and length>0 and length<=512 and (test("[[:cntrl:]]")|not);
    def sha: type=="string" and test("^[0-9a-f]{40}$");
    def digest: type=="string" and test("^sha256:[0-9a-f]{64}$");
    def positive: type=="number" and .>0 and .<=999999999999999 and floor==.;
    select(length==1) | .[0] |
    select(type=="object" and keys==["bindings","schemaVersion"] and .schemaVersion==1) |
    select(.bindings|type=="array" and length<=1) |
    select(all(.bindings[];
      type=="object" and keys==["constructorSpecSha256","consumer","consumerRepository","kind","publication","sourceIdentity","sourcePath","tenantIdentity","workflow"] and
      (.constructorSpecSha256|type=="string" and test("^[0-9a-f]{64}$")) and
      .consumer=="business-site" and .consumerRepository=="devantler-tech/business-site" and
      (.kind=="published-bootstrap" or .kind=="published-binding") and .workflow=="publish-app" and
      .sourcePath=="k8s/providers/hetzner/apps/business-site/tenant.yaml" and
      (.tenantIdentity | type=="object" and keys==["apiVersion","kind","name","namespace"] and
        .apiVersion=="kro.run/v1alpha1" and .kind=="Tenant" and .name=="business-site" and .namespace=="flux-system") and
      (.sourceIdentity | type=="object" and keys==["apiVersion","artifactURL","digest","kind","name","namespace"] and .apiVersion=="source.toolkit.fluxcd.io/v1" and .kind=="OCIRepository" and
        .artifactURL=="oci://ghcr.io/devantler-tech/business-site/manifests" and
        .name=="business-site" and .namespace=="business-site" and (.digest|digest)) and
      (.publication | type=="object" and keys==["callerWorkflow","event","gitTag","publisherIdentity","sourceSHA","verifiedOn","workflowRunAttempt","workflowRunId"] and
        .callerWorkflow==".github/workflows/publish-runtime.yaml" and .event=="release" and
        (.gitTag|type=="string" and test("^v[0-9]+[.][0-9]+[.][0-9]+$")) and (.sourceSHA|sha) and
        .publisherIdentity=="devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0" and
        (.verifiedOn|type=="string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) and
        (.workflowRunId|positive) and (.workflowRunAttempt|positive))
    ))
  ' "$file"
)

# Bind the full reviewed graph, not just its one visible source constructor.
canonical_tenant_constructor() (
  set -euo pipefail
  trap - EXIT
  local constructor_file="$1" record="$2" spec expected actual
  # shellcheck source=scripts/tenant-source-constructors.lib.sh
  source "$(dirname "${BASH_SOURCE[0]}")/tenant-source-constructors.lib.sh"
  tenant_source_default_subject "$constructor_file" >/dev/null || return 1
  spec="$(yq -o=json -I=0 '.' "$constructor_file" | jq -cSs '
    [.[]|select(.apiVersion=="kro.run/v1alpha1" and .kind=="ResourceGraphDefinition" and .metadata.name=="tenant.kro.run")]
    | select(length==1) | .[0].spec')" || return 1
  expected="$(printf '%s\n' "$record" | jq -er .constructorSpecSha256)"
  actual="$(printf '%s\n' "$spec" | shasum -a 256)" || return 1
  [ "${actual%% *}" = "$expected" ]
)

# The record is hand-reviewed admission. A verification-only receipt cannot parse as it.
canonical_tenant_projection() (
  set -euo pipefail
  trap - EXIT
  local projection_root="$1" node="$2" records record path resolved approvals publisher commit bound_scope node_scope
  projection_root="$(cd "$projection_root" && pwd -P)" || return 1
  records="$(canonical_tenant_bindings "$projection_root")" || return 1
  record="$(printf '%s\n' "$records" | jq -cer --argjson node "$node" '
    [.bindings[] | select(.tenantIdentity.apiVersion==$node.apiVersion and
      .tenantIdentity.kind==$node.kind and .tenantIdentity.namespace==$node.metadata.namespace and
      .tenantIdentity.name==$node.metadata.name)] | select(length==1) | .[0]')" || {
    printf 'business-site: typed Tenant needs its exact reviewed canonical source admission\n' >&2
    return 1
  }
  canonical_observation_consistent "$record" "${APPROVED_IDENTITIES_FILE:-$projection_root/scripts/publish-workflow-approved-revisions.identities.tsv}" || return 1
  path="$(printf '%s\n' "$record" | jq -r .sourcePath)"
  resolved="$(realpath "$projection_root/$path" 2>/dev/null)" || return 1
  [ "$resolved" = "$projection_root/$path" ] && [ -f "$resolved" ] && [ ! -L "$resolved" ] || return 1
  bound_scope="$(yq -o=json -I=0 '.' "$resolved" | jq -cSes '
    select(length==1) | .[0] |
    {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace},spec}')" || return 1
  node_scope="$(printf '%s\n' "$node" | jq -cSe '
    {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace},spec}')" || return 1
  [ "$bound_scope" = "$node_scope" ] || return 1
  canonical_tenant_constructor "$projection_root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml" "$record" || return 1
  publisher="$(printf '%s\n' "$record" | jq -r .publication.publisherIdentity)"
  commit="${publisher##*@}"
  approvals="${CANONICAL_APPROVALS_FILE:-$projection_root/scripts/publish-workflow-canonical-approvals.tsv}"
  [ -f "$approvals" ] && [ ! -L "$approvals" ] || return 1
  [ "$(head -n1 "$approvals")" = $'consumer\tworkflow\trepository\tcommit' ] || return 1
  [ "$(awk -F'\t' -v pin="$commit" 'NR>1 && $1=="business-site" && $2=="publish-app" && $3=="devantler-tech/.github" && $4==pin && NF==4 {n++} END {print n+0}' "$approvals")" = 1 ] || return 1
  printf '%s\n' "$node" | jq -cer --argjson record "$record" --arg pin "$commit" '
    select(.apiVersion==$record.tenantIdentity.apiVersion and .kind==$record.tenantIdentity.kind and
      .metadata.name==$record.tenantIdentity.name and .metadata.namespace==$record.tenantIdentity.namespace and
      (.spec|keys)==["externalDns","name","sops","source","webApp"] and .spec.name==$record.consumer and
      .spec.webApp==false and .spec.externalDns==false and .spec.sops==false and
      (.spec.source|keys)==["digest","publisherRevision","reconcileApplication"] and
      .spec.source.digest==$record.sourceIdentity.digest and .spec.source.publisherRevision==$pin and
      .spec.source.reconcileApplication==false) |
    {apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",
      metadata:{namespace:$record.sourceIdentity.namespace,name:$record.sourceIdentity.name},
      spec:{url:$record.sourceIdentity.artifactURL,ref:{digest:$record.sourceIdentity.digest},
        verify:{provider:"cosign",matchOIDCIdentity:[{
          issuer:"^https://token\\.actions\\.githubusercontent\\.com$",
          subject:("^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-app\\.yaml@"+$pin+"$")
        }]}}}
  '
)

# Attribute the complete parser observation before emitting any projected source.
canonical_tenant_inventory_projection() (
  set -euo pipefail
  trap - EXIT
  local inventory_root="$1" inventory="$2" hits_file="$3" mode="${4:-raw}" records hits count node record graph_file graph_count projected observed_spec expected_hash
  [ "$mode" = raw ] || [ "$mode" = rendered ] || return 1
  records="$(canonical_tenant_bindings "$inventory_root")" || return 1
  hits="$(cat "$hits_file")" || return 1
  count="$(printf '%s\n' "$hits" | jq -er length)" || return 1
  if [ "$count" = 0 ]; then
    [ "$(printf '%s\n' "$records" | jq -r '.bindings|length')" = 0 ] || return 1
    return 0
  fi
  [ "$count" = 1 ] && [ "$(printf '%s\n' "$records" | jq -r '.bindings|length')" = 1 ] || {
    printf 'typed source needs complete admission; inventory is UNKNOWN: %s\n' "$hits" >&2
    return 1
  }
  record="$(printf '%s\n' "$records" | jq -c '.bindings[0]')"
  # The path is part of admission: another copy of an identical declaration
  # cannot borrow the binding or silently add a second source.
  if [ "$mode" = raw ]; then
    [ "$(printf '%s\n' "$hits" | jq -r '.[0].file')" = "$(printf '%s\n' "$record" | jq -r .sourcePath)" ] || return 1
  fi
  node="$(jq -cse --arg path "$inventory_root/$(printf '%s\n' "$record" | jq -r .sourcePath)" --arg mode "$mode" '
    [.[] | select($mode=="rendered" or .file==$path) | .document | .. | objects |
      select(.apiVersion=="kro.run/v1alpha1" and .kind=="Tenant")] | select(length==1) | .[0]' "$inventory")" || return 1
  # A cloned schema is ambiguous even when its CEL happens to be identical.
  graph_count="$(jq -sr '[.[] | .document | .. | objects | select(.kind=="ResourceGraphDefinition" and
    (.spec.schema.group // "kro.run")=="kro.run" and .spec.schema.apiVersion=="v1alpha1" and
    .spec.schema.kind=="Tenant")] | length' "$inventory")" || return 1
  [ "$graph_count" = 1 ] || return 1
  graph_file="$inventory_root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
  # Require the observed graph to be the complete exact admitted graph, not a
  # different same-GVK producer while the expected file is merely present.
  observed_spec="$(jq -cSes --arg file "$graph_file" --arg mode "$mode" '[.[] |
    select($mode=="rendered" or .file==$file) | .document | .. | objects |
    select(.apiVersion=="kro.run/v1alpha1" and .kind=="ResourceGraphDefinition" and .metadata.name=="tenant.kro.run")] |
    select(length==1) | .[0].spec' "$inventory")" || return 1
  expected_hash="$(printf '%s\n' "$record" | jq -r .constructorSpecSha256)"
  [ "$(printf '%s\n' "$observed_spec" | shasum -a 256 | awk '{print $1}')" = "$expected_hash" ] || return 1
  projected="$(canonical_tenant_projection "$inventory_root" "$node")" || return 1
  printf '%s\n' "$projected"
)

# Classify every source-producing schema and its complete-GVK instances together.
canonical_typed_source_hits() {
  jq -cs --arg prefix "$1/" '
    [.[] | .file as $file | .document | .. | objects | {file:$file,resource:.}] as $nodes |
    [$nodes[].resource | select(.kind=="ResourceGraphDefinition") |
      select(any(.. | objects; .kind=="OCIRepository" and has("spec"))) | .spec.schema |
      if (.kind|type=="string" and test("^[A-Za-z][A-Za-z0-9]*$")) and
         ((.group // "kro.run")|type=="string" and test("^[a-z0-9]+([.-][a-z0-9]+)*$")) and
         (.apiVersion|type=="string" and test("^[a-z][a-z0-9]*$"))
      then {kind,apiVersion:((.group // "kro.run")+"/"+.apiVersion)}
      else error("source-producing schema GVK is unknown") end] as $schemas |
    [$nodes[] | .resource as $node |
      select(any($schemas[]; .kind==$node.kind and .apiVersion==$node.apiVersion)) |
      {file:(.file|ltrimstr($prefix)),name:(.resource.metadata | if type=="object" then (.name // "?") else "?" end)}]
  ' "$2"
}

# Reverify the immutable publication and compare its complete evidence tuple.
canonical_publication_verify() (
  set -euo pipefail
  trap - EXIT
  local publication_record="$1" publication_receipt
  publication_receipt="$(business_site_manifest_evidence --consumer business-site --verify-only \
    --tag "$(printf '%s\n' "$publication_record" | jq -er .publication.gitTag)" \
    --source-sha "$(printf '%s\n' "$publication_record" | jq -er .publication.sourceSHA)" \
    --digest "$(printf '%s\n' "$publication_record" | jq -er .sourceIdentity.digest)")" || return 1
  printf '%s\n' "$publication_receipt" | jq -es --argjson binding "$publication_record" '
    length==1 and .[0]=={
      schemaVersion:1,kind:"manifest-publication-evidence",consumer:$binding.consumer,
      deploymentAuthorized:false,gitTag:$binding.publication.gitTag,sourceSHA:$binding.publication.sourceSHA,
      digest:$binding.sourceIdentity.digest,publisherIdentity:$binding.publication.publisherIdentity,
      callerWorkflow:$binding.publication.callerWorkflow,event:$binding.publication.event,
      workflowRunId:$binding.publication.workflowRunId,workflowRunAttempt:$binding.publication.workflowRunAttempt
    }' >/dev/null
)

# A complete cluster listing distinguishes first publication from real source
# verification. An existing observed binding can never regress to bootstrap.
canonical_source_inventory_state() (
  set -euo pipefail
  trap - EXIT
  local state_record="$1" state_inventory="$2"
  jq -cers --argjson binding "$state_record" '
    def text: type=="string" and length>0 and length<=512 and (test("[[:cntrl:]]")|not);
    def generation: type=="number" and .>0 and floor==.;
    select(length==1) | .[0] |
    select(type=="object" and
      ((.apiVersion=="v1" and .kind=="List") or
       (.apiVersion=="source.toolkit.fluxcd.io/v1" and .kind=="OCIRepositoryList")) and
      (.metadata|type=="object") and (.items|type=="array") and
      ((.metadata|has("continue")|not) or .metadata.continue=="") and
      all(.items[]; type=="object" and .apiVersion=="source.toolkit.fluxcd.io/v1" and .kind=="OCIRepository" and
        (.metadata|type=="object") and (.metadata.name|text) and (.metadata.namespace|text) and
        (.metadata.generation|generation) and (.spec|type=="object") and (.spec.url|text) and
        ((has("status")|not) or
          (.status|type=="object") and
          ((.status|has("conditions")|not) or
            (.status.conditions|type=="array") and all(.status.conditions[];
              type=="object" and (.type|text) and (.status=="True" or .status=="False" or .status=="Unknown") and
              ((has("observedGeneration")|not) or (.observedGeneration|generation))))))) |
    [.items[] | select(.spec.url==$binding.sourceIdentity.artifactURL or
      (.metadata.namespace==$binding.sourceIdentity.namespace and .metadata.name==$binding.sourceIdentity.name))] |
    if length==0 then "absent"
    elif length!=1 then error("ambiguous canonical source")
    else .[0] | . as $source |
      select(.apiVersion==$binding.sourceIdentity.apiVersion and .kind==$binding.sourceIdentity.kind and
        .metadata.namespace==$binding.sourceIdentity.namespace and .metadata.name==$binding.sourceIdentity.name and
        .spec.url==$binding.sourceIdentity.artifactURL and .spec.ref.digest==$binding.sourceIdentity.digest and
        (.metadata.generation|type=="number" and .>0 and floor==.)) |
      if ((.status.artifact.revision // "")=="") and
        all(.status.conditions[]?; (.type!="Ready" and .type!="SourceVerified") or .status!="True")
      then "pending"
      else select(.status.observedGeneration==.metadata.generation and
        (.status.artifact.revision==$binding.sourceIdentity.digest or
         .status.artifact.revision==(($binding.publication.gitTag|ltrimstr("v"))+"@"+$binding.sourceIdentity.digest)) and
        ([.status.conditions[]?|select(.type=="Ready")]|length)==1 and
        ([.status.conditions[]?|select(.type=="SourceVerified")]|length)==1 and
        all(.status.conditions[]?|select(.type=="Ready" or .type=="SourceVerified");
          .status=="True" and .observedGeneration==$source.metadata.generation)) | "observed"
      end
    end
  ' "$state_inventory"
)

# This reader is also used by the guard: generated current observations and
# bootstrap authority cannot contradict each other or invent legacy history.
canonical_observation_consistent() (
  set -euo pipefail
  trap - EXIT
  local consistency_record="$1" identities="$2" expected actual
  [ -f "$identities" ] && [ ! -L "$identities" ] || return 1
  [ "$(head -n1 "$identities")" = $'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_publisher_identity\tmain_publisher_identity\tobserved_on' ] || return 1
  actual="$(awk -F '\t' '$1=="business-site" {if(NF!=7) exit 1; print}' "$identities")" || return 1
  if [ "$(printf '%s\n' "$consistency_record" | jq -r .kind)" = published-bootstrap ]; then
    [ -z "$actual" ]
  else
    expected="$(printf '%s\n' "$consistency_record" | jq -r '[.consumer,.workflow,(.publication.gitTag|ltrimstr("v")),.sourceIdentity.digest,.publication.publisherIdentity,.publication.publisherIdentity] | @tsv')"
    [ "$(printf '%s\n' "$actual" | wc -l | tr -d ' ')" = 1 ] &&
      [ "${actual%$'\t'*}" = "$expected" ] &&
      [[ "${actual##*$'\t'}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]
  fi
)

