#!/usr/bin/env bash
# The only dynamic source profile admitted by the generic Tenant (ADR 0019).
# This reads the exact constructor and its complete schema input domain; it never
# evaluates arbitrary CEL.
#
# A Tenant follows its own signed release tags: the source takes the newest stable
# release in `spec.source.version` and accepts only the tenant's own repository's
# release workflow on a release tag (ADR 0017, #6265). It never names the shared
# publisher, so a Tenant is not a shared-publisher consumer. That subject is built
# from the regex-escaped tenant name, so the printed default subject names it with
# the literal `<tenant>`, which is no repository name and matches no identity. The
# exact constructor below is what binds that segment to the tenant.
tenant_source_default_subject() (
  set -o pipefail
  local file="$1" observed="${2:-}" ref expected_subject default_subject source_type version_type lifecycle
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  ref='{"semver":"${schema.spec.source.version}","semverFilter":"^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"}'
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  expected_subject='${"^https://github\\.com/devantler-tech/" + schema.spec.name.replace(".", "\\.").replace("-", "\\-") + "/\\.github/workflows/[^/]+\\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$"}'
  default_subject='^https://github\.com/devantler-tech/<tenant>/\.github/workflows/[^/]+\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'
  source_type='TenantSource | default={}'
  version_type='string | default=">=0.0.0" minLength=1 maxLength=64'
  lifecycle='boolean | default=true validation="self || !oldSelf"'
  if [[ -n "$observed" && "$observed" != "$expected_subject" ]]; then
    printf 'invalid Tenant source constructor: unexpected subject\n' >&2
    return 1
  fi
  if ! yq -o=json -I=0 '.' "$file" | jq -se \
    --argjson ref "$ref" --arg subject "$expected_subject" --arg source_type "$source_type" \
    --arg version_type "$version_type" --arg lifecycle "$lifecycle" '
      [ .[] | select(.apiVersion == "kro.run/v1alpha1" and .kind == "ResourceGraphDefinition" and .metadata.name == "tenant.kro.run") ] as $graphs
      | ($graphs | length) == 1
      and ($graphs[0].spec.schema | .apiVersion == "v1alpha1" and .kind == "Tenant" and .scope == "Namespaced"
        and .types.TenantSource == {version: $version_type, reconcileApplication: $lifecycle}
        and .spec.source == $source_type and .spec.name == "string" and (.spec | has("reconcileApplication") | not))
      and ([$graphs[0].spec.resources[] | select(.id == "ociRepository")] | length) == 1
      and ([$graphs[0].spec.resources[] | select(.id == "ociRepository")][0].template |
        .apiVersion == "source.toolkit.fluxcd.io/v1" and .kind == "OCIRepository"
        and .metadata.name == "${schema.spec.name}" and .metadata.namespace == "${tenantNamespace.metadata.name}"
        and .spec.url == "oci://ghcr.io/devantler-tech/${schema.spec.name}/manifests"
        and .spec.ref == $ref and .spec.verify == {provider: "cosign", matchOIDCIdentity: [{
          issuer: "^https://token\\.actions\\.githubusercontent\\.com$", subject: $subject
        }]})
      and ([.[] | .. | objects | select(has("subject")) | select(.subject == $subject)] | length) == 1
    ' >/dev/null; then
    printf 'invalid Tenant source constructor: graph context, input domain or constructor differs\n' >&2
    return 1
  fi
  printf '%s\n' "$default_subject"
)
