#!/usr/bin/env bash
# The only dynamic source profile admitted by the generic Tenant (ADR 0019).
# This reads the exact constructor and its complete schema input domain; it never
# evaluates arbitrary CEL.
#
# A Tenant follows its own signed release tags: the source takes the newest stable
# release and accepts only the workflow the tenant releases from in its own
# repository, cd.yaml or business-site's publish-runtime.yaml, on a release tag
# (ADR 0017, #6265, #6306). It never names the shared
# publisher, so a Tenant is not a shared-publisher consumer. That subject is built
# from the regex-escaped tenant name, so the printed default subject names it with
# the literal `<tenant>`, which is no repository name and matches no identity. The
# exact constructor below is what binds that segment to the tenant.
tenant_source_default_subject() (
  set -o pipefail
  local file="$1" observed="${2:-}" ref expected_subject default_subject source_type lifecycle digest_type publisher_type name_type
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  ref='{"semver":">=0.0.0","semverFilter":"^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"}'
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  expected_subject='${"^https://github\\.com/devantler-tech/" + schema.spec.name.replace(".", "\\.").replace("-", "\\-") + "/\\.github/workflows/" + (schema.spec.name == "business-site" ? "publish-runtime" : "cd") + "\\.yaml@refs/tags/v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$"}'
  default_subject='^https://github\.com/devantler-tech/<tenant>/\.github/workflows/cd\.yaml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'
  # The retired pin fields stay in the schema only because KRO refuses an API update that removes
  # an installed field; the source validation refuses every non-empty value.
  source_type="TenantSource | default={} description=\"Follows signed stable releases. Legacy pin fields must be empty.\" validation=\"(!has(self.digest) || self.digest == '') && (!has(self.publisherRevision) || self.publisherRevision == '')\""
  digest_type='string | default="" maxLength=71 pattern="^$|^sha256:[0-9a-f]{64}$"'
  publisher_type='string | default="" maxLength=40 pattern="^$|^[0-9a-f]{40}$"'
  lifecycle='boolean | default=true validation="self || !oldSelf"'
  # The tenant name is validated by the graph itself (#6306).
  name_type='string | description="A DNS label: lowercase letters, digits and hyphens, at most 63 characters." validation="size(self) <= 63 && self.matches('"'"'^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'"'"')"'
  if [[ -n "$observed" && "$observed" != "$expected_subject" ]]; then
    printf 'invalid Tenant source constructor: unexpected subject\n' >&2
    return 1
  fi
  if ! yq -o=json -I=0 '.' "$file" | jq -se \
    --argjson ref "$ref" --arg subject "$expected_subject" --arg source_type "$source_type" \
    --arg lifecycle "$lifecycle" --arg name_type "$name_type" \
    --arg digest_type "$digest_type" --arg publisher_type "$publisher_type" '
      [ .[] | select(.apiVersion == "kro.run/v1alpha1" and .kind == "ResourceGraphDefinition" and .metadata.name == "tenant.kro.run") ] as $graphs
      | ($graphs | length) == 1
      and ($graphs[0].spec.schema | .apiVersion == "v1alpha1" and .kind == "Tenant" and .scope == "Namespaced"
        and .types.TenantSource == {digest: $digest_type, publisherRevision: $publisher_type, reconcileApplication: $lifecycle}
        and .spec.source == $source_type and .spec.name == $name_type and (.spec | has("reconcileApplication") | not))
      and ([$graphs[0].spec.resources[] | select(.id == "ociRepository")] | length) == 1
      and ([$graphs[0].spec.resources[] | select(.id == "ociRepository")][0].template |
        .apiVersion == "source.toolkit.fluxcd.io/v1" and .kind == "OCIRepository"
        and .metadata.name == "${schema.spec.name}" and .metadata.namespace == "${tenantNamespace.metadata.name}"
        and .spec.url == "oci://ghcr.io/devantler-tech/${schema.spec.name}/manifests"
        and .spec.ref == $ref and .spec.verify == {provider: "cosign", matchOIDCIdentity: [{
          issuer: "^https://token\\.actions\\.githubusercontent\\.com$", subject: $subject
        }]})
      and ([.[] | .. | objects | select(has("subject")) | select(.subject == $subject)] | length) == 1
      # The verified source must be the ONLY thing the graph can fetch and apply from. Every
      # child is one of the reviewed kinds; no other Flux source, HelmRelease or controller
      # template exists; and every sourceRef anywhere in the graph names the verified source.
      and ([$graphs[0].spec.resources[] | .template | {apiVersion, kind}] | all(. as $child | [
            {apiVersion: "v1", kind: "Namespace"}, {apiVersion: "v1", kind: "ServiceAccount"},
            {apiVersion: "v1", kind: "ResourceQuota"}, {apiVersion: "v1", kind: "LimitRange"},
            {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding"},
            {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding"},
            {apiVersion: "networking.k8s.io/v1", kind: "NetworkPolicy"},
            {apiVersion: "source.toolkit.fluxcd.io/v1", kind: "OCIRepository"},
            {apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization"}
          ] | index([$child]) != null))
      and ([$graphs[0].spec.resources[] | select(.template.apiVersion | tostring | startswith("source.toolkit.fluxcd.io/"))] | map(.id)) == ["ociRepository"]
      and ([$graphs[0].spec.resources[] | select(.template.kind == "Kustomization")] | length) >= 1
      and ([$graphs[0] | .. | objects | select(has("sourceRef") or has("chartRef"))]
        | all(has("chartRef") | not) and all(.sourceRef == {kind: "OCIRepository", name: "${ociRepository.metadata.name}"}))
    ' >/dev/null; then
    printf 'invalid Tenant source constructor: graph context, input domain or constructor differs\n' >&2
    return 1
  fi
  printf '%s\n' "$default_subject"
)
