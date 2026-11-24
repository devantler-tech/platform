#!/usr/bin/env bash
# Prove the GitHub token broker's caller gate (devantler-tech/monorepo ADR 0020)
# admits only allowed callers, and that nothing reaches octo-sts around it.
#
# 1. Static wiring: the Service and the network policy reach only the gate's
#    port, the gate forwards to octo-sts on the pod's loopback, GitHub's keys
#    are fetched over verified TLS, and the set of allowed callers is exactly
#    the devantler-tech organization.
# 2. Behaviour: runs the shipped config in the Deployment's own pinned Envoy,
#    with GitHub's key fetch replaced by a local test key and octo-sts by a
#    fixed "admitted" reply, then sends signed test tokens. Each way a caller
#    could get past the gate is refused, refusals happen before the rate
#    limit, and the rate limit stops an admitted caller in a loop.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="${root}/k8s/providers/hetzner/apps/github-broker"
deployment="${dir}/server/deployment.yaml"
# Under TMPDIR, so a local Docker VM that shares only the home folder can mount it.
work="$(mktemp -d "${TMPDIR:-/tmp}/github-broker-gate.XXXXXX")"
container=''
completed=0
# Stop Envoy, remove the work directory, and fail any exit that did not reach
# the final PASS.
cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm -f "${container}" >/dev/null 2>&1 || true
  fi
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: github-broker gate: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: github-broker gate: %s\n' "$1" >&2
  exit 1
}
for tool in curl docker jq openssl xxd yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

readonly issuer=https://token.actions.githubusercontent.com
readonly org_id=178524219

# --- 1. Static wiring -------------------------------------------------------

yq '.data["envoy.yaml"]' "${dir}/server/config-map-gate.yaml" >"${work}/shipped.yaml"
hcm='.static_resources.listeners[0].filter_chains[0].filters[0].typed_config'
filters="${hcm}.http_filters"

image="$(yq '.spec.template.spec.containers[] | select(.name == "gate") | .image' "${deployment}")"
[[ "${image}" == docker.io/envoyproxy/envoy:*@sha256:* ]] || fail "unexpected gate image: ${image}"
args="$(yq -o=json -I=0 '.spec.template.spec.containers[] | select(.name == "gate") | .args' "${deployment}")"
[[ "${args}" == '["--config-path","/etc/github-broker/gate/envoy.yaml","--log-level","warn"]' ]] ||
  fail "unexpected gate args: ${args}"
mount="$(yq '.spec.template.spec.containers[] | select(.name == "gate") | .volumeMounts[] | select(.mountPath == "/etc/github-broker/gate") | .name' "${deployment}")"
[[ "$(yq ".spec.template.spec.volumes[] | select(.name == \"${mount}\") | .configMap.name" "${deployment}")" == github-broker-gate ]] ||
  fail 'the gate does not mount the github-broker-gate ConfigMap'

gate_port="$(yq '.spec.template.spec.containers[] | select(.name == "gate") | .ports[] | select(.name == "gate") | .containerPort' "${deployment}")"
listener_port="$(yq '.static_resources.listeners[0].address.socket_address.port_value' "${work}/shipped.yaml")"
[[ "${gate_port}" == 8081 && "${listener_port}" == 8081 ]] ||
  fail "the gate container port (${gate_port}) and Envoy listener (${listener_port}) must both be 8081"
[[ "$(yq '.static_resources.listeners | length' "${work}/shipped.yaml")" == 1 ]] || fail 'the gate must have exactly one listener'
[[ "$(yq '.spec.ports | length' "${dir}/service.yaml")" == 1 && "$(yq '.spec.ports[0].targetPort' "${dir}/service.yaml")" == gate ]] ||
  fail 'the Service must target only the gate port'
ingress_ports="$(yq -o=json -I=0 '[.spec.ingress[].toPorts[].ports[].port]' "${dir}/cilium-network-policy.yaml")"
[[ "${ingress_ports}" == '["8081"]' ]] || fail "the network policy must admit only the gate's port, not ${ingress_ports}"
octo_port="$(yq '.spec.template.spec.containers[] | select(.name == "octo-sts") | .env[] | select(.name == "PORT") | .value' "${deployment}")"
upstream="$(yq -o=json -I=0 '.static_resources.clusters[] | select(.name == "octo_sts") | .load_assignment.endpoints[0].lb_endpoints[0].endpoint.address.socket_address' "${work}/shipped.yaml")"
[[ "${upstream}" == "{\"address\":\"127.0.0.1\",\"port_value\":${octo_port}}" ]] ||
  fail "the gate must forward to octo-sts (port ${octo_port}) on loopback, not ${upstream}"

[[ "$(yq -o=json -I=0 "[${filters}[].name]" "${work}/shipped.yaml")" == '["envoy.filters.http.jwt_authn","envoy.filters.http.rbac","envoy.filters.http.local_ratelimit","envoy.filters.http.router"]' ]] ||
  fail 'the filters must run in the order: verify the token, check the caller, rate-limit, route'

provider="${filters}[] | select(.name == \"envoy.filters.http.jwt_authn\") | .typed_config"
[[ "$(yq "${provider} | .providers | keys | join(\",\")" "${work}/shipped.yaml")" == github_actions ]] ||
  fail 'the gate must trust exactly one token issuer, GitHub Actions'
[[ "$(yq "${provider} | .providers.github_actions.issuer" "${work}/shipped.yaml")" == "${issuer}" ]] || fail 'the issuer is not GitHub Actions'
# shellcheck disable=SC2016 # the literal Flux variable, substituted at deploy time.
[[ "$(yq -o=json -I=0 "${provider} | .providers.github_actions.audiences" "${work}/shipped.yaml")" == '["sts.${domain}"]' ]] ||
  fail "the audience must be exactly the broker's own hostname"
[[ "$(yq "${provider} | .providers.github_actions.forward" "${work}/shipped.yaml")" == true ]] ||
  fail 'the gate must forward the token, so octo-sts verifies it again'
[[ "$(yq -o=json -I=0 "${provider} | .rules" "${work}/shipped.yaml")" == '[{"match":{"prefix":"/"},"requires":{"provider_name":"github_actions"}}]' ]] ||
  fail 'every path must require a verified token'
[[ "$(yq "${provider} | .providers.github_actions.remote_jwks.http_uri.uri" "${work}/shipped.yaml")" == "${issuer}/.well-known/jwks" ]] ||
  fail "the gate must fetch GitHub's published signing keys"
tls='.static_resources.clusters[] | select(.name == "github_oidc") | .transport_socket.typed_config'
[[ "$(yq "${tls} | .sni" "${work}/shipped.yaml")" == token.actions.githubusercontent.com &&
  "$(yq "${tls} | .common_tls_context.validation_context.trusted_ca.filename" "${work}/shipped.yaml")" == /etc/ssl/certs/ca-certificates.crt &&
  "$(yq -o=json -I=0 "${tls} | .common_tls_context.validation_context.match_typed_subject_alt_names" "${work}/shipped.yaml")" == '[{"san_type":"DNS","matcher":{"exact":"token.actions.githubusercontent.com"}}]' ]] ||
  fail "GitHub's signing keys must be fetched over TLS verified for token.actions.githubusercontent.com"

rbac="${filters}[] | select(.name == \"envoy.filters.http.rbac\") | .typed_config.rules"
[[ "$(yq "${rbac} | .action" "${work}/shipped.yaml")" == ALLOW ]] || fail 'the caller check must be an allow list'
# The full set of allowed callers. Adding one changes this line on purpose.
allowed="$(yq -o=json -I=0 "[${rbac} | .policies | to_entries[] | {\"policy\": .key, \"principals\": .value.principals}]" "${work}/shipped.yaml")"
[[ "${allowed}" == "[{\"policy\":\"devantler-tech-workflows\",\"principals\":[{\"sourced_metadata\":{\"metadata_source\":\"DYNAMIC\",\"metadata_matcher\":{\"filter\":\"envoy.filters.http.jwt_authn\",\"path\":[{\"key\":\"github_actions\"},{\"key\":\"repository_owner_id\"}],\"value\":{\"string_match\":{\"exact\":\"${org_id}\"}}}}}]}]" ]] ||
  fail "the allowed callers changed: ${allowed}"

# --- 2. Behaviour -------------------------------------------------------------

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
openssl genrsa -out "${work}/key.pem" 2048 2>/dev/null
openssl genrsa -out "${work}/other-key.pem" 2048 2>/dev/null
modulus="$(openssl rsa -in "${work}/key.pem" -noout -modulus | cut -d= -f2 | xxd -r -p | b64url)"
jwks="$(jq -cn --arg n "${modulus}" '{keys: [{kty: "RSA", alg: "RS256", use: "sig", kid: "test", n: $n, e: "AQAB"}]}')"

# token <key> <claims json>: a signed RS256 JWT with the test key id.
token() {
  local header claims signature
  header="$(printf '%s' '{"alg":"RS256","typ":"JWT","kid":"test"}' | b64url)"
  claims="$(printf '%s' "$2" | b64url)"
  signature="$(printf '%s' "${header}.${claims}" | openssl dgst -sha256 -sign "$1" | b64url)"
  printf '%s.%s.%s' "${header}" "${claims}" "${signature}"
}
now="$(date +%s)"
# claims <jq filter applied to a valid devantler-tech claim set>
claims() {
  jq -cn --arg iss "${issuer}" --arg owner "${org_id}" --argjson now "${now}" \
    "{iss: \$iss, aud: \"sts.example.test\", sub: \"repo:devantler-tech/platform:ref:refs/heads/main\", repository_owner_id: \$owner, iat: \$now, nbf: \$now, exp: (\$now + 300)} | $1"
}

mkdir -p "${work}/config"
# shellcheck disable=SC2016 # ${domain} is the literal Flux variable the test fills in.
JWKS="${jwks}" yq "
  (${provider} | .providers.github_actions) |= (del(.remote_jwks) | .local_jwks = {\"inline_string\": strenv(JWKS)}) |
  (${hcm}.route_config.virtual_hosts[0].routes[] | select(.match.path == \"/sts/exchange\")) |=
    (del(.route) | .direct_response = {\"status\": 200, \"body\": {\"inline_string\": \"admitted\"}}) |
  del(.static_resources.clusters[] | select(.name == \"github_oidc\"))
" "${work}/shipped.yaml" | sed 's/\${domain}/example.test/g' >"${work}/config/envoy.yaml"
grep -q 'local_jwks' "${work}/config/envoy.yaml" || fail 'the test config did not swap in the local key'
chmod -R a+rX "${work}/config"

container="$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges -p 127.0.0.1::8081 \
  --mount "type=bind,src=${work}/config,dst=/etc/github-broker/gate,readonly" \
  "${image}" --config-path /etc/github-broker/gate/envoy.yaml --log-level warn)"
address="$(docker port "${container}" 8081/tcp | head -n 1)"
[[ -n "${address}" ]] || fail 'Envoy published no port'
base="http://${address}"
for _ in $(seq 1 60); do
  curl -s -o /dev/null "${base}/" && break
  sleep 0.5
done
curl -s -o /dev/null "${base}/" || fail "Envoy did not start: $(docker logs "${container}" 2>&1 | tail -c 800)"

exchange='/sts/exchange?scope=devantler-tech/platform&identity=test'
# expect <status> <reason> <message> <path> [curl args...]: the response has that
# status, and its body names that reason (empty: any), so two refusals with the
# same status are told apart by the rule that refused them.
expect() {
  local want="$1" reason="$2" message="$3" path="$4" got
  shift 4
  got="$(curl -s -o "${work}/body" -w '%{http_code}' "$@" "${base}${path}")"
  [[ "${got}" == "${want}" ]] || fail "${message}: expected HTTP ${want}, got ${got} ($(head -c 200 "${work}/body"))"
  [[ -z "${reason}" ]] || grep -Fq "${reason}" "${work}/body" || fail "${message}: refused for another reason: $(head -c 200 "${work}/body")"
}
bearer() { printf 'Authorization: Bearer %s' "$(token "${2:-${work}/key.pem}" "$(claims "$1")")"; }

expect 200 admitted 'a devantler-tech workflow' "${exchange}" -H "$(bearer .)"
[[ "$(cat "${work}/body")" == admitted ]] || fail 'the admitted request did not reach the upstream'
expect 200 admitted 'a doubled slash still matches only after normalizing' '/sts//exchange?identity=test' -H "$(bearer .)"

expect 401 'Jwt is missing' 'no token' "${exchange}"
expect 401 'Jwt is missing' 'a token in the query string instead of the header' "${exchange}&access_token=$(token "${work}/key.pem" "$(claims .)")"
expect 401 'Jwt verification fails' 'a token signed by another key' "${exchange}" -H "$(bearer . "${work}/other-key.pem")"
expect 401 'Jwt issuer is not configured' 'another issuer' "${exchange}" -H "$(bearer '.iss = "https://issuer.example.com"')"
expect 403 'Audiences in Jwt are not allowed' 'another audience' "${exchange}" -H "$(bearer '.aud = "sts.other.example"')"
# shellcheck disable=SC2016 # $now is a jq variable, not a shell one.
expect 401 'Jwt is expired' 'an expired token' "${exchange}" -H "$(bearer '.exp = ($now - 3600) | .iat = ($now - 7200) | .nbf = ($now - 7200)')"
expect 401 'Jwt verification fails' 'a token without an expiry' "${exchange}" -H "$(bearer 'del(.exp)')"
expect 403 'RBAC: access denied' 'another organization' "${exchange}" -H "$(bearer '.repository_owner_id = "1"')"
expect 403 'RBAC: access denied' 'no organization claim' "${exchange}" -H "$(bearer 'del(.repository_owner_id)')"
expect 403 'RBAC: access denied' 'the organization ID as a number, not a string' "${exchange}" -H "$(bearer ".repository_owner_id = ${org_id}")"
expect 403 'RBAC: access denied' 'an organization ID with a suffix' "${exchange}" -H "$(bearer ".repository_owner_id = \"${org_id}0\"")"
expect 404 '' 'an admitted caller on any other path' '/' -H "$(bearer .)"
expect 400 '' 'an escaped slash' '/sts%2Fexchange' -H "$(bearer .)"

# The admitted budget runs out within the minute, and refusals neither spend it
# nor get past it: they are refused before the rate limit.
limited=0
for _ in $(seq 1 70); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "$(bearer .)" "${base}${exchange}")"
  if [[ "${code}" == 429 ]]; then
    limited=1
    break
  fi
  [[ "${code}" == 200 ]] || fail "an admitted request got HTTP ${code} before the limit"
done
[[ "${limited}" == 1 ]] || fail 'an admitted caller in a loop was never rate-limited'
expect 403 'RBAC: access denied' 'another organization, after the budget is spent' "${exchange}" -H "$(bearer '.repository_owner_id = "1"')"
expect 401 'Jwt is missing' 'no token, after the budget is spent' "${exchange}"

grep -Fq 'bash scripts/tests/test-github-broker-gate.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'github-broker gate: PASS\n'
