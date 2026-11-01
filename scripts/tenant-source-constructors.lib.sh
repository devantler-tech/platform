#!/usr/bin/env bash
# The only dynamic publisher/selector profile admitted by the generic Tenant.
# This reads both exact constructors and their complete schema input domain;
# it never evaluates arbitrary CEL or grants a consumer publisher approval.
tenant_source_default_subject() (
  set -o pipefail
  local file="$1" observed="${2:-}" ref expected_subject default_subject source_type digest_type publisher_type lifecycle
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  ref='${schema.spec.source.digest == "" ? {"semver": ">=1.0.0"} : {"digest": schema.spec.source.digest}}'
  # shellcheck disable=SC2016 # Literal CEL, never shell expansion.
  expected_subject='${schema.spec.source.publisherRevision == "" ? "^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-app\\.yaml@[0-9a-f]{40}|\\.github/\\.github/workflows/publish-app\\.yaml@(467f07582436a26b62cf4aa756423512f85ba0c0|af042c540ce8db4d64add98c34b5c57691e0e812))$" : "^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-app\\.yaml@" + schema.spec.source.publisherRevision + "$"}'
  default_subject='^https://github\.com/devantler-tech/(actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}|\.github/\.github/workflows/publish-app\.yaml@(467f07582436a26b62cf4aa756423512f85ba0c0|af042c540ce8db4d64add98c34b5c57691e0e812))$'
  source_type="TenantSource | default={} validation=\"((!has(self.digest) || self.digest == '') == (!has(self.publisherRevision) || self.publisherRevision == '')) && (!has(self.reconcileApplication) || self.reconcileApplication || (has(self.digest) && self.digest != ''))\" validation=\"!has(oldSelf.digest) || oldSelf.digest == '' || (has(self.digest) && self.digest != '')\""
  digest_type='string | default="" maxLength=71 pattern="^$|^sha256:[0-9a-f]{64}$"'
  publisher_type='string | default="" maxLength=40 pattern="^$|^[0-9a-f]{40}$"'
  lifecycle='boolean | default=true validation="self || !oldSelf"'
  if [[ -n "$observed" && "$observed" != "$expected_subject" ]]; then
    printf 'invalid Tenant source constructor: unexpected subject\n' >&2
    return 1
  fi
  if ! yq -o=json -I=0 '.' "$file" | jq -se \
    --arg ref "$ref" --arg subject "$expected_subject" --arg source_type "$source_type" \
    --arg digest_type "$digest_type" --arg publisher_type "$publisher_type" --arg lifecycle "$lifecycle" '
      [ .[] | select(.apiVersion == "kro.run/v1alpha1" and .kind == "ResourceGraphDefinition" and .metadata.name == "tenant.kro.run") ] as $graphs
      | ($graphs | length) == 1
      and ($graphs[0].spec.schema | .apiVersion == "v1alpha1" and .kind == "Tenant" and .scope == "Namespaced"
        and .types.TenantSource == {digest: $digest_type, publisherRevision: $publisher_type, reconcileApplication: $lifecycle}
        and .spec.source == $source_type and (.spec | has("reconcileApplication") | not))
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
    printf 'invalid Tenant source constructor: graph context, paired domain or constructor differs\n' >&2
    return 1
  fi
  printf '%s\n' "$default_subject"
)
