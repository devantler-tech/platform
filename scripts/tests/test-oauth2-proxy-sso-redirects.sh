#!/usr/bin/env bash
# Guards the oauth2-proxy login round-trip on every host it fronts.
#
# oauth2-proxy finishes a login at /oauth2/callback on the host the user started
# from, then returns them to the page they asked for as a path on that host, so
# it must not be given a fixed redirect_url. Dex only accepts a
# callback it has registered exactly, so every host an HTTPRoute sends to
# oauth2-proxy needs `https://<host>/oauth2/callback` on Dex's public-client.
# Without it, logging in on that host stops at Dex with "Unregistered
# redirect_uri". The reverse also holds: an entry whose route is gone is a
# callback Dex keeps accepting for no reason, so it must be removed with the
# route.
#
# Hosts are read from the rendered provider overlays, the trees Flux applies,
# not from raw files: a route a provider does not include needs no callback,
# and a route only a provider patch adds still does. The route oauth2-proxy's
# own chart renders is not in those trees, so its hostname is read from the
# HelmRelease values instead.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly repo_root

readonly providers=(hetzner docker)
readonly layers=(infrastructure/controllers infrastructure apps)
readonly dex_file='k8s/bases/infrastructure/controllers/dex/helm-release.yaml'

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

fail() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

# Every HTTPRoute that sends traffic to the oauth2-proxy Service. A backendRef
# with no namespace means the route's own namespace, and one with no kind means
# a core Service, per the Gateway API defaults. `$ns` is a yq variable, not a
# shell one.
# shellcheck disable=SC2016
readonly fronted_routes='
  select(.kind == "HTTPRoute")
  | .metadata.namespace as $ns
  | select(
      [ .spec.rules[]?.backendRefs[]?
        | select(
            (.group // "") == ""
            and (.kind // "Service") == "Service"
            and .name == "oauth2-proxy"
            and (.namespace // $ns) == "oauth2-proxy")
      ] | length > 0)'

hosts_of() {
  yq eval -N "${fronted_routes} | .spec.hostnames[]" "$1" | sort -u
}

# --- Self-test the route selector ------------------------------------------
# A selector that matches nothing would make every check below pass over an
# empty set, so prove it picks exactly the routes it should on a fixture first.
cat >"${workdir}/fixture.yaml" <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: fronted, namespace: app}
spec:
  hostnames: [fronted.example]
  rules:
    - backendRefs: [{name: oauth2-proxy, namespace: oauth2-proxy, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: same-namespace, namespace: oauth2-proxy}
spec:
  hostnames: [same-namespace.example]
  rules:
    - backendRefs: [{name: oauth2-proxy, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: direct, namespace: app}
spec:
  hostnames: [direct.example]
  rules:
    - backendRefs: [{name: app, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: other-namespace, namespace: app}
spec:
  hostnames: [other-namespace.example]
  rules:
    - backendRefs: [{name: oauth2-proxy, port: 80}]
EOF
fixture_hosts="$(hosts_of "${workdir}/fixture.yaml" | tr '\n' ' ')"
[[ "${fixture_hosts}" == 'fronted.example same-namespace.example ' ]] ||
  fail "the oauth2-proxy route selector picked '${fixture_hosts}' from its fixture instead of 'fronted.example same-namespace.example'; fix the selector before trusting this test"

# --- Render every layer Flux reconciles --------------------------------------
all_hosts="${workdir}/all-hosts.txt"
: >"${all_hosts}"

for provider in "${providers[@]}"; do
  rendered="${workdir}/${provider}.yaml"
  : >"${rendered}"
  for layer in "${layers[@]}"; do
    dir="${repo_root}/k8s/providers/${provider}/${layer}"
    {
      kubectl kustomize "${dir}"
      printf '\n---\n'
    } >>"${rendered}" 2>"${workdir}/render.err" ||
      fail "k8s/providers/${provider}/${layer} failed to build: $(tail -5 "${workdir}/render.err")"
  done

  # A route that names no hostname inherits the listener's, so this test cannot
  # tell which callback it needs. Refuse it rather than skip it.
  unnamed="$(yq eval -N "${fronted_routes} | select((.spec.hostnames // []) | length == 0) | .metadata.namespace + \"/\" + .metadata.name" "${rendered}")"
  [[ -z "${unnamed}" ]] ||
    fail "${provider}: these HTTPRoutes send traffic to oauth2-proxy without naming a hostname, so their login callback cannot be checked: ${unnamed//$'\n'/, }"

  hosts="$(hosts_of "${rendered}")"
  if [[ "${provider}" == hetzner && -z "${hosts}" ]]; then
    fail 'the production overlays put no host behind oauth2-proxy; either the SSO wiring changed shape or this test stopped seeing it'
  fi

  # oauth2-proxy's own route is rendered by its chart, so read its hostname from
  # the HelmRelease values.
  own_hosts="$(yq eval -N 'select(.kind == "HelmRelease" and .metadata.name == "oauth2-proxy" and .metadata.namespace == "oauth2-proxy") | .spec.values.gatewayApi.hostnames[]' "${rendered}")"
  [[ -n "${own_hosts}" ]] ||
    fail "${provider}: the oauth2-proxy HelmRelease names no gatewayApi hostname; its own callback host can no longer be derived"

  # oauth2-proxy must finish each login on the host it started from. A fixed
  # redirect_url brings every login back through one host, where the path
  # oauth2-proxy returns the user to belongs to no app. The callback it derives
  # instead is https — the scheme Dex registers — only while cookie_secure is set.
  o2p="$(yq eval -N 'select(.kind == "HelmRelease" and .metadata.name == "oauth2-proxy" and .metadata.namespace == "oauth2-proxy") | .spec.values' "${rendered}")"
  o2p_config="$(yq eval -N '.config.configFile // ""' - <<<"${o2p}")"
  [[ -n "${o2p_config}" ]] ||
    fail "${provider}: the oauth2-proxy HelmRelease has no config.configFile; its login settings moved, so this test is checking nothing"
  if grep -Eq '^[[:space:]]*redirect_url[[:space:]]*=' <<<"${o2p_config}" ||
    grep -Eq 'redirect[-_]url' <<<"$(yq eval -N '.extraArgs // ""' - <<<"${o2p}")"; then
    fail "${provider}: oauth2-proxy sets a fixed redirect_url, so every login finishes on one host and returns the user to a path on a host that serves no app. Remove it from k8s/bases/infrastructure/controllers/oauth2-proxy/helm-release.yaml; oauth2-proxy then finishes each login on the host it started from"
  fi
  grep -Eq '^[[:space:]]*cookie_secure[[:space:]]*=[[:space:]]*true[[:space:]]*$' <<<"${o2p_config}" ||
    fail "${provider}: oauth2-proxy must set cookie_secure = true: it is what makes the login callback it derives https, the only scheme Dex has registered"

  registered="$(yq eval -N 'select(.kind == "HelmRelease" and .metadata.name == "dex" and .metadata.namespace == "dex") | .spec.values.config.staticClients[] | select(.id == "public-client") | .redirectURIs[]' "${rendered}")"
  [[ -n "${registered}" ]] ||
    fail "${provider}: no Dex public-client redirectURIs were rendered; the Dex client moved or was renamed, so this test is checking nothing"

  missing=()
  while IFS= read -r host; do
    [[ -n "${host}" ]] || continue
    grep -qxF -- "https://${host}/oauth2/callback" <<<"${registered}" ||
      missing+=("https://${host}/oauth2/callback")
  done < <(printf '%s\n%s\n' "${hosts}" "${own_hosts}" | sort -u)

  if ((${#missing[@]} > 0)); then
    fail "${provider}: these hosts are behind oauth2-proxy but Dex does not accept their login callback, so signing in there fails with 'Unregistered redirect_uri'. Add each to the public-client redirectURIs in ${dex_file}: ${missing[*]}"
  fi

  printf '%s\n%s\n' "${hosts}" "${own_hosts}" >>"${all_hosts}"
  printf '%s\n' "${registered}" >"${workdir}/${provider}-registered.txt"
done

# --- No callback outlives its route ----------------------------------------
# The Dex client is shared by both providers, so an entry is live while any
# provider still fronts its host.
sort -u -o "${all_hosts}" "${all_hosts}"
stale=()
while IFS= read -r uri; do
  [[ "${uri}" =~ ^https://([^/]+)/oauth2/callback$ ]] || continue
  grep -qxF -- "${BASH_REMATCH[1]}" "${all_hosts}" || stale+=("${uri}")
done < <(sort -u "${workdir}"/*-registered.txt)

if ((${#stale[@]} > 0)); then
  fail "Dex still accepts an oauth2-proxy login callback on hosts no HTTPRoute sends to oauth2-proxy. Remove each from the public-client redirectURIs in ${dex_file}: ${stale[*]}"
fi

printf 'PASS: oauth2-proxy finishes each login on the host it started from, and Dex accepts that callback on all %d hosts it fronts and on no other\n' \
  "$(grep -c . "${all_hosts}")"
