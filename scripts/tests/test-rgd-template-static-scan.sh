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
grep -Fq -- '--connect-timeout 10 --max-time 60' "$GATE" ||
  fail "remote resource fetches are not bounded by connection and transfer timeouts"
# shellcheck disable=SC2016 # the scanner variable reference is a literal source invariant.
grep -Fq -- '--max-filesize "$MAX_REMOTE_RESOURCE_BYTES"' "$GATE" ||
  fail "remote resource fetches are not bounded by response size"

production_override_log="$(mktemp)"
if RGD_TEST_REMOTE_BASELINE="${REPO_ROOT}/scripts/rgd-template-static-scan-remote-resources.tsv" \
  "$GATE" "$REPO_ROOT" >"$production_override_log" 2>&1; then
  fail "the gate accepted a test-only remote baseline for the production source root"
fi
grep -Fq "RGD_TEST_REMOTE_BASELINE is only permitted for an isolated fixture source" \
  "$production_override_log" || fail "the production remote-baseline override did not fail closed"
rm -f "$production_override_log"

"$WIRING_TEST" || fail "the RGD gate is not wired on every protected event route"

# The real committed templates are the positive control. A gate that rejects them cannot be wired
# into pull requests, and a test that exercises only the mutation cannot prove that it can.
"$GATE" "$REPO_ROOT" || fail "the committed RGD templates do not pass the static-scan gate"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

copy_rgd_inputs() {
  local source_root="$1" target_root="$2"
  local input_index="${target_root}/.rgd-inputs.tsv"
  local candidate_index="${target_root}/.rgd-candidates.txt"
  local candidate candidate_json relative_path schema_api_version schema_kind
  local definition_name generated_api_group generated_api_version instance_json
  local instance_api_version instance_kind matches_rgd kustomization_file resource_path resource_file

  : >"$input_index"
  {
    find "$source_root/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print
    while IFS= read -r kustomization_file; do
      while IFS= read -r resource_path; do
        [ -n "$resource_path" ] || continue
        resource_file="${kustomization_file%/*}/${resource_path}"
        [ -f "$resource_file" ] || continue
        printf '%s/%s\n' "$(cd "$(dirname "$resource_file")" && pwd)" "$(basename "$resource_file")"
      done < <(yq -o=json -I=0 '(.resources[]?, .components[]?, .bases[]?)' "$kustomization_file" |
        jq -r 'select(type == "string")')
    done < <(
      find "$source_root/k8s" -type f \
        \( -name 'kustomization.yaml' -o -name 'kustomization.yml' -o -name 'Kustomization' \) \
        -print
    )
  } | LC_ALL=C sort -u >"$candidate_index"

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
  done <"$candidate_index"

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
  done <"$candidate_index"

  rm -f "$candidate_index" "$input_index"
}

# The gate discovers Kustomize-readable JSON and extensionless inputs, so the fixture copier must
# preserve that same set. Otherwise adding their reviewed baseline rows breaks every later probe.
readonly COPY_PROBE_SOURCE="${WORK}/copy-probe-source"
readonly COPY_PROBE_TARGET="${WORK}/copy-probe-target"
readonly COPY_PROBE_RGD="k8s/rgd/probe.json"
mkdir -p "$COPY_PROBE_SOURCE/k8s/rgd" "$COPY_PROBE_TARGET"
yq -o=json -I=2 '.' "$REPO_ROOT/$WEBAPP_RGD" >"$COPY_PROBE_SOURCE/$COPY_PROBE_RGD"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["probe.json"]' \
  >"$COPY_PROBE_SOURCE/k8s/rgd/kustomization.yaml"
copy_rgd_inputs "$COPY_PROBE_SOURCE" "$COPY_PROBE_TARGET"
[ -r "$COPY_PROBE_TARGET/$COPY_PROBE_RGD" ] ||
  fail "the fixture copier omitted a Kustomize-referenced JSON RGD"
rm -rf "$COPY_PROBE_SOURCE" "$COPY_PROBE_TARGET"

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

# A cluster Kustomize overlay may patch a safe Flux control after its raw document has been scanned.
# The rendered object must not gain protected post-render controls invisibly.
readonly FLUX_OVERLAY_ROOT="k8s/clusters/flux-overlay-probe"
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["flux-kustomization.yaml"] |
  .patches = [{"patch": "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: infrastructure\n  namespace: flux-system\nspec:\n  patches:\n    - target:\n        group: kro.run\n        version: v1alpha1\n        kind: ResourceGraphDefinition\n      patch: |-\n        - op: add\n          path: /spec/resources/0/template/spec/privileged\n          value: true"}]' \
  >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "a Kustomize overlay that injects protected Flux controls" \
  "Kustomization patch mutates protected Flux controls" "spec.patches"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

# Replacements run after raw Flux controls are scanned too. Rewriting a safe patch selector from a
# ConfigMap to an RGD must not bypass the protected-field guard via a `kind: Kustomization` target.
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "v1" | .kind = "ConfigMap" |
  .metadata.name = "flux-replacement-source" | .data.kind = "ResourceGraphDefinition"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/replacement-source.yaml"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.patches = [{
    "target": {"apiVersion": "v1", "kind": "ConfigMap", "name": "safe-probe"},
    "patch": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: safe-probe"
  }]' >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["replacement-source.yaml", "flux-kustomization.yaml"] |
  .replacements = [{
    "source": {"kind": "ConfigMap", "name": "flux-replacement-source", "fieldPath": "data.kind"},
    "targets": [{
      "select": {
        "group": "kustomize.toolkit.fluxcd.io",
        "version": "v1",
        "kind": "Kustomization",
        "name": "infrastructure"
      },
      "fieldPaths": ["spec.patches.0.target.kind"]
    }]
  }]' >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "a replacement that rewrites protected Flux patch controls" \
  "Kustomization replacement mutates protected Flux controls" "spec.patches"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

# The same protected-control guard must hold when the replacement lives in a referenced file written
# as a Kustomize sequence. Reading that shape as a single mapping yields no targets at all, so the
# guard is skipped and the rewrite reaches Flux with nothing reported — a silent bypass rather than a
# rejected build.
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "v1" | .kind = "ConfigMap" |
  .metadata.name = "flux-replacement-source" | .data.kind = "ResourceGraphDefinition"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/replacement-source.yaml"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.patches = [{
    "target": {"apiVersion": "v1", "kind": "ConfigMap", "name": "safe-probe"},
    "patch": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: safe-probe"
  }]' >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '[{
    "source": {"kind": "ConfigMap", "name": "flux-replacement-source", "fieldPath": "data.kind"},
    "targets": [{
      "select": {
        "group": "kustomize.toolkit.fluxcd.io",
        "version": "v1",
        "kind": "Kustomization",
        "name": "infrastructure"
      },
      "fieldPaths": ["spec.patches.0.target.kind"]
    }]
  }]' >"$WORK/$FLUX_OVERLAY_ROOT/replacement-flux.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["replacement-source.yaml", "flux-kustomization.yaml"] |
  .replacements = [{"path": "replacement-flux.yaml"}]' \
  >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "a sequence-shaped path-backed replacement rewriting protected Flux patch controls" \
  "Kustomization replacement mutates protected Flux controls" "spec.patches"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

# Legacy strategic merges support inline YAML and must enter the same rendered Flux-control guard as
# modern patches. Their embedded Flux identity otherwise hides the protected target from raw scans.
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["flux-kustomization.yaml"] |
  .patchesStrategicMerge = ["apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: infrastructure\n  namespace: flux-system\nspec:\n  patches:\n    - target:\n        group: kro.run\n        version: v1alpha1\n        kind: ResourceGraphDefinition\n      patch: |\n        apiVersion: kro.run/v1alpha1\n        kind: ResourceGraphDefinition\n        metadata:\n          name: webapp.kro.run"]' \
  >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "an inline legacy strategic merge that injects protected Flux controls" \
  "Kustomization patch mutates protected Flux controls" "spec.patches"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

# Standalone PatchTransformers are another post-source mutation surface. A Flux selector plus a
# protected patch body must receive the same rendered-control validation as modern/legacy patches.
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '.apiVersion = "builtin" | .kind = "PatchTransformer" |
  .metadata.name = "flux-control-probe" |
  .target = {
    "group": "kustomize.toolkit.fluxcd.io",
    "version": "v1",
    "kind": "Kustomization",
    "name": "infrastructure"
  } |
  .patch = "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: infrastructure\nspec:\n  patches:\n    - target:\n        group: kro.run\n        version: v1alpha1\n        kind: ResourceGraphDefinition\n      patch: |\n        apiVersion: kro.run/v1alpha1\n        kind: ResourceGraphDefinition\n        metadata:\n          name: webapp.kro.run"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/flux-transformer.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["flux-kustomization.yaml"] |
  .transformers = ["flux-transformer.yaml"]' >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "a standalone transformer that injects protected Flux controls" \
  "Kustomization transformer mutates protected Flux controls" "spec.patches"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

# Custom FieldSpecs can redirect a built-in annotation transformer into Flux commonMetadata. The
# path and selector must be rejected even when the raw Flux object itself preserves the opt-out.
mkdir -p "$WORK/$FLUX_OVERLAY_ROOT"
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "infrastructure" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner"' \
  >"$WORK/$FLUX_OVERLAY_ROOT/flux-kustomization.yaml"
yq -n '.commonAnnotations = [{
    "group": "kustomize.toolkit.fluxcd.io",
    "version": "v1",
    "kind": "Kustomization",
    "path": "spec/commonMetadata/annotations",
    "create": true
  }]' >"$WORK/$FLUX_OVERLAY_ROOT/flux-fields.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["flux-kustomization.yaml"] |
  .commonAnnotations."kustomize.toolkit.fluxcd.io/substitute" = "enabled" |
  .configurations = ["flux-fields.yaml"]' >"$WORK/$FLUX_OVERLAY_ROOT/kustomization.yaml"
expect_rejected "a custom FieldSpec that rewrites protected Flux commonMetadata" \
  "Kustomization configuration mutates protected Flux controls" "spec/commonMetadata"
rm -rf "$WORK/${FLUX_OVERLAY_ROOT:?}"

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

# Inline replacements are parsed in the build file rather than a referenced replacement document.
# Keep a dedicated probe so the earlier selector guard cannot regress while path-backed coverage stays
# green.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .replacements = [{
    "source": {"kind": "ConfigMap", "name": "replacement-source", "fieldPath": "data.namespace"},
    "targets": [{
      "select": {"group": "kro.run", "version": "v1alpha1", "kind": "WebApp"},
      "fieldPaths": ["spec.name"]
    }]
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "an inline replacement targeting a generated WebApp" \
  "Kustomization targets or ambiguously selects" "WebApp"
rm -f "$WORK/$PROVIDER_PROBE"

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

# Kustomize's own documented shape for a replacement file is a SEQUENCE of replacement entries, and
# the single-mapping spelling above is the exception rather than the rule. A scanner that reads only
# the mapping spelling sees no targets at all in the ordinary file, so the guard above must hold for
# both.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .replacements = [{"path": "replacement-rgd.yaml"}]' >"$WORK/$PROVIDER_PROBE"
yq -n '[{
    "source": {
      "kind": "ConfigMap",
      "name": "replacement-source",
      "fieldPath": "data.namespace"
    },
    "targets": [{
      "select": {
        "group": "kro.run",
        "version": "v1alpha1",
        "kind": "WebApp",
        "name": "wedding-app"
      },
      "fieldPaths": ["spec.name"]
    }]
  }]' >"$WORK/$RGD_REPLACEMENT"
expect_rejected "a sequence-shaped path-backed replacement targeting a generated WebApp" \
  "Kustomization replacement targets or ambiguously selects" "WebApp"
rm -f "$WORK/$RGD_REPLACEMENT" "$WORK/$PROVIDER_PROBE"

# commonAnnotations is a built-in transformer. Even an explicitly empty value erases the RGD
# substitution opt-out, making the raw-source check pass while Flux later expands nested templates.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["../../../bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"] |
  .commonAnnotations."kustomize.toolkit.fluxcd.io/substitute" = ""' \
  >"$WORK/$PROVIDER_PROBE"
expect_rejected "a commonAnnotations override of the Flux substitution opt-out" \
  "Kustomization must not override the Flux substitution opt-out"
rm -f "$WORK/$PROVIDER_PROBE"

# The substitute key was the ONLY commonAnnotations entry the guard inspected, so every other
# annotation reached the sealed graph unchecked -- including the Flux behaviour controls, which
# change how the RGD is applied without changing a byte this scan reads. Probe with an unrelated
# annotation so the general transformer guard is what has to catch it, not the substitute check.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["../../../bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"] |
  .commonAnnotations."kustomize.toolkit.fluxcd.io/reconcile" = "disabled"' \
  >"$WORK/$PROVIDER_PROBE"
expect_rejected "an unrelated commonAnnotations entry over a consumed RGD" \
  "Kustomization uses a built-in transformer that can mutate protected resources" "commonAnnotations"
rm -f "$WORK/$PROVIDER_PROBE"

# Built-in global transformers mutate every resource accumulated by the Kustomization. Applying a
# label to a directly consumed RGD changes the deployed graph after its raw source was reviewed.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["../../../bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"] |
  .commonLabels = {"rgd-probe": "mutated"}' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a built-in commonLabels transformer over a consumed RGD" \
  "Kustomization uses a built-in transformer that can mutate protected resources" "commonLabels"
rm -f "$WORK/$PROVIDER_PROBE"

# Custom var references are post-source transformers too. A WebApp image containing $(IMAGE) is
# accepted as an ordinary Docker Hub reference by the raw scanner, then configurations can redirect
# that field to an unreviewed registry during the Kustomize build.
readonly RGD_VAR_CONFIGURATION="k8s/providers/probe/infrastructure/rgd-vars.yaml"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .configurations = ["rgd-vars.yaml"] |
  .vars = [{
    "name": "IMAGE",
    "objref": {"apiVersion": "v1", "kind": "ConfigMap", "name": "image-vars"},
    "fieldref": {"fieldpath": "data.image"}
  }]' >"$WORK/$PROVIDER_PROBE"
yq -n '.varReference = [{
  "group": "kro.run",
  "version": "v1alpha1",
  "kind": "WebApp",
  "path": "spec/image"
}]' >"$WORK/$RGD_VAR_CONFIGURATION"
expect_rejected "a custom varReference targeting a generated WebApp" \
  "Kustomization configuration targets or ambiguously selects" "WebApp"
rm -f "$WORK/$RGD_VAR_CONFIGURATION" "$WORK/$PROVIDER_PROBE"

# ImageTransformer field specs extend the top-level images directive into custom resources. That
# would make an apparently reviewed WebApp image replaceable whenever an image transformer is added.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] |
  .configurations = ["rgd-vars.yaml"]' \
  >"$WORK/$PROVIDER_PROBE"
yq -n '.images = [{
  "group": "kro.run",
  "version": "v1alpha1",
  "kind": "WebApp",
  "path": "spec/image"
}]' >"$WORK/$RGD_VAR_CONFIGURATION"
expect_rejected "a custom image transformer targeting a generated WebApp" \
  "Kustomization configuration targets or ambiguously selects" "WebApp"
rm -f "$WORK/$RGD_VAR_CONFIGURATION" "$WORK/$PROVIDER_PROBE"

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
expect_rejected "an unbaselined JSON ResourceGraphDefinition referenced by Kustomize" \
  "$JSON_RGD" "RGD graph, instance, or finding evidence changed"
rm -f "$WORK/$JSON_RGD" "$WORK/$PROVIDER_PROBE"

# Kubernetes List is expanded by Kustomize. A protected item nested below its top-level kind must
# not disappear from source discovery and baseline evidence.
readonly LIST_RGD="k8s/providers/probe/infrastructure/provider-resource-graph-definition-list.json"
WEBAPP_RGD_SOURCE="$REPO_ROOT/$WEBAPP_RGD" yq -n -o=json -I=2 '
  .apiVersion = "v1" | .kind = "List" | .items = [load(strenv(WEBAPP_RGD_SOURCE))]' \
  >"$WORK/$LIST_RGD"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["provider-resource-graph-definition-list.json"]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a ResourceGraphDefinition nested in a Kubernetes List" \
  "Kubernetes List contains a ResourceGraphDefinition" "$LIST_RGD"
rm -f "$WORK/$LIST_RGD" "$WORK/$PROVIDER_PROBE"

WEBAPP_INSTANCE_SOURCE="$REPO_ROOT/$WEBAPP_INSTANCE" yq -n -o=json -I=2 '
  .apiVersion = "v1" | .kind = "List" | .items = [load(strenv(WEBAPP_INSTANCE_SOURCE))]' \
  >"$WORK/$LIST_RGD"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["provider-resource-graph-definition-list.json"]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a generated WebApp nested in a Kubernetes List" \
  "Kubernetes List contains a generated RGD instance" "WebApp"
rm -f "$WORK/$LIST_RGD" "$WORK/$PROVIDER_PROBE"

# A new remote remains renderable by Kustomize but has no reviewed URL/content digest in this source
# scanner's evidence. Reject it explicitly instead of silently treating the URL as a missing file.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["https://example.invalid/rgd.yaml"]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "an unscanned remote Kustomize resource" \
  "Kustomization resource is not a local path" "https://example.invalid/rgd.yaml"
rm -f "$WORK/$PROVIDER_PROBE"
rmdir "$WORK/$(dirname "$PROVIDER_PROBE")"

# A digest-pinned remote may still contain a Flux Kustomization whose post-render controls target a
# protected graph. Remote bytes are not added to the ordinary resource-path scan, so reject this
# control-plane kind explicitly rather than treating the digest alone as sufficient evidence.
readonly REMOTE_FLUX_ROOT="k8s/providers/probe/remote-flux"
readonly REMOTE_FLUX_URL="https://example.invalid/flux-kustomization.yaml"
readonly REMOTE_FLUX_FILE="${WORK}/remote-flux-kustomization.yaml"
readonly REMOTE_FLUX_BASELINE="${WORK}/remote-flux-baseline.tsv"
readonly REMOTE_CURL_BIN="${WORK}/remote-bin"
export REMOTE_FLUX_URL
mkdir -p "$WORK/$REMOTE_FLUX_ROOT" "$REMOTE_CURL_BIN"
yq -n '.apiVersion = "v1" | .kind = "List" | .items = [{
    "apiVersion": "kustomize.toolkit.fluxcd.io/v1",
    "kind": "Kustomization",
    "metadata": {"name": "remote-flux-probe", "namespace": "flux-system"},
    "spec": {
      "path": "./k8s/providers/hetzner",
      "patches": [{
        "target": {"group": "kro.run", "version": "v1alpha1", "kind": "ResourceGraphDefinition"},
        "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
      }]
    }
  }]' >"$REMOTE_FLUX_FILE"
remote_flux_sha="$(
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$REMOTE_FLUX_FILE" | awk '{print $1}'
  else
    shasum -a 256 "$REMOTE_FLUX_FILE" | awk '{print $1}'
  fi
)"
printf '%s\t%s\n' "$REMOTE_FLUX_URL" "$remote_flux_sha" >"$REMOTE_FLUX_BASELINE"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [strenv(REMOTE_FLUX_URL)]' \
  >"$WORK/$REMOTE_FLUX_ROOT/kustomization.yaml"
cat >"$REMOTE_CURL_BIN/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      output="$2"
      shift 2
      ;;
    *) shift ;;
  esac
done
[ -n "$output" ]
cp "$FAKE_REMOTE_FILE" "$output"
EOF
chmod +x "$REMOTE_CURL_BIN/curl"
remote_flux_log="${WORK}/remote-flux.log"
if PATH="$REMOTE_CURL_BIN:$PATH" FAKE_REMOTE_FILE="$REMOTE_FLUX_FILE" \
  RGD_TEST_REMOTE_BASELINE="$REMOTE_FLUX_BASELINE" "$GATE" "$WORK" >"$remote_flux_log" 2>&1; then
  fail "the gate accepted a reviewed remote Flux Kustomization"
fi
if ! grep -Fq "remote Kustomize resource declares a Flux Kustomization" "$remote_flux_log"; then
  sed 's/^/  /' "$remote_flux_log" >&2
  fail "the remote Flux probe failed without the explicit control-plane rejection"
fi
echo "PASS(probe): reviewed remote Flux Kustomizations are rejected explicitly."
rm -rf "$WORK/${REMOTE_FLUX_ROOT:?}" "$REMOTE_CURL_BIN"
rm -f "$REMOTE_FLUX_FILE" "$REMOTE_FLUX_BASELINE" "$remote_flux_log"

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

# Built-in transformers are ordinary Kustomize features for unrelated applications. The protected
# guard must follow the resource graph rather than ban them across the complete Kubernetes tree.
readonly UNRELATED_CONFIG_MAP="k8s/providers/probe/infrastructure/unrelated-config-map.yaml"
mkdir -p "$WORK/$(dirname "$PROVIDER_PROBE")"
yq -n '.apiVersion = "v1" | .kind = "ConfigMap" | .metadata.name = "unrelated"' \
  >"$WORK/$UNRELATED_CONFIG_MAP"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["unrelated-config-map.yaml"] |
  .images = [{"name": "unrelated", "newName": "example.invalid/unrelated"}]' \
  >"$WORK/$PROVIDER_PROBE"
"$GATE" "$WORK" || fail "the gate rejected a built-in transformer on unrelated resources"
echo "PASS(probe): unrelated built-in transformers remain permitted."
rm -f "$WORK/$UNRELATED_CONFIG_MAP" "$WORK/$PROVIDER_PROBE"

# Conversely, a parent Kustomization's transformer reaches protected resources through a child
# Kustomization directory and must be rejected transitively.
readonly RGD_CHILD_KUSTOMIZATION="k8s/providers/probe/infrastructure/rgd-child/kustomization.yaml"
mkdir -p "$WORK/$(dirname "$RGD_CHILD_KUSTOMIZATION")"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["../../../../bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"]' \
  >"$WORK/$RGD_CHILD_KUSTOMIZATION"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = ["rgd-child"] |
  .commonLabels = {"rgd-probe": "mutated"}' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a transitive built-in transformer over a consumed RGD" \
  "Kustomization uses a built-in transformer that can mutate protected resources" "commonLabels"
rm -f "$WORK/$RGD_CHILD_KUSTOMIZATION" "$WORK/$PROVIDER_PROBE"
rmdir "$WORK/$(dirname "$RGD_CHILD_KUSTOMIZATION")"

# A Component's transformers apply to the including parent's accumulated resources. Even an empty
# component therefore inherits protected scope from a parent that consumes an RGD through a sibling
# resource entry.
readonly RGD_COMPONENT_KUSTOMIZATION="k8s/providers/probe/infrastructure/rgd-component/kustomization.yaml"
mkdir -p "$WORK/$(dirname "$RGD_COMPONENT_KUSTOMIZATION")"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1alpha1" |
  .kind = "Component" | .resources = [] |
  .commonAnnotations."kustomize.toolkit.fluxcd.io/substitute" = ""' \
  >"$WORK/$RGD_COMPONENT_KUSTOMIZATION"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["../../../bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"] |
  .components = ["rgd-component"]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a component transformer inherited by an RGD-consuming parent" \
  "Kustomization must not override the Flux substitution opt-out" "rgd-component"
rm -f "$WORK/$RGD_COMPONENT_KUSTOMIZATION" "$WORK/$PROVIDER_PROBE"
rmdir "$WORK/$(dirname "$RGD_COMPONENT_KUSTOMIZATION")"

# The infrastructure Flux Kustomization performs postBuild substitution after this scanner reads
# the raw graph. Every RGD must opt out explicitly so a `${...}` value cannot be rewritten into an
# unsafe boolean or image after the reviewed template and finding evidence have already passed.
yq -i 'del(.metadata.annotations."kustomize.toolkit.fluxcd.io/substitute")' \
  "$WORK/$WEBAPP_RGD"
expect_rejected "an RGD without the Flux substitution opt-out" \
  "Flux postBuild substitution must be disabled for every ResourceGraphDefinition"
restore_webapp

# Flux decrypts RGD definitions before apply too. Ciphertext in a nested template cannot be scanned
# or baselined as evidence for the plaintext workload that reaches KRO.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) =
  "ENC[AES256_GCM,data:AAAA,iv:BBBB,tag:CCCC,type:str]" |
  .sops = {"mac": "ENC[AES256_GCM,data:DDDD,iv:EEEE,tag:FFFF,type:str]", "version": "3.9.4"}' \
  "$WORK/$WEBAPP_RGD"
expect_rejected "a SOPS-encrypted ResourceGraphDefinition template" \
  "SOPS-encrypted ResourceGraphDefinitions cannot be validated from ciphertext"
restore_webapp

# Generated instances also feed Flux postBuild substitution. Security-sensitive namespace and image
# inputs must not carry expressions that are replaced after their raw evidence is sealed.
# shellcheck disable=SC2016 # the Flux substitution expression must remain literal in the fixture
yq -i '.spec.name = "${NAMESPACE}"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a generated instance with a substitutable namespace" \
  "generated instance must not contain Flux substitution expressions" "spec.name"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"
# shellcheck disable=SC2016 # the Flux substitution expression must remain literal in the fixture
yq -i '.spec.image = "${IMAGE}"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a generated instance with a substitutable image" \
  "generated instance must not contain Flux substitution expressions" "spec.image"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Every generated-spec value feeds graph behavior after the raw instance hash is sealed. Tenant's
# externalDns flag creates cluster-scoped bindings, so field-specific name/image checks are not
# sufficient.
# shellcheck disable=SC2016 # the Flux default-value expression must remain literal in the fixture
yq -i '.spec.externalDns = "${ENABLE_EXTERNAL_DNS:=true}"' \
  "$WORK/k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml"
expect_rejected "a generated Tenant with a substitutable graph option" \
  "generated instance must not contain Flux substitution expressions" "spec.externalDns"
cp "$REPO_ROOT/k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml" \
  "$WORK/k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml"

# A scalar spec has no descendant path for jq paths(scalars), but Flux can replace the scalar with a
# complete mapping after the instance evidence is sealed.
# shellcheck disable=SC2016 # the Flux whole-spec expression must remain literal in the fixture
yq -i '.spec = "${INSTANCE_SPEC}"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a generated instance with a substitutable scalar spec" \
  "generated instance must not contain Flux substitution expressions" "(spec)"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Flux decrypts SOPS resources before apply. Ciphertext cannot serve as namespace, registry, or
# complete-instance evidence for the plaintext generated instance that reaches the cluster.
yq -i '.spec.name = "ENC[AES256_GCM,data:AAAA,iv:BBBB,tag:CCCC,type:str]" |
  .spec.image = "ENC[AES256_GCM,data:DDDD,iv:EEEE,tag:FFFF,type:str]" |
  .sops = {"mac": "ENC[AES256_GCM,data:GGGG,iv:HHHH,tag:IIII,type:str]", "version": "3.9.4"}' \
  "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a SOPS-encrypted generated WebApp instance" \
  "SOPS-encrypted generated instances cannot be validated from ciphertext"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Flux postBuild substitution envsubsts the whole manifest, so metadata carries the same risk as the
# spec. metadata.namespace decides which namespace the generated graph lands in, and a spec-scoped
# scan reads a substitutable one as a fixed literal.
# shellcheck disable=SC2016 # the Flux substitution expression must remain literal in the fixture
yq -i '.metadata.namespace = "${TARGET_NAMESPACE}"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a generated instance with a substitutable metadata namespace" \
  "generated instance must not contain Flux substitution expressions" "metadata.namespace"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# SOPS encrypted_regex can cover any key, so ciphertext outside the spec is just as unreadable. An
# instance whose metadata is encrypted but which carries no sops block is not reviewable evidence.
yq -i '.metadata.annotations."platform.devantler.tech/owner" =
  "ENC[AES256_GCM,data:AAAA,iv:BBBB,tag:CCCC,type:str]"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a generated instance with ciphertext outside its spec" \
  "SOPS-encrypted generated instances cannot be validated from ciphertext"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

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

# Kubernetes List items may themselves be Flux Kustomizations. The post-render patch scan must use
# the same recursive List traversal as protected resource discovery.
readonly FLUX_RGD_LIST_PATCH="k8s/clusters/prod/flux-kustomization-rgd-list-patch-probe.yaml"
yq -n '.apiVersion = "v1" | .kind = "List" | .items = [{
    "apiVersion": "kustomize.toolkit.fluxcd.io/v1",
    "kind": "Kustomization",
    "metadata": {"name": "rgd-list-patch-probe", "namespace": "flux-system"},
    "spec": {
      "path": "./k8s/providers/hetzner",
      "patches": [{
        "target": {"group": "kro.run", "version": "v1alpha1", "kind": "ResourceGraphDefinition"},
        "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
      }]
    }
  }]' >"$WORK/$FLUX_RGD_LIST_PATCH"
expect_rejected "a Flux Kustomization nested in a List and targeting an RGD" \
  "Flux Kustomization targets or ambiguously selects" "ResourceGraphDefinition"
rm -f "$WORK/$FLUX_RGD_LIST_PATCH"

# Selectively SOPS-encrypted Flux selectors are decrypted before reconciliation, so ciphertext cannot
# be compared as if it were the deployed patch target.
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "rgd-encrypted-patch-probe" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.patches = [{
    "target": {"group": "kro.run", "version": "v1alpha1", "kind": "ENC[AES256_GCM,data:AAAA,iv:BBBB,tag:CCCC,type:str]"},
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }] |
  .sops = {"mac": "ENC[AES256_GCM,data:DDDD,iv:EEEE,tag:FFFF,type:str]", "version": "3.9.4"}' \
  >"$WORK/$FLUX_RGD_PATCH"
expect_rejected "a SOPS-encrypted Flux Kustomization selector" \
  "SOPS-encrypted Flux Kustomizations cannot be validated from ciphertext"
rm -f "$WORK/$FLUX_RGD_PATCH"

# Flux commonMetadata is also applied after the Kustomize build and can erase the resource-level
# substitution opt-out on every RGD in the deployment.
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "rgd-metadata-probe" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.commonMetadata.annotations."kustomize.toolkit.fluxcd.io/substitute" = "disabled"' \
  >"$WORK/$FLUX_RGD_PATCH"
"$GATE" "$WORK" || fail "the gate rejected a Flux commonMetadata substitution opt-out"
echo "PASS(probe): Flux commonMetadata may preserve the substitution opt-out."
rm -f "$WORK/$FLUX_RGD_PATCH"

yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "rgd-metadata-probe" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.commonMetadata.annotations."kustomize.toolkit.fluxcd.io/substitute" = "enabled"' \
  >"$WORK/$FLUX_RGD_PATCH"
expect_rejected "a Flux commonMetadata override of the substitution opt-out" \
  "Flux Kustomization must not override the substitution opt-out"
rm -f "$WORK/$FLUX_RGD_PATCH"

# Flux strategic-merge patches may omit target and identify the resource in the patch body itself.
yq -n '.apiVersion = "kustomize.toolkit.fluxcd.io/v1" |
  .kind = "Kustomization" | .metadata.name = "rgd-targetless-patch-probe" |
  .metadata.namespace = "flux-system" | .spec.path = "./k8s/providers/hetzner" |
  .spec.patches = [{"patch": "apiVersion: kro.run/v1alpha1\nkind: ResourceGraphDefinition\nmetadata:\n  name: webapp.kro.run\nspec:\n  resources: []"}]' \
  >"$WORK/$FLUX_RGD_PATCH"
expect_rejected "a targetless Flux strategic-merge patch declaring an RGD" \
  "Flux Kustomization targetless patch declares an RGD definition or generated instance" \
  "ResourceGraphDefinition"
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
