#!/usr/bin/env bash
# Guards the oauth2-proxy login round-trip on every host it fronts.
#
# oauth2-proxy finishes a login at /oauth2/callback on the host the user started
# from, then returns them to the page they asked for as a path on that host. So
# oauth2-proxy must not be given a fixed redirect_url, and no route may replace
# that page with a fixed X-Auth-Request-Redirect. Dex only accepts a
# callback it has registered exactly, so every host an HTTPRoute sends to
# oauth2-proxy needs both exact HTTPS default-port forms on Dex's public-client:
# `https://<host>/oauth2/callback` and `https://<host>:443/oauth2/callback`.
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
    # Check the build on its own: grouped with the separator, the group's
    # status would be printf's, and a layer that failed to build would be
    # read as one with no routes.
    kubectl kustomize "${dir}" >>"${rendered}" 2>"${workdir}/render.err" ||
      fail "k8s/providers/${provider}/${layer} failed to build: $(tail -5 "${workdir}/render.err")"
    printf '\n---\n' >>"${rendered}"
  done

  # The private preview has its own listener and can reach only the sign-in proxy.
  yq eval -N -o=json '.' "${rendered}" | jq -es --arg provider "${provider}" '
    def host: "dev.devantler.tech";
    def listener: "https-dev-devantler-tech";
    def tlsSecret: "business-site-preview-tls";
    [.[] | select(.kind == "HTTPRoute" and any(.spec.hostnames[]?; . == host))] as $routes |
    [.[] | select(.kind == "Gateway") | .metadata as $gateway |
      .spec.listeners[]? | select(.hostname == host) | {gateway:$gateway, listener:.}] as $listeners |
    if $provider == "hetzner" then
      ($routes | length == 1) and ($listeners | length == 1) and
      ($routes[0] | .metadata.namespace == "oauth2-proxy" and .spec.hostnames == [host] and
        .spec.parentRefs == [{name:"platform", namespace:"kube-system", sectionName:listener}] and
        (.spec.rules | length == 1) and .spec.rules[0].backendRefs == [{name:"oauth2-proxy", port:80}] and
        .spec.rules[0].filters == [{type:"ResponseHeaderModifier", responseHeaderModifier:{set:[
          {name:"Strict-Transport-Security", value:"max-age=63072000; includeSubDomains; preload"}]}}]) and
      ($listeners[0].gateway | .name == "platform" and .namespace == "kube-system") and
      ($listeners[0].listener | .name == listener and .protocol == "HTTPS" and .port == 443 and
        .tls == {mode:"Terminate", certificateRefs:[{name:tlsSecret}]} and
        .allowedRoutes == {namespaces:{from:"Selector", selector:{matchLabels:{"kubernetes.io/metadata.name":"oauth2-proxy"}}},
          kinds:[{group:"gateway.networking.k8s.io", kind:"HTTPRoute"}]}) and
      ([.[] | select(.kind == "Certificate" and .metadata.namespace == "kube-system" and
        .spec.secretName == tlsSecret and .spec.dnsNames == [host])] | length == 1) and
      ([.[] | select(.kind == "HelmRelease" and .metadata.name == "oauth2-proxy" and .metadata.namespace == "oauth2-proxy") |
        .spec.values.extraEnv[]? | select(.name == "OAUTH2_PROXY_WHITELIST_DOMAINS")] | length == 1 and
          .[0].value == ("${domain},.${domain}," + host))
    else
      ($routes == [] and $listeners == [])
    end' >/dev/null || fail "${provider}: the private preview must use its dedicated HTTPS listener and only the sign-in proxy"

  # A route that names no hostname inherits the listener's, so this test cannot
  # tell which callback it needs. Refuse it rather than skip it.
  unnamed="$(yq eval -N "${fronted_routes} | select((.spec.hostnames // []) | length == 0) | .metadata.namespace + \"/\" + .metadata.name" "${rendered}")"
  [[ -z "${unnamed}" ]] ||
    fail "${provider}: these HTTPRoutes send traffic to oauth2-proxy without naming a hostname, so their login callback cannot be checked: ${unnamed//$'\n'/, }"

  # oauth2-proxy prefers an X-Auth-Request-Redirect header over the page the
  # user asked for, so a route that sets one sends every login to that one URL.
  fixed="$(yq eval -N "${fronted_routes} | select([.spec.rules[]?.filters[]? | select(.type == \"RequestHeaderModifier\") | ((.requestHeaderModifier.set // []) + (.requestHeaderModifier.add // []))[] | select(.name | downcase == \"x-auth-request-redirect\")] | length > 0) | .metadata.namespace + \"/\" + .metadata.name" "${rendered}")"
  [[ -z "${fixed}" ]] ||
    fail "${provider}: these HTTPRoutes send every login through oauth2-proxy to one fixed URL, dropping the page the user asked for. Remove their X-Auth-Request-Redirect header: ${fixed//$'\n'/, }"

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
  value_end='[[:space:]]*(#.*)?$'
  grep -Eq "^[[:space:]]*cookie_secure[[:space:]]*=[[:space:]]*true${value_end}" <<<"${o2p_config}" ||
    fail "${provider}: oauth2-proxy must set cookie_secure = true: it is what makes the login callback it derives https, the only scheme Dex has registered"

  # The SSO cookies stay host-only, and the __Host- prefix makes the browser
  # enforce it: no other host under the same site can plant one. The browser
  # refuses a __Host- cookie unless it is Secure (above), has Path=/ and has
  # no Domain. oauth2-proxy also reads cookie settings from the chart's
  # config.cookieName value (rendered as a flag), from extraArgs flags and from
  # OAUTH2_PROXY_COOKIE_* variables, all of which win over the config file, so
  # none of them may set one.
  grep -Eq "^[[:space:]]*cookie_name[[:space:]]*=[[:space:]]*(\"__Host-[^\"]+\"|'__Host-[^']+')${value_end}" <<<"${o2p_config}" ||
    fail "${provider}: oauth2-proxy's cookie_name must start with __Host-, so the browser keeps the SSO cookies to the host that set them and refuses one planted by another host under the same site"
  if grep -Eq '^[[:space:]]*cookie_path[[:space:]]*=' <<<"${o2p_config}" &&
    ! grep -Eq "^[[:space:]]*cookie_path[[:space:]]*=[[:space:]]*(\"/\"|'/')${value_end}" <<<"${o2p_config}"; then
    fail "${provider}: oauth2-proxy sets a cookie_path other than /; the browser drops a __Host- cookie with any other path, so nobody could stay signed in"
  fi
  if grep -Eq '^[[:space:]]*cookie_domains?[[:space:]]*=' <<<"${o2p_config}" &&
    ! grep -Eq "^[[:space:]]*cookie_domains[[:space:]]*=[[:space:]]*\[[[:space:]]*\]${value_end}" <<<"${o2p_config}"; then
    fail "${provider}: oauth2-proxy sets a cookie domain; the browser drops a __Host- cookie that carries a Domain, so nobody could stay signed in"
  fi
  # The reads fail the test when they fail; a grep that finds nothing is the
  # expected case, so only the greps may come back empty.
  # These checks hold the cookie to the HelmRelease's own spec.values, so
  # everything else that could change what the pod reads is refused: the chart
  # ignores config.configFile when config.existingConfig is set, alphaConfig is
  # enabled or a --config/--alpha-config flag names another file; envFrom can
  # bring OAUTH2_PROXY_COOKIE_* in from a ConfigMap or Secret; and Flux merges
  # spec.valuesFrom over the values and applies spec.postRenderers to the
  # rendered Deployment.
  o2p_release="$(yq eval -N 'select(.kind == "HelmRelease" and .metadata.name == "oauth2-proxy" and .metadata.namespace == "oauth2-proxy") | .spec' "${rendered}")"
  o2p_values_from_count="$(yq eval -N '(.valuesFrom // []) | length' - <<<"${o2p_release}")"
  o2p_post_renderer_count="$(yq eval -N '(.postRenderers // []) | length' - <<<"${o2p_release}")"
  o2p_cookie_name_value="$(yq eval -N '.config.cookieName // ""' - <<<"${o2p}")"
  o2p_existing_config="$(yq eval -N '.config.existingConfig // ""' - <<<"${o2p}")"
  o2p_alpha_config="$(yq eval -N '.alphaConfig.enabled // false' - <<<"${o2p}")"
  o2p_env_from_count="$(yq eval -N '(.envFrom // []) | length' - <<<"${o2p}")"
  o2p_args="$(yq eval -N '.extraArgs // ""' - <<<"${o2p}")"
  o2p_env_names="$(yq eval -N '.extraEnv[]?.name // ""' - <<<"${o2p}")"
  cookie_overrides="$(
    [[ -z "${o2p_cookie_name_value}" ]] || echo config.cookieName
    [[ -z "${o2p_existing_config}" ]] || echo config.existingConfig
    [[ "${o2p_alpha_config}" == false ]] || echo alphaConfig.enabled
    [[ "${o2p_env_from_count}" == 0 ]] || echo envFrom
    [[ "${o2p_values_from_count}" == 0 ]] || echo spec.valuesFrom
    [[ "${o2p_post_renderer_count}" == 0 ]] || echo spec.postRenderers
    { grep -Eo '(^|[^[:alnum:]_])(alpha-)?config([[:space:]]*:|=)' <<<"${o2p_args}" || true; } | sed 's/.*/extraArgs --config or --alpha-config/'
    { grep -Eo 'cookie[-_](name|secure|domains?|path)' <<<"${o2p_args}" || true; } | sed 's/^/extraArgs /'
    { grep -E '^OAUTH2_PROXY_COOKIE_(NAME|SECURE|DOMAINS?|PATH)$' <<<"${o2p_env_names}" || true; } | sed 's/^/extraEnv /'
  )"
  [[ -z "${cookie_overrides}" ]] ||
    fail "${provider}: oauth2-proxy's cookie settings are overridden outside its config file, where this test cannot hold them to the __Host- rules: ${cookie_overrides//$'\n'/, }"

  registered="$(yq eval -N 'select(.kind == "HelmRelease" and .metadata.name == "dex" and .metadata.namespace == "dex") | .spec.values.config.staticClients[] | select(.id == "public-client") | .redirectURIs[]' "${rendered}")"
  [[ -n "${registered}" ]] ||
    fail "${provider}: no Dex public-client redirectURIs were rendered; the Dex client moved or was renamed, so this test is checking nothing"

  missing=()
  while IFS= read -r host; do
    [[ -n "${host}" ]] || continue
    grep -qxF -- "https://${host}/oauth2/callback" <<<"${registered}" ||
      missing+=("https://${host}/oauth2/callback")
    grep -qxF -- "https://${host}:443/oauth2/callback" <<<"${registered}" ||
      missing+=("https://${host}:443/oauth2/callback")
  done < <(printf '%s\n%s\n' "${hosts}" "${own_hosts}" | sort -u)

  if ((${#missing[@]} > 0)); then
    fail "${provider}: these hosts are behind oauth2-proxy but Dex does not accept their login callback, so signing in there fails with 'Unregistered redirect_uri'. Add each to the public-client redirectURIs in ${dex_file}: ${missing[*]}"
  fi

  printf '%s\n%s\n' "${hosts}" "${own_hosts}" >>"${all_hosts}"
  printf '%s\n' "${registered}" >"${workdir}/${provider}-registered.txt"
done

# Extract only the two exact HTTPS default-port callback forms. An arbitrary
# port, userinfo or wildcard must not be normalized into an authorized host.
callback_host() {
  local authority
  [[ "$1" =~ ^https://([^/]+)/oauth2/callback$ ]] || return 1
  authority="${BASH_REMATCH[1]}"
  case "$authority" in
    *:443) authority="${authority%:443}" ;;
  esac
  [[ -n "$authority" && "$authority" != *:* && "$authority" != *@* && "$authority" != *\** ]] || return 1
  printf '%s\n' "$authority"
}

for uri in https://fronted.example/oauth2/callback https://fronted.example:443/oauth2/callback; do
  [[ "$(callback_host "$uri")" == fronted.example ]] || fail 'the callback selector rejected a valid HTTPS default-port form'
done
for uri in http://fronted.example/oauth2/callback https://fronted.example:8443/oauth2/callback \
  https://fronted.example:443:443/oauth2/callback https://user@fronted.example:443/oauth2/callback \
  'https://*.example:443/oauth2/callback' https://fronted.example:443/other/callback; do
  if callback_host "$uri" >/dev/null; then fail 'the callback selector admitted an unrelated origin, port or path'; fi
done

# --- No callback outlives its route ----------------------------------------
# The Dex client is shared by both providers, so an entry is live while any
# provider still fronts its host.
sort -u -o "${all_hosts}" "${all_hosts}"
stale=()
while IFS= read -r uri; do
  [[ "${uri}" == */oauth2/callback ]] || continue
  host="$(callback_host "$uri")" || fail "Dex accepts a non-default HTTPS oauth2-proxy callback: ${uri}"
  grep -qxF -- "${host}" "${all_hosts}" || stale+=("${uri}")
done < <(sort -u "${workdir}"/*-registered.txt)

if ((${#stale[@]} > 0)); then
  fail "Dex still accepts an oauth2-proxy login callback on hosts no HTTPRoute sends to oauth2-proxy. Remove each from the public-client redirectURIs in ${dex_file}: ${stale[*]}"
fi

printf 'PASS: a login through oauth2-proxy returns to the page it started from on all %d hosts it fronts, and Dex accepts no other oauth2-proxy callback\n' \
  "$(grep -c . "${all_hosts}")"
