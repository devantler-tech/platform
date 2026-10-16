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
        .critical.identity["docker-reference"] == ($artifact + "@" + $digest)))' "$verification_dir/result" >/dev/null
)
