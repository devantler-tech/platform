# shellcheck shell=bash
# Publication is evidence about a signed artifact, never proof of a Flux apply.
# Keep this companion separate from the generated applied-evidence tables.

read_bound_publications() {
  local root="$1" file="$2" document entry relative resolved source_json
  root="$(cd "$root" && pwd -P)" || return 1
  [ -f "$file" ] && [ ! -L "$file" ] || return 1
  document="$(jq -ces '
    select(length == 1) | .[0] |
    select(type == "object" and keys == ["publications","schemaVersion"] and .schemaVersion == 1) |
    select(.publications | type == "array") |
    select(all(.publications[];
      type == "object" and (keys | length) == 14 and
      all(.[]; type == "string" and length > 0 and length <= 512 and (test("[[:cntrl:]]") | not)) and
      (.kind == "published-bootstrap" or .kind == "published-binding") and
      (.consumer | test("^[a-z0-9.][a-z0-9.-]*$")) and
      (.workflow == "publish-app" or .workflow == "publish-manifests") and
      (.sourcePath | test("^k8s/[a-zA-Z0-9_./-]+[.]ya?ml$")) and
      (.sourcePath | split("/") | all(.[]; . != "" and . != "." and . != "..")) and
      (.namespace | test("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")) and
      (.name | test("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")) and
      .artifactURL == ("oci://ghcr.io/devantler-tech/" +
        (if .consumer == ".github" then "github-config" else .consumer end) + "/manifests") and
      (.gitTag | test("^v[0-9]+[.][0-9]+[.][0-9]+$")) and
      (.sourceSHA | test("^[0-9a-f]{40}$")) and
      (.digest | test("^sha256:[0-9a-f]{64}$")) and
      (.signerSHA | test("^[0-9a-f]{40}$")) and
      (.mainPinSHA | test("^[0-9a-f]{40}$")) and
      (.candidateSHA | test("^[0-9a-f]{40}$")) and
      (.verifiedOn | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")))) |
    select(([.publications[].consumer] | unique | length) == (.publications | length))
  ' "$file" 2>/dev/null)" || return 1
  while IFS= read -r entry; do
    relative="$(printf '%s\n' "$entry" | jq -r .sourcePath)" || return 1
    resolved="$(realpath "$root/$relative" 2>/dev/null)" || return 1
    [ "$resolved" = "$root/$relative" ] && [ -f "$resolved" ] || return 1
    source_json="$(yq eval -o=json -I=0 '.' "$resolved" 2>/dev/null)" || return 1
    printf '%s\n' "$source_json" | jq -ers --argjson binding "$entry" '
      length == 1 and (.[0] |
        .apiVersion == "source.toolkit.fluxcd.io/v1" and .kind == "OCIRepository" and
        .metadata.namespace == $binding.namespace and .metadata.name == $binding.name and
        .spec.url == $binding.artifactURL and
        (.spec.ref | keys == ["digest"]) and .spec.ref.digest == $binding.digest and
        .spec.suspend != true)
    ' >/dev/null 2>&1 || return 1
  done < <(printf '%s\n' "$document" | jq -c '.publications[]')
  printf '%s\n' "$document"
}

publication_binding() {
  local root="$1" file="$2" consumer="$3" workflow="$4" digest="$5" document
  document="$(read_bound_publications "$root" "$file")" || return 1
  printf '%s\n' "$document" | jq -cer \
    --arg consumer "$consumer" --arg workflow "$workflow" --arg digest "$digest" \
    '[.publications[] | select(.consumer == $consumer and .workflow == $workflow and
      .digest == $digest)] | select(length == 1) | .[0]'
}

# Provider acceptance is deliberately separate from parsing the reviewed record.
# A failed verifier cannot be replaced by plausible JSON on stdout.
verify_bound_publication() (
  local binding="$1" verification_dir identity digest artifact repo tag sha
  identity="$(printf '%s\n' "$binding" | jq -r '"https://github.com/devantler-tech/actions/.github/workflows/" + .workflow + ".yaml@" + .signerSHA')"
  digest="$(printf '%s\n' "$binding" | jq -r .digest)"
  artifact="$(printf '%s\n' "$binding" | jq -r '.artifactURL | ltrimstr("oci://")')"
  repo="$(printf '%s\n' "$binding" | jq -r .consumer)"
  tag="$(printf '%s\n' "$binding" | jq -r .gitTag)"
  sha="$(printf '%s\n' "$binding" | jq -r .sourceSHA)"
  verification_dir="$(mktemp -d)" || return 1
  trap 'rm -rf "$verification_dir"' EXIT
  cosign verify --certificate-identity "$identity" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-github-workflow-repository "devantler-tech/$repo" \
    --certificate-github-workflow-ref "refs/tags/$tag" \
    --certificate-github-workflow-sha "$sha" "$artifact@$digest" >"$verification_dir/result" 2>"$verification_dir/error" || return 1
  jq -es --arg digest "$digest" --arg artifact "$artifact" '
    length == 1 and (.[0] | type == "array" and length > 0 and
      all(.[]; .critical.image["docker-manifest-digest"] == $digest and
        (.critical.identity["docker-reference"] == $artifact or
         .critical.identity["docker-reference"] == ($artifact + "@" + $digest))))' "$verification_dir/result" >/dev/null
)

# Business-site's release caller uses a maintained publisher, not the legacy
# identity encoded by schemaVersion 1 approval records. These helpers supply
# manifest-publication evidence only; no approval generator consumes this receipt.
business_manifest_tag_commit() {
  local tag="$1" body
  body="$(gh_retry api "repos/devantler-tech/business-site/git/ref/tags/$tag")" || return 1
  printf '%s\n' "$body" | jq -ers --arg tag "$tag" '
    select(length==1) | .[0] |
    select(.ref==("refs/tags/"+$tag) and .object.type=="commit") | .object.sha |
    select(type=="string" and test("^[0-9a-f]{40}$"))' 2>/dev/null
}

business_manifest_release_record() {
  local tag="$1" body
  body="$(gh_retry api "repos/devantler-tech/business-site/releases/tags/$tag")" || return 1
  printf '%s\n' "$body" | jq -cers --arg tag "$tag" '
    select(length==1) | .[0] |
    select(.tag_name==$tag and .draft==false and .prerelease==false and
      (.id | type=="number" and .>0 and .<=9007199254740991 and floor==.) and
      (.published_at | type=="string" and length>0)) |
    {id,tag_name,draft,prerelease,published_at}' 2>/dev/null
}

business_manifest_run_record() {
  local tag="$1" source_sha="$2"
  jq -cers --arg tag "$tag" --arg source "$source_sha" '
    select(length==1) | .[0] |
    select(type=="object" and .repository.full_name=="devantler-tech/business-site" and
      .event=="release" and .path==".github/workflows/publish-runtime.yaml" and
      .head_branch==$tag and .head_sha==$source and .status=="completed" and .conclusion=="success" and
      (.id | type=="number" and .>0 and .<=9007199254740991 and floor==.) and
      (.run_attempt | type=="number" and .>0 and .<=9007199254740991 and floor==.) and
      (.referenced_workflows | type=="array" and length==1) and
      .referenced_workflows[0].sha=="467f07582436a26b62cf4aa756423512f85ba0c0" and
      .referenced_workflows[0].path=="devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0") |
    {id,run_attempt,event,path,head_branch,head_sha,status,conclusion,repository:.repository.full_name,
      publisherIdentity:.referenced_workflows[0].path}' 2>/dev/null
}

business_site_manifest_evidence() (
  local consumer='' verify_only=0 tag='' source_sha='' digest='' seen=':' option
  while [ "$#" -gt 0 ]; do
    option="$1"
    case "$seen" in *":$option:"*) return 2 ;; esac
    seen="$seen$option:"
    case "$option" in
      --verify-only) verify_only=1; shift; continue ;;
      --consumer | --tag | --source-sha | --digest) [ "$#" -ge 2 ] || return 2 ;;
      *) return 2 ;;
    esac
    case "$option" in
      --consumer) consumer="$2" ;;
      --tag) tag="$2" ;;
      --source-sha) source_sha="$2" ;;
      --digest) digest="$2" ;;
    esac
    shift 2
  done
  [ "$consumer" = business-site ] && [ "$verify_only" -eq 1 ] || return 2
  [[ "$tag" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] || return 2
  [[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || return 2
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 2
  local publisher='devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0'
  local artifact='ghcr.io/devantler-tech/business-site/manifests'
  local release runs selected record caller calls business_verification_dir current run_id run_attempt
  [ "$(business_manifest_tag_commit "$tag")" = "$source_sha" ] || return 1
  release="$(business_manifest_release_record "$tag")" || return 1
  runs="$(gh_retry api --method GET repos/devantler-tech/business-site/actions/runs \
    --raw-field "branch=$tag" --raw-field event=release --raw-field per_page=100)" || return 1
  selected="$(printf '%s\n' "$runs" | jq -cers --arg tag "$tag" --arg source "$source_sha" '
    select(length==1) | .[0] |
    select(type=="object" and (.workflow_runs | type=="array") and
      (.total_count | type=="number" and .>=0 and .<=100 and floor==.) and
      .total_count==(.workflow_runs | length) and
      all(.workflow_runs[]; type=="object" and
        (.head_sha | type=="string" and test("^[0-9a-f]{40}$")) and
        (.head_branch | type=="string" or type=="null") and
        (.event | type=="string") and (.path | type=="string") and
        (.status | type=="string") and (.conclusion | type=="string" or type=="null"))) |
    [.workflow_runs[] | select(.head_branch==$tag and .head_sha==$source and
      .event=="release" and .path==".github/workflows/publish-runtime.yaml" and
      .status=="completed" and .conclusion=="success")] | select(length==1) | .[0]' 2>/dev/null)" || return 1
  record="$(printf '%s\n' "$selected" | business_manifest_run_record "$tag" "$source_sha")" || return 1
  run_id="$(printf '%s\n' "$record" | jq -r .id)" || return 1
  run_attempt="$(printf '%s\n' "$record" | jq -r .run_attempt)" || return 1
  caller="$(gh_retry api --method GET repos/devantler-tech/business-site/contents/.github/workflows/publish-runtime.yaml \
    --raw-field "ref=$source_sha" -H 'Accept: application/vnd.github.raw')" || return 1
  calls="$(printf '%s\n' "$caller" | yq eval -o=json -I=0 '.' - 2>/dev/null)" || return 1
  printf '%s\n' "$calls" | jq -es --arg publisher "$publisher" '
    length==1 and (.[0] | type=="object" and .on.release.types==["published"] and
      (.jobs | type=="object") and
      ([.jobs[].uses // "" | select(type=="string" and contains("/.github/workflows/publish-app.yaml@"))] == [$publisher]))' \
    >/dev/null 2>&1 || return 1
  business_verification_dir="$(mktemp -d)" || return 1
  trap 'rm -rf "$business_verification_dir"' EXIT
  cosign verify --certificate-identity "https://github.com/$publisher" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-github-workflow-repository devantler-tech/business-site \
    --certificate-github-workflow-ref "refs/tags/$tag" \
    --certificate-github-workflow-sha "$source_sha" "$artifact@$digest" \
    >"$business_verification_dir/signature.json" 2>"$business_verification_dir/signature-error" || return 1
  jq -es --arg digest "$digest" --arg artifact "$artifact" '
    length==1 and (.[0] | type=="array" and length>0 and all(.[];
      .critical.image["docker-manifest-digest"]==$digest and
      (.critical.identity["docker-reference"]==$artifact or
       .critical.identity["docker-reference"]==($artifact+"@"+$digest))))' "$business_verification_dir/signature.json" \
    >/dev/null 2>&1 || return 1
  # Rebind the original current attempt, tag and published release after registry reads.
  current="$(gh_retry api "repos/devantler-tech/business-site/actions/runs/$run_id")" || return 1
  current="$(printf '%s\n' "$current" | business_manifest_run_record "$tag" "$source_sha")" || return 1
  [ "$current" = "$record" ] || return 1
  [ "$(business_manifest_tag_commit "$tag")" = "$source_sha" ] || return 1
  [ "$(business_manifest_release_record "$tag")" = "$release" ] || return 1
  jq -n --arg tag "$tag" --arg source "$source_sha" --arg digest "$digest" \
    --arg publisher "$publisher" --argjson run "$run_id" --argjson attempt "$run_attempt" '{
      schemaVersion:1,kind:"manifest-publication-evidence",consumer:"business-site",
      deploymentAuthorized:false,gitTag:$tag,sourceSHA:$source,digest:$digest,
      publisherIdentity:$publisher,callerWorkflow:".github/workflows/publish-runtime.yaml",
      event:"release",workflowRunId:$run,workflowRunAttempt:$attempt
    }'
)
