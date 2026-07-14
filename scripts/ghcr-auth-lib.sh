#!/usr/bin/env bash
# Shared secret-safe helpers for the Git/SOPS GHCR pull credential.

# Decrypt only the Docker config scalar into a caller-owned restricted file.
decrypt_flux_ghcr_docker_config() {
  local output_file="$1"
  local secret_file="${2:-k8s/bases/bootstrap/secret.enc.yaml}"

  ksail workload cipher decrypt \
    "${secret_file}" \
    --extract '["stringData"]["ghcr_dockerconfigjson"]' \
    --output "${output_file}" \
    >/dev/null
  chmod 600 "${output_file}"
}

# Validate the Docker config and write canonical username/password JSON.
write_flux_ghcr_credentials() {
  local docker_config="$1"
  local output_file="$2"

  if ! jq -e '
    def non_empty_string: type == "string" and length > 0;
    (.auths["ghcr.io"] // {}) as $auth
    | ((($auth | has("username")) or ($auth | has("password"))))
        as $explicit_present
    | (($auth.username | non_empty_string)
        and ($auth.password | non_empty_string)) as $explicit_valid
    | ($auth | has("auth")) as $encoded_present
    | (if $encoded_present then
        try (
          $auth.auth
          | @base64d
          | capture("^(?<username>[^:]+):(?<password>.+)$")
        ) catch null
      else null end) as $decoded
    | (($decoded != null)
        and ($decoded.username | non_empty_string)
        and ($decoded.password | non_empty_string)) as $encoded_valid
    | select(
        ((($explicit_present | not) or $explicit_valid)
          and (($encoded_present | not) or $encoded_valid)
          and ($explicit_valid or $encoded_valid)
          and (((($explicit_present and $encoded_present) | not))
            or (($auth.username == $decoded.username)
              and ($auth.password == $decoded.password))))
      )
    | if $explicit_valid then
        {username: $auth.username, password: $auth.password}
      else
        {username: $decoded.username, password: $decoded.password}
      end
  ' "${docker_config}" > "${output_file}"; then
    echo "::error::The SOPS GHCR pull credential is not a valid Docker config with non-empty, consistent ghcr.io username/password and auth fields."
    return 1
  fi
  chmod 600 "${output_file}"
}

# Print a non-secret ciphertext revision for redaction-resistant drift detection.
flux_ghcr_revision() {
  local secret_file="${1:-k8s/bases/bootstrap/secret.enc.yaml}"

  # Hash the committed SOPS ciphertext, not the decrypted credential. This
  # changes when the pull credential is rotated without publishing a stable
  # verifier for the token itself in the Kubernetes Node annotation.
  yq -er '
    .stringData.ghcr_dockerconfigjson
    | select(tag == "!!str" and length > 0 and test("^ENC\\["))
  ' "${secret_file}" \
    | shasum -a 256 \
    | awk '{print $1}'
}
