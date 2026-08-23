#!/usr/bin/env bash
# ResourceGraphDefinition workload templates are nested under a custom resource, so ordinary
# repository scans do not inspect them as Kubernetes manifests. This behavioural test proves the
# dedicated gate scans both the committed templates and a deliberately non-compliant mutation.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GATE="${REPO_ROOT}/scripts/scan-rgd-templates.sh"
readonly BASELINE="${REPO_ROOT}/scripts/rgd-template-static-scan-baseline.tsv"
readonly WIRING_TEST="${REPO_ROOT}/scripts/tests/test-rgd-template-static-scan-wiring.sh"
readonly WEBAPP_RGD="k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"
readonly TENANT_RGD="k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
readonly WEBAPP_INSTANCE="k8s/providers/docker/apps/web-app-wedding-app.yaml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ -x "$GATE" ] || fail "the RGD template static-scan gate is missing or not executable: $GATE"
command -v yq >/dev/null 2>&1 || fail "yq is required"
command -v trivy >/dev/null 2>&1 || fail "trivy is required"

# shellcheck disable=SC2016 # These are literal source invariants, not test-shell arithmetic.
grep -Fq 'semantic_start_line=$((finding_start_line - leading_blank_lines - 1))' "$GATE" ||
  fail "Trivy one-based start lines are not converted to yq zero-based lines"
# shellcheck disable=SC2016 # These are literal source invariants, not test-shell arithmetic.
grep -Fq 'semantic_end_line=$((finding_end_line - leading_blank_lines - 1))' "$GATE" ||
  fail "Trivy one-based end lines are not converted to yq zero-based lines"

"$WIRING_TEST" || fail "the RGD gate is not wired on every protected event route"

# The real committed templates are the positive control. A gate that rejects them cannot be wired
# into pull requests, and a test that exercises only the mutation cannot prove that it can.
"$GATE" "$REPO_ROOT" || fail "the committed RGD templates do not pass the static-scan gate"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

copy_rgd_inputs() {
  local source_root="$1" target_root="$2"
  local input_index="${target_root}/.rgd-inputs.tsv"
  local candidate candidate_json relative_path schema_api_version schema_kind
  local definition_name generated_api_group generated_api_version instance_json
  local instance_api_version instance_kind matches_rgd

  : >"$input_index"
  while IFS= read -r candidate; do
    candidate_json="$(yq -o=json -I=0 '.' "$candidate" |
      jq -c 'select(type == "object" and (.kind // "") == "ResourceGraphDefinition")')"
    [ -n "$candidate_json" ] || continue
    relative_path="${candidate#"$source_root"/}"
    mkdir -p "$target_root/$(dirname "$relative_path")"
    cp "$candidate" "$target_root/$relative_path"

    schema_api_version="$(jq -r '.spec.schema.apiVersion // ""' <<<"$candidate_json")"
    schema_kind="$(jq -r '.spec.schema.kind // ""' <<<"$candidate_json")"
    [ -n "$schema_api_version" ] || fail "fixture RGD omits spec.schema.apiVersion: $candidate"
    [ -n "$schema_kind" ] || fail "fixture RGD omits spec.schema.kind: $candidate"
    if [[ "$schema_api_version" == */* ]]; then
      generated_api_version="$schema_api_version"
    else
      definition_name="$(jq -r '.metadata.name // ""' <<<"$candidate_json")"
      generated_api_group="${definition_name#*.}"
      if [ -z "$definition_name" ] || [ "$generated_api_group" = "$definition_name" ]; then
        fail "fixture RGD metadata.name does not encode a generated API group: $candidate"
      fi
      generated_api_version="${generated_api_group}/${schema_api_version}"
    fi
    printf '%s\t%s\n' "$generated_api_version" "$schema_kind" >>"$input_index"
  done < <(find "$source_root/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

  [ -s "$input_index" ] || fail "fixture source contains no ResourceGraphDefinitions"
  while IFS= read -r candidate; do
    matches_rgd=false
    while IFS= read -r instance_json; do
      instance_api_version="$(jq -r '.apiVersion // ""' <<<"$instance_json")"
      instance_kind="$(jq -r '.kind // ""' <<<"$instance_json")"
      if awk -F '\t' -v api="$instance_api_version" -v kind="$instance_kind" '
        $1 == api && $2 == kind { found = 1 }
        END { exit(found ? 0 : 1) }
      ' "$input_index"; then
        matches_rgd=true
        break
      fi
    done < <(
      yq -o=json -I=0 'select(tag == "!!map")' "$candidate" |
        jq -cS 'select(.kind != null)'
    )
    "$matches_rgd" || continue
    relative_path="${candidate#"$source_root"/}"
    mkdir -p "$target_root/$(dirname "$relative_path")"
    cp "$candidate" "$target_root/$relative_path"
  done < <(find "$source_root/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | LC_ALL=C sort)

  rm -f "$input_index"
}

copy_rgd_inputs "$REPO_ROOT" "$WORK"

# Kind alone does not identify a generated KRO API. An unrelated group may legitimately define the
# same Kind; it must not inherit KRO-specific placement or substitution policies.
readonly UNRELATED_INSTANCE="k8s/providers/docker/apps/unrelated-webapp.yaml"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$UNRELATED_INSTANCE"
yq -i '.apiVersion = "example.io/v1" | .spec.name = "kube-system"' "$WORK/$UNRELATED_INSTANCE"
"$GATE" "$WORK" || fail "an unrelated API group with a shared Kind was treated as an RGD instance"
rm -f "$WORK/$UNRELATED_INSTANCE"
echo "PASS(probe): RGD instances are matched by API version and kind."

# Finding counts must remain attributable to individual nested resources. Otherwise removing one
# KSV-0039 finding while introducing the same rule on a sibling resource leaves the aggregate row
# unchanged after the graph digest is reviewed.
tenant_ksv0039_targets="$({
  awk -F '\t' '$2 ~ /resource-graph-definitions\/tenant/ && $3 == "KSV-0039" { print $2 }' "$BASELINE" |
    LC_ALL=C sort -u
} | wc -l | tr -d ' ')"
[ "$tenant_ksv0039_targets" -gt 1 ] ||
  fail "the finding ratchet collapses KSV-0039 across Tenant resources"

# The content ratchet represents parsed template semantics, not yq's emitted presentation. Put a
# wrapper first on PATH that adds harmless blank lines and double-quotes every string in each
# serialized resource. This changes Trivy's ranges and cause text without changing the parsed
# manifest, so finding identity must remain stable.
YQ_BIN="$(command -v yq)"
readonly YQ_BIN
mkdir -p "$WORK/bin"
cat >"$WORK/bin/yq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = '(.spec.resources[] | select(.id == strenv(RESOURCE_ID))).template' ]; then
  printf '\n\n'
  "$REAL_YQ" "$@" | "$REAL_YQ" '(.. | select(tag == "!!str")) style="double"'
else
  exec "$REAL_YQ" "$@"
fi
EOF
chmod +x "$WORK/bin/yq"
PATH="$WORK/bin:$PATH" REAL_YQ="$YQ_BIN" "$GATE" "$WORK" ||
  fail "the finding or content ratchet depends on yq's YAML serialization"
echo "PASS(probe): finding and content evidence ignore serializer-only presentation."

expect_rejected() {
  local label="$1" log="${WORK}/gate-probe.log" expected_fragment
  shift
  if "$GATE" "$WORK" >"$log" 2>&1; then
    fail "the gate accepted $label"
  fi
  for expected_fragment in "$@"; do
    if ! grep -Fq "$expected_fragment" "$log"; then
      sed 's/^/  /' "$log" >&2
      fail "the gate failed for $label without reporting the expected $expected_fragment finding"
    fi
  done
  echo "PASS(probe): $label is rejected with the expected evidence."
}

restore_webapp() {
  cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$WEBAPP_RGD"
}

restore_tenant() {
  cp "$REPO_ROOT/$TENANT_RGD" "$WORK/$TENANT_RGD"
}

# A future RGD must enter the scan automatically. Copying a valid current definition under a new
# sibling path reproduces the review finding: a fixed two-file list reports success while leaving
# this third definition completely unseen.
readonly PROBE_RGD="k8s/bases/infrastructure/resource-graph-definitions/probe/resource-graph-definition.yaml"
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
expect_rejected "an unbaselined third ResourceGraphDefinition" "$PROBE_RGD"
rm -f "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# Direct-main and manual-CD routes run this gate without the repository naming check. A new RGD
# hidden as one document in a multi-document file must therefore fail here rather than disappear
# from discovery when the parsed kinds collapse into a newline-separated value.
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
yq -i '.metadata.name = "multidocwebapp.kro.run" |
  .spec.schema.kind = "MultiDocWebApp"' "$WORK/$PROBE_RGD"
printf '\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: adjacent-probe\n' >>"$WORK/$PROBE_RGD"
expect_rejected "a multi-document file containing an RGD" \
  "ResourceGraphDefinition must be the only YAML document"
rm -f "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# Version-only schema APIs use the generated group encoded after the first dot in metadata.name,
# not the outer ResourceGraphDefinition CRD group. A custom-group instance must therefore still
# enter the committed-instance placement and substitution guards.
readonly GROUPED_INSTANCE="k8s/providers/docker/apps/grouped-webapp-probe.yaml"
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
yq -i '.metadata.name = "groupedwebapp.platform.example" |
  .spec.schema.kind = "GroupedWebApp"' "$WORK/$PROBE_RGD"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$GROUPED_INSTANCE"
yq -i '.apiVersion = "platform.example/v1alpha1" |
  .kind = "GroupedWebApp" | .spec.name = "kube-system"' "$WORK/$GROUPED_INSTANCE"
expect_rejected "a custom-group RGD instance targeting kube-system" "KSV-0037"
rm -f "$WORK/$GROUPED_INSTANCE" "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# Provider overlays may consume the base RGDs, but they may not mutate a graph after the base-only
# extraction. A targeted JSON patch would otherwise make the rendered provider diverge invisibly.
readonly PROVIDER_PROBE="k8s/providers/probe/infrastructure/kustomization.yaml"
mkdir -p "$WORK/$(dirname "$PROVIDER_PROBE")"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "ResourceGraphDefinition",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a provider overlay patch targeting an RGD" \
  "Kustomization targets or ambiguously selects" "ResourceGraphDefinition"
rm -f "$WORK/$PROVIDER_PROBE"

# A Kustomize selector may omit kind and still match every resource satisfying its remaining
# fields. Such a selector must not bypass the provider-overlay guard merely because `.target.kind`
# is absent.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a provider overlay RGD selector without kind" \
  "Kustomization targets or ambiguously selects" "missing kind"
rm -f "$WORK/$PROVIDER_PROBE"

# Generated instances feed values into templates after extraction too. A patch to a WebApp or
# Tenant can otherwise bypass the raw-instance placement, registry, and complete-content evidence.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "WebApp",
      "name": "wedding-app"
    },
    "patch": "- op: replace\n  path: /spec/name\n  value: kube-system"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a provider overlay patch targeting a generated WebApp" \
  "Kustomization targets or ambiguously selects" "WebApp"
rm -f "$WORK/$PROVIDER_PROBE"

yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "Web.*",
      "name": "wedding-app"
    },
    "patch": "- op: replace\n  path: /spec/name\n  value: kube-system"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a regex provider overlay patch targeting a generated WebApp" \
  "Kustomization targets or ambiguously selects" "Web.*"
rm -f "$WORK/$PROVIDER_PROBE"

# Standalone Kustomize transformers run after the raw RGD is read by the scanner. A targeted
# PatchTransformer must receive the same guard as an inline or path-backed patch, or it can change a
# nested workload while the reviewed definition and finding baseline remain unchanged.
readonly RGD_TRANSFORMER="k8s/providers/probe/infrastructure/patch-transformer-rgd.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .transformers = ["patch-transformer-rgd.yaml"]' >"$WORK/$PROVIDER_PROBE"
yq -n '.apiVersion = "builtin" | .kind = "PatchTransformer" |
  .metadata.name = "mutate-rgd" |
  .target = {
    "group": "kro.run",
    "version": "v1alpha1",
    "kind": "ResourceGraphDefinition",
    "name": "webapp.kro.run"
  } |
  .patch = "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"' \
  >"$WORK/$RGD_TRANSFORMER"
expect_rejected "a standalone transformer targeting an RGD" \
  "Kustomization transformer targets or ambiguously selects" "ResourceGraphDefinition"
rm -f "$WORK/$RGD_TRANSFORMER" "$WORK/$PROVIDER_PROBE"

# Path-backed replacements run after the raw definitions and instances are hashed. Loading only
# inline replacement targets leaves a referenced file free to mutate a generated instance without
# changing any reviewed evidence.
readonly RGD_REPLACEMENT="k8s/providers/probe/infrastructure/replacement-rgd.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .replacements = [{"path": "replacement-rgd.yaml"}]' >"$WORK/$PROVIDER_PROBE"
yq -n '.source = {
    "kind": "ConfigMap",
    "name": "replacement-source",
    "fieldPath": "data.namespace"
  } | .targets = [{
    "select": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "WebApp",
      "name": "wedding-app"
    },
    "fieldPaths": ["spec.name"]
  }]' >"$WORK/$RGD_REPLACEMENT"
expect_rejected "a path-backed replacement targeting a generated WebApp" \
  "Kustomization replacement targets or ambiguously selects" "WebApp"
rm -f "$WORK/$RGD_REPLACEMENT" "$WORK/$PROVIDER_PROBE"

# commonAnnotations is a built-in transformer. Overriding the RGD substitution opt-out there makes
# the raw-source check pass while Flux later expands values inside the nested templates.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .commonAnnotations."kustomize.toolkit.fluxcd.io/substitute" = "enabled"' \
  >"$WORK/$PROVIDER_PROBE"
expect_rejected "a commonAnnotations override of the Flux substitution opt-out" \
  "Kustomization must not override the Flux substitution opt-out"
rm -f "$WORK/$PROVIDER_PROBE"

# Legacy strategic-merge entries may contain inline YAML instead of a path. An unrelated patch must
# remain usable, while an inline generated-instance patch must receive the same post-scan guard as a
# path-backed patch.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patchesStrategicMerge = [
    "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated"
  ]' >"$WORK/$PROVIDER_PROBE"
"$GATE" "$WORK" || fail "the gate rejected a permitted inline legacy ConfigMap patch"
echo "PASS(probe): an unrelated inline legacy strategic-merge patch remains permitted."
rm -f "$WORK/$PROVIDER_PROBE"

yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patchesStrategicMerge = [
    "apiVersion: kro.run/v1alpha1\nkind: WebApp\nmetadata:\n  name: wedding-app\nspec:\n  name: kube-system"
  ]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "an inline legacy patch declaring a generated WebApp" \
  "Kustomization inline legacy patch declares an RGD definition or generated instance" "WebApp"
rm -f "$WORK/$PROVIDER_PROBE"

# Provider-local definitions are valid graph sources rather than mutations, but they must enter the
# same evidence baseline. Leaving discovery rooted only in the shared base would certify neither
# their nested resources nor their graph content.
readonly PROVIDER_RGD="k8s/providers/probe/infrastructure/provider-resource-graph-definition.yaml"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROVIDER_RGD"
yq -i '.metadata.name = "providerwebapp.kro.run" |
  .spec.schema.kind = "ProviderWebApp"' "$WORK/$PROVIDER_RGD"
expect_rejected "an unbaselined provider ResourceGraphDefinition" "$PROVIDER_RGD"
rm -f "$WORK/$PROVIDER_RGD"

# Kustomize accepts JSON and extensionless resources. Direct-main/manual-CD execution does not run
# the naming validator, so discovery must follow every readable resource path rather than depend on
# a .yaml/.yml suffix.
readonly JSON_RGD="k8s/providers/probe/infrastructure/provider-resource-graph-definition.json"
yq -o=json -I=2 '.metadata.name = "jsonwebapp.kro.run" |
  .spec.schema.kind = "JsonWebApp"' "$REPO_ROOT/$WEBAPP_RGD" >"$WORK/$JSON_RGD"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["provider-resource-graph-definition.json"]' \
  >"$WORK/$PROVIDER_PROBE"
expect_rejected "an unbaselined JSON ResourceGraphDefinition referenced by Kustomize" "$JSON_RGD"
rm -f "$WORK/$JSON_RGD" "$WORK/$PROVIDER_PROBE"
rmdir "$WORK/$(dirname "$PROVIDER_PROBE")"

# The same post-scan mutation is unsafe in a shared-base Kustomization, not only below providers.
# A full-tree guard must reject an inline JSON6902 patch that targets the raw RGD after extraction.
readonly BASE_RGD_KUSTOMIZATION="k8s/bases/infrastructure/resource-graph-definitions/kustomization.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["webapp", "tenant"] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "ResourceGraphDefinition",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$BASE_RGD_KUSTOMIZATION"
expect_rejected "a shared-base overlay patch targeting an RGD" \
  "Kustomization targets or ambiguously selects" "ResourceGraphDefinition"
rm -f "$WORK/$BASE_RGD_KUSTOMIZATION"

for alternate_kustomization_name in kustomization.yml Kustomization; do
  alternate_kustomization="$(dirname "$BASE_RGD_KUSTOMIZATION")/${alternate_kustomization_name}"
  yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
    .kind = "Kustomization" | .resources = ["webapp", "tenant"] | .patches = [{
      "target": {
        "group": "kro.run",
        "version": "v1alpha1",
        "kind": "ResourceGraphDefinition",
        "name": "webapp.kro.run"
      },
      "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
    }]' >"$WORK/$alternate_kustomization"
  expect_rejected "an RGD patch in $alternate_kustomization_name" \
    "Kustomization targets or ambiguously selects" "ResourceGraphDefinition"
  rm -f "$WORK/$alternate_kustomization"
done

# The infrastructure Flux Kustomization performs postBuild substitution after this scanner reads
# the raw graph. Every RGD must opt out explicitly so a `${...}` value cannot be rewritten into an
# unsafe boolean or image after the reviewed template and finding evidence have already passed.
yq -i 'del(.metadata.annotations."kustomize.toolkit.fluxcd.io/substitute")' \
  "$WORK/$WEBAPP_RGD"
expect_rejected "an RGD without the Flux substitution opt-out" \
  "Flux postBuild substitution must be disabled for every ResourceGraphDefinition"
restore_webapp

# Flux Kustomization patches are applied after the ordinary Kustomize render. They must be checked
# independently from build-file patches or they can mutate an RGD after its raw evidence is sealed.
readonly FLUX_RGD_PATCH="k8s/clusters/prod/flux-kustomization-rgd-patch-probe.yaml"
mkdir -p "$WORK/$(dirname "$FLUX_RGD_PATCH")"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" |
  .metadata.name = "rgd-patch-probe" |
  .metadata.namespace = "flux-system" |
  .spec.path = "./k8s/providers/hetzner" |
  .spec.patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "ResourceGraphDefinition",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$FLUX_RGD_PATCH"
expect_rejected "a Flux Kustomization patch targeting an RGD" \
  "Flux Kustomization targets or ambiguously selects" "ResourceGraphDefinition"
rm -f "$WORK/$FLUX_RGD_PATCH"

# Resource-level graph controls decide whether a template is instantiated. They must be evidence too:
# gating the default-deny NetworkPolicy changes tenant isolation without changing its template bytes.
# shellcheck disable=SC2016 # the KRO expression must remain literal in the fixture
yq -i '(.spec.resources[] | select(.id == "defaultDeny").includeWhen) =
  ["${schema.spec.resourceQuota.enabled}"]' "$WORK/$TENANT_RGD"
expect_rejected "a condition that disables default-deny for defaulted tenants" "RGD-CONTENT"
restore_tenant

# A baselined finding must retain its reviewed cause, not merely its ID and line range. Replacing
# the caller-supplied image expression with a forced mutable latest tag keeps KSV-0013 at the same
# location and count, so a count-only ratchet accepts the changed cause.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) = "nginx:latest"' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image' "$WORK/$WEBAPP_RGD")" = "nginx:latest" ] ||
  fail "the baselined-cause regression fixture was not created"
expect_rejected "a changed value hidden behind the same finding ID and range" "RGD-CONTENT"
restore_webapp

# Moving an existing rule between containers must be visible even when its resource-level count and
# Trivy cause text are unchanged. Clone the reviewed container byte-for-byte, then pin only the
# original image: KSV-0013 moves from containers[0] to containers[1] with identical cause content.
# The graph digest changes too, but the finding diff must retain both old and new semantic paths.
# shellcheck disable=SC2016 # $reviewed_container is a yq variable, not a shell expansion
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0]) as $reviewed_container |
  (.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) =
  "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
  (.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers) += [$reviewed_container]' "$WORK/$WEBAPP_RGD"
container_identity_log="${WORK}/container-identity.log"
if "$GATE" "$WORK" >"$container_identity_log" 2>&1; then
  fail "the gate accepted a KSV-0013 move between workload containers"
fi
container_cause_count="$(awk -F '\t' '
  $3 == "KSV-0013" && index($5, "CAUSE-SHA256:") == 1 { print $5 }
' "$container_identity_log" | LC_ALL=C sort -u | wc -l | tr -d ' ')"
[ "$container_cause_count" -gt 1 ] || {
  sed 's/^/  /' "$container_identity_log" >&2
  fail "the finding ratchet hides a KSV-0013 move between workload containers"
}
echo "PASS(probe): container-level finding identity cannot cancel within one resource."
restore_webapp

# Committed RGD instances can feed template expressions values the extraction cannot infer. Guard
# the namespace-driving spec.name so an opt-in pilot cannot instantiate otherwise-safe templates
# inside kube-system while both the ordinary custom-resource scan and template scan stay green.
yq -i '.spec.name = "kube-system"' "$WORK/$WEBAPP_INSTANCE"
[ "$(yq '.spec.name' "$WORK/$WEBAPP_INSTANCE")" = "kube-system" ] ||
  fail "the unsafe committed-instance fixture was not created"
expect_rejected "a committed WebApp instance targeting kube-system" "KSV-0037"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Parse the kind as YAML rather than prefiltering its serialized spelling. Quoting is valid and must
# not let the same unsafe instance bypass the placement guard.
yq -i '.kind style="double" | .spec.name = "kube-system"' "$WORK/$WEBAPP_INSTANCE"
[ "$(yq '.kind' "$WORK/$WEBAPP_INSTANCE")" = "WebApp" ] ||
  fail "the quoted-kind committed-instance fixture was not created"
expect_rejected "a quoted-kind WebApp instance targeting kube-system" "KSV-0037"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Security-sensitive substituted values need their own policy checks. The outer WebApp CR is not a
# workload Trivy recognizes, so an untrusted image registry otherwise remains invisible.
yq -i '.spec.image = "evil.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a committed WebApp image from an untrusted registry" "KSV-0125"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# A hostname that merely ends with a trusted registry is still a different registry. The RGD
# instance guard must be stricter than Trivy's suffix-based KSV-0125 implementation here.
yq -i '.spec.image = "attackerghcr.io/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a look-alike committed WebApp image registry" "KSV-0125"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Ratchet the complete parsed instance as well as explicit security policies. That keeps every value
# substituted into a generated resource review-visible, including fields without a Trivy rule today.
yq -i '.spec.host = "unexpected.example"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "an unreviewed committed instance substitution" "INSTANCE-CONTENT"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Mutate the nested Deployment rather than adding a top-level manifest. `privileged: true` is a
# scanner-supported Kubernetes misconfiguration and represents exactly the blind spot this gate
# closes: the outer RGD remains valid while its generated workload becomes unsafe.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].securityContext.privileged) = true' \
  "$WORK/$WEBAPP_RGD"

[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].securityContext.privileged' "$WORK/$WEBAPP_RGD")" = "true" ] ||
  fail "the non-compliant nested-template fixture was not created"

expect_rejected "a privileged container" "KSV-0017"

# The other concrete failure classes from #3055 stay covered too. Each mutation starts from the
# committed RGD so one finding cannot make another assertion pass accidentally.
restore_webapp
yq -i 'del(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].resources)' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0] | has("resources")' "$WORK/$WEBAPP_RGD")" = "false" ] ||
  fail "the missing-resource-limits fixture was not created"
expect_rejected "a container without resource requests or limits" "KSV-0011"

restore_webapp
yq -i '(.spec.resources[] | select(.id == "deployment") | .template.metadata.namespace) =
  "kube-system"' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.metadata.namespace' "$WORK/$WEBAPP_RGD")" = "kube-system" ] ||
  fail "the kube-system placement fixture was not created"
expect_rejected "a tenant workload placed in kube-system" "KSV-0037"

echo "PASS: committed RGD graphs and instances pass; graph, substitution, and workload regressions fail."
