#!/usr/bin/env bash
# Measure whether the Gateway API ExternalAuth subrequest crosses nodes on the deployed Cilium.
#
# THE QUESTION (devantler-tech/platform#2284, #3784). On Cilium 1.20.0-pre.3 the gateway Envoy's
# ext_authz subrequest to oauth2-proxy was black-holed whenever it crossed nodes. The ipcache read
# (diagnose-cilium-ext-authz-ipcache.sh) cannot settle it while node encryption is off, so this
# script sends real requests through a test-only route and observes whether they arrive.
#
# THE PATH. Requests enter through the gateway's Hetzner load balancer, as public traffic does. The
# gateway listeners accept TLS only from the load balancer's private address (the `gateway-receiver`
# physical-peer fence sets `directSourcePrefixRanges` on every TLS filter chain), so a client inside
# the cluster can never complete a handshake; that is the fence working, not a fault. The load
# balancer adds the PROXY protocol header the listeners require, so the client sends none. The client
# connects to the load balancer's public IPv4 address with no server name, which the listener's
# catch-all TLS chain serves, and selects the route with the HTTP `Host` header.
#
# WHICH ENVOY ANSWERED IS OBSERVED, NOT ASSUMED. Cilium runs one Envoy per node, and the load
# balancer chooses the node. Each measurement is a PAIR of requests that one curl invocation sends on
# ONE connection (`--next`): first the plain control route, then the ExternalAuth route. The control
# route's backend (whoami) echoes the connection's source address, and Cilium's Envoy connects to
# backends from its own node's ingress IP (CiliumNode spec.ingress.ipv4), so the echo names the node
# whose Envoy holds that connection — and therefore also answered the ExternalAuth request. A pair is
# attributed only when the echo is exactly one node's ingress IP and curl reports the second request
# reused the connection (`%{num_connects}` = 0); any other pair is counted as unattributed and left
# out of both groups. A pair attributed to a node hosting an oauth2-proxy endpoint is SAME-NODE (Envoy
# does not prefer local endpoints, so it is a mix — enough to prove ext_authz works at all); one
# attributed to a node hosting none is CROSS-NODE, so its ExternalAuth call must cross nodes.
# Placement is read from the oauth2-proxy and whoami EndpointSlices — what Envoy actually targets,
# addresses included — before and after the run, together with the node set and the ingress IPs; any
# change is INCONCLUSIVE. The Cilium and cilium-envoy DaemonSets must also be fully rolled out and
# unchanged across the run: after a failed deploy releases the lock, old and new datapath pods can run
# side by side, and a verdict taken across them would describe no single deployed revision.
#
# WHY A FAILURE IS ATTRIBUTABLE. The routes use hostnames under `externalauth-probe.invalid`, which no
# DNS serves; external-dns publishes nothing outside its zone. The `http` listener accepts routes only
# from its own namespace, so a route in `whoami` cannot attach there (`NotAllowedByListeners`); the
# `https` listener accepts every namespace and has no hostname, so it serves the made-up hostnames
# with its default certificate. The client skips certificate checks (`curl -k`): no `.invalid` name
# has a certificate, and TLS is not what the probe measures. The two routes of a pair have the SAME
# backend, so if the control route does not answer every time, the path or policy is broken and the
# run is INCONCLUSIVE rather than blamed on ext_authz.
#
#   delivered  a 302/303 whose Location is the Dex login (only oauth2-proxy produces it), or a 401
#   lost       no response within the timeout, a 502/503/504, or an EMPTY 403 (Envoy's ext_authz
#              error) — the shapes a subrequest that never arrived takes
#   other      anything else (a 200 means the filter is not applied; a 404 or 301 means the route is
#              not programmed; another 5xx came from a backend that DID answer) — any occurrence
#              makes the run INCONCLUSIVE, and the distinct status codes are printed
#
# Warm-up sends pairs until the control route answers 200 and the ExternalAuth route answers anything
# but the unprogrammed-route shapes (404, 301). A timeout counts as programmed there, so a COMPLETE
# black-hole still reaches the measurement instead of reading as a broken path. Every measured pair
# repeats that proof for its own Envoy: its control answer must be 200, and the control route is
# created only after the ExternalAuth route was accepted (see the creation order below).
#
# THE VERDICT (at least 10 pairs requested and at least 10 attributed pairs in EACH group; fewer is
# always INCONCLUSIVE)
#   FIXED           control routes answered every time, a same-node pair was delivered at least once,
#                   and EVERY cross-node pair was delivered.
#   FAULT-PERSISTS  control routes answered every time, a same-node pair was delivered at least once,
#                   at least 90% of cross-node pairs were lost, and the same-node loss rate sits near
#                   the remote-endpoint share and well below the cross-node rate — the #2284 pattern,
#                   not an intermittent blip.
#   INCONCLUSIVE    anything else, including intermittent loss below that bar.
#
# GUARD. The ExternalAuth route shares the `https` listener with every public HTTPS route. The run
# finishes with one request to an unrouted hostname, which must still get the listener's no-route
# answer (404); anything else means the probe changed another route's behaviour, and the run is
# INCONCLUSIVE. This detects the exposure; it cannot prevent it for the run's duration.
#
# WHAT IT WRITES — stated plainly, because unlike the ipcache read this changes production while it
# runs. Everything carries the label `platform.devantler.tech/externalauth-probe=<run-id>`:
#   * in `whoami`: two HTTPRoutes (control, ExternalAuth);
#   * in `oauth2-proxy`: one ReferenceGrant letting the ExternalAuth route name the oauth2-proxy
#     Service.
# No existing object is modified. EXIT, INT and TERM all delete everything carrying the run's label.
# If a previous run left labelled objects behind, this run refuses before creating anything.
#
# WHAT IT PRINTS. The workflow log of a public repository is public, so no address, node name, UID
# or redirect URL is printed — only counts, status codes and the verdict. kubectl's and curl's stderr
# are discarded for the same reason. Under GitHub Actions the load balancer address is registered
# with `::add-mask::` as soon as it is read, before any request uses it.
#
# EXIT CODES
#   0  a conclusive verdict (FIXED or FAULT-PERSISTS)
#   1  usage error — nothing was read or written
#   3  INCONCLUSIVE — the run proved nothing, and a caller must not report it as green
#   4  cleanup failed — labelled objects may remain; delete them by label before the next run
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly probe_namespace='whoami'
readonly oauth2_namespace='oauth2-proxy'
readonly oauth2_service='oauth2-proxy'
readonly backend_service='whoami'
readonly gateway_namespace='kube-system'
readonly gateway_service='cilium-gateway-platform'
readonly probe_label='platform.devantler.tech/externalauth-probe'
readonly control_host='control.externalauth-probe.invalid'
readonly authz_host='authz.externalauth-probe.invalid'
readonly guard_host='unrouted.externalauth-probe.invalid'
# Each pair sends 2 requests, each bounded by the timeout, so the measurement takes at most
# 2 * requests * timeout. The cap keeps the whole run, including warm-up, inside the workflow's job
# timeout (pinned by the test).
readonly max_request_seconds=250
readonly min_conclusive_requests=10
readonly warmup_attempts=12
# Wall-clock bound on waiting for route acceptance, so slow API calls cannot stretch it (the test
# shortens it; production keeps the default).
readonly route_wait_seconds="${PROBE_ROUTE_WAIT_SECONDS:-120}"
readonly poll_seconds="${PROBE_POLL_SECONDS:-5}"
readonly warmup_pause_seconds="${PROBE_WARMUP_PAUSE_SECONDS:-1}"

usage() {
  printf 'Usage: %s --context <kube-context> --run-id <digits> [--requests N] [--timeout SECONDS]\n' "$(basename "$0")" >&2
}

context=''
run_id=''
requests='40'
timeout='5'
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context | --run-id | --requests | --timeout)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      case "$1" in
        --context) context="$2" ;;
        --run-id) run_id="$2" ;;
        --requests) requests="$2" ;;
        --timeout) timeout="$2" ;;
      esac
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage
      exit 1
      ;;
  esac
done

# Required, never defaulted: writing to whatever the kubeconfig's current-context happens to be would
# change a cluster that is not the one being measured.
if [[ -z "${context}" ]]; then
  printf 'Refusing to run: --context is required.\n' >&2
  usage
  exit 1
fi
# The run id becomes part of object names and a label value, so it is digits only (a GitHub run id).
if [[ ! "${run_id}" =~ ^[1-9][0-9]{0,19}$ ]]; then
  printf 'Refusing to run: --run-id must be a decimal number with no leading zero.\n' >&2
  usage
  exit 1
fi
# Plain decimal with no leading zero BEFORE any arithmetic: bash reads `08` as invalid octal.
if [[ ! "${requests}" =~ ^[1-9][0-9]{0,2}$ ]] || ((requests > 100)); then
  printf 'Refusing to run: --requests must be a whole number from 1 to 100.\n' >&2
  usage
  exit 1
fi
if [[ ! "${timeout}" =~ ^[1-9][0-9]?$ ]] || ((timeout > 10)); then
  printf 'Refusing to run: --timeout must be a whole number of seconds from 1 to 10.\n' >&2
  usage
  exit 1
fi
if ((requests * timeout > max_request_seconds)); then
  printf 'Refusing to run: --requests x --timeout must not exceed %s seconds.\n' "${max_request_seconds}" >&2
  usage
  exit 1
fi

readonly control_route="externalauth-probe-control-${run_id}"
readonly authz_route="externalauth-probe-authz-${run_id}"
readonly probe_grant="externalauth-probe-${run_id}"

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}" || true
  fi
}

inconclusive() {
  printf 'VERDICT: INCONCLUSIVE\n'
  printf 'Reason: %s\n' "$1"
  summary "### Cilium ExternalAuth cross-node probe (#2284)"
  summary ""
  summary "**Verdict:** INCONCLUSIVE — $1"
  exit 3
}

conclude() {
  printf 'VERDICT: %s\n' "$1"
  printf 'Reason: %s\n' "$2"
  summary "### Cilium ExternalAuth cross-node probe (#2284)"
  summary ""
  summary "**Verdict:** $1 — $2"
  exit 0
}

# Every request is bounded: a stalled API server or network must end in INCONCLUSIVE (or a reported
# cleanup failure), never in a probe that hangs until the workflow kills it mid-cleanup.
readonly request_timeout='30s'
kc() {
  kubectl --context "${context}" --request-timeout="${request_timeout}" "$@" 2>/dev/null
}

# Placement from a Service's EndpointSlices ($1 namespace, $2 Service): every endpoint must be
# ready, not terminating, and carry a node and a target UID. Emits "FP <fingerprint>" then one
# "NODE <name>" per endpoint.
read_endpoints() {
  local json
  json="$(kc -n "$1" get endpointslices -l "kubernetes.io/service-name=$2" -o json)" || return 1
  jq -r '
    [.items[]?.endpoints[]? | {
        ready: (.conditions.ready == true),
        terminating: (.conditions.terminating == true),
        node: (.nodeName // ""),
        uid: (.targetRef.uid // ""),
        addrs: ([.addresses[]?] | sort | join(","))
      }]
    | if length == 0 then "ERR none"
      elif any(.[]; (.ready | not) or .terminating or .node == "" or .uid == "" or .addrs == "") then "ERR unsettled"
      else "FP " + (map(.uid + "|" + .node + "|" + .addrs) | sort | join(";")), (.[] | "NODE " + .node)
      end
  ' <<<"${json}"
}

# The node set. Emits "FP <fingerprint>" (name and UID of every node), or "ERR none" / "ERR identity".
read_nodes() {
  local json
  json="$(kc get nodes -o json)" || return 1
  jq -r '
    if ([.items[]?] | length) == 0 then "ERR none"
    elif any(.items[]?; (.metadata.name // "") == "" or (.metadata.uid // "") == "") then "ERR identity"
    else "FP " + ([.items[] | .metadata.name + "|" + .metadata.uid] | sort | join(";"))
    end
  ' <<<"${json}"
}

# Both datapath DaemonSets fully rolled out: observed their current generation, and every scheduled
# pod updated, available and ready. Emits "FP <fingerprint>" (name, generation, desired count), or
# "ERR missing" / "ERR rolling".
read_datapath() {
  local json
  json="$(kc -n kube-system get daemonsets cilium cilium-envoy -o json)" || return 1
  jq -r '
    [.items[]? | {
        name: (.metadata.name // ""),
        gen: (.metadata.generation // -1),
        observed: (.status.observedGeneration // -2),
        desired: (.status.desiredNumberScheduled // -1),
        updated: (.status.updatedNumberScheduled // -2),
        available: (.status.numberAvailable // -3),
        ready: (.status.numberReady // -4),
        unavailable: (.status.numberUnavailable // 0)
      }]
    | if ([.[].name] | sort) != ["cilium", "cilium-envoy"] then "ERR missing"
      elif any(.[]; .observed != .gen or .desired < 1 or .updated != .desired
                    or .available != .desired or .ready != .desired or .unavailable != 0) then "ERR rolling"
      else "FP " + (map(.name + "|" + (.gen | tostring) + "|" + (.desired | tostring)) | sort | join(";"))
      end
  ' <<<"${json}"
}

# Per-node Cilium ingress IPs — the source address a node's Envoy uses toward backends. Emits
# "FP <fingerprint>" then "ING <node> <ipv4>" per node, or "ERR none" / "ERR incomplete" /
# "ERR duplicate".
read_ingress_ips() {
  local json
  json="$(kc get ciliumnodes -o json)" || return 1
  jq -r '
    [.items[]? | {name: (.metadata.name // ""), ip: (.spec.ingress.ipv4 // "")}]
    | if length == 0 then "ERR none"
      elif any(.[]; .name == "" or .ip == "") then "ERR incomplete"
      elif ([.[].ip] | unique | length) != length then "ERR duplicate"
      else "FP " + (map(.name + "|" + .ip) | sort | join(";")), (.[] | "ING " + .name + " " + .ip)
      end
  ' <<<"${json}"
}

# The gateway load balancer: its Service must declare that the load balancer sends the PROXY header,
# and publish exactly one public IPv4 address. Emits "LB <ipv4>", or "ERR proxy" / "ERR address".
read_gateway_address() {
  local json
  json="$(kc -n "${gateway_namespace}" get service "${gateway_service}" -o json)" || return 1
  jq -r '
    def ipv4: test("^[0-9]{1,3}(\\.[0-9]{1,3}){3}$");
    def private: test("^(0\\.|10\\.|127\\.|169\\.254\\.|192\\.168\\.|172\\.(1[6-9]|2[0-9]|3[01])\\.|100\\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\\.)");
    (.metadata.annotations["load-balancer.hetzner.cloud/uses-proxyprotocol"] // "") as $proxy
    | [.status.loadBalancer.ingress[]? | .ip // empty | select(ipv4) | select(private | not)] as $public
    | if $proxy != "true" then "ERR proxy"
      elif ($public | length) != 1 then "ERR address"
      else "LB " + $public[0]
      end
  ' <<<"${json}"
}

# ---------------------------------------------------------------------------
# 1. Topology before: settled oauth2-proxy and control backend endpoints, nodes, datapath, ingress
#    IPs, and the load balancer the requests enter through.
# ---------------------------------------------------------------------------
endpoint_lines="$(read_endpoints "${oauth2_namespace}" "${oauth2_service}")" || inconclusive 'could not read the oauth2-proxy endpoints'
case "${endpoint_lines}" in
  'ERR none') inconclusive 'the oauth2-proxy Service has no endpoints' ;;
  'ERR unsettled') inconclusive 'an oauth2-proxy endpoint is not settled (rollout in flight)' ;;
esac
endpoint_fp_before="$(sed -n 's/^FP //p' <<<"${endpoint_lines}")"
endpoint_node_list="$(sed -n 's/^NODE //p' <<<"${endpoint_lines}")"

backend_lines="$(read_endpoints "${probe_namespace}" "${backend_service}")" || inconclusive 'could not read the control backend endpoints'
case "${backend_lines}" in
  'ERR none') inconclusive 'the control backend Service has no endpoints' ;;
  'ERR unsettled') inconclusive 'a control backend endpoint is not settled (rollout in flight)' ;;
esac
backend_fp_before="$(sed -n 's/^FP //p' <<<"${backend_lines}")"

node_lines="$(read_nodes)" || inconclusive 'could not read the nodes'
case "${node_lines}" in
  'ERR none') inconclusive 'no nodes were found' ;;
  'ERR identity') inconclusive 'a node reported no name or UID' ;;
esac
node_fp_before="$(sed -n 's/^FP //p' <<<"${node_lines}")"
datapath_lines="$(read_datapath)" || inconclusive 'could not read the Cilium and cilium-envoy DaemonSets'
case "${datapath_lines}" in
  'ERR missing') inconclusive 'the Cilium or cilium-envoy DaemonSet was not found' ;;
  'ERR rolling') inconclusive 'a Cilium or cilium-envoy rollout is incomplete, so nodes may run different datapath revisions' ;;
esac
datapath_fp_before="$(sed -n 's/^FP //p' <<<"${datapath_lines}")"

ingress_lines="$(read_ingress_ips)" || inconclusive 'could not read the per-node Cilium ingress IPs'
case "${ingress_lines}" in
  'ERR none' | 'ERR incomplete') inconclusive 'a node has no Cilium ingress IP, so the Envoy that answers a request cannot be attributed' ;;
  'ERR duplicate') inconclusive 'two nodes report the same Cilium ingress IP' ;;
esac
ingress_fp_before="$(sed -n 's/^FP //p' <<<"${ingress_lines}")"
readonly ipv4_re='^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
while read -r _ _ ip; do
  [[ -z "${ip:-}" ]] && continue
  [[ "${ip}" =~ ${ipv4_re} ]] || inconclusive 'a node has no well-formed Cilium ingress IP'
done < <(grep '^ING ' <<<"${ingress_lines}" || true)

if [[ -z "${endpoint_fp_before}" || -z "${endpoint_node_list}" || -z "${backend_fp_before}" || -z "${node_fp_before}" || -z "${datapath_fp_before}" || -z "${ingress_fp_before}" ]]; then
  inconclusive 'the topology reads did not parse'
fi

gateway_lines="$(read_gateway_address)" || inconclusive 'could not read the gateway load balancer Service'
case "${gateway_lines}" in
  'ERR proxy') inconclusive 'the gateway load balancer does not declare that it sends the PROXY header the listeners require' ;;
  'ERR address') inconclusive 'the gateway load balancer does not publish exactly one public IPv4 address' ;;
esac
gateway_address="$(sed -n 's/^LB //p' <<<"${gateway_lines}")"
[[ "${gateway_address}" =~ ${ipv4_re} ]] || inconclusive 'the gateway load balancer address did not parse'
# Masked before anything can print it; only the runner sees this line.
if [[ "${GITHUB_ACTIONS:-}" == 'true' ]]; then
  printf '::add-mask::%s\n' "${gateway_address}"
fi
readonly gateway_url="https://${gateway_address}/"

printf 'oauth2-proxy endpoints on %s node(s); nodes with an ingress IP: %s\n' \
  "$(sort -u <<<"${endpoint_node_list}" | grep -c . || true)" "$(grep -c '^ING ' <<<"${ingress_lines}" || true)"

# ---------------------------------------------------------------------------
# 2. Refuse to run over leftovers. A labelled object from an earlier run means its cleanup failed;
#    this run must not measure through it, and must not delete it (it is not this run's).
# ---------------------------------------------------------------------------
if ! leftovers="$(kc -n "${probe_namespace}" get httproutes,ciliumnetworkpolicies,pods -l "${probe_label}" -o name)"; then
  inconclusive 'could not check for objects left by an earlier probe run'
fi
if ! leftover_grants="$(kc -n "${oauth2_namespace}" get referencegrants -l "${probe_label}" -o name)"; then
  inconclusive 'could not check for objects left by an earlier probe run'
fi
if [[ -n "${leftovers}${leftover_grants}" ]]; then
  inconclusive "objects labelled ${probe_label} already exist; delete them by label before running again"
fi

# ---------------------------------------------------------------------------
# 3. Create the probe objects. From here on, the traps remove everything this run labelled.
# ---------------------------------------------------------------------------
# Invoked by the EXIT trap below. CI's shellcheck reports that as SC2317, newer releases as SC2329.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local rc=$?
  local failed=0
  trap - EXIT INT TERM
  # Wait for the objects to be gone, bounded below the 30s request timeout so the watch is not cut
  # off first: a delete that returns while finalizers still hold an object would report success and
  # leave the next run refusing to start. A timeout counts as a cleanup failure.
  kc -n "${probe_namespace}" delete httproutes -l "${probe_label}=${run_id}" --ignore-not-found --wait=true --timeout=25s >/dev/null || failed=1
  kc -n "${oauth2_namespace}" delete referencegrants -l "${probe_label}=${run_id}" --ignore-not-found --wait=true --timeout=25s >/dev/null || failed=1
  if [[ "${failed}" -ne 0 ]]; then
    printf 'CLEANUP: FAILED — delete objects labelled %s=%s by hand\n' "${probe_label}" "${run_id}"
    summary ""
    summary "**Cleanup failed** — delete objects labelled \`${probe_label}=${run_id}\` by hand."
    exit 4
  fi
  printf 'CLEANUP: done\n'
  exit "${rc}"
}
trap cleanup EXIT
# A cancelled job sends TERM (or INT); turning it into a normal exit runs the same cleanup at once.
trap 'exit 143' INT TERM

route_manifest() {
  local name="$1" host="$2" filters="$3"
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ${name}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  parentRefs:
    - name: platform
      namespace: kube-system
      sectionName: https
  hostnames:
    - ${host}
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
${filters}
      backendRefs:
        - name: whoami
          port: 80
YAML
}

# The same ExternalAuth filter shape the reverted cutover used (#2283), so the probe exercises the
# datapath #1881 would ship.
readonly authz_filters='      filters:
        - type: ExternalAuth
          externalAuth:
            protocol: HTTP
            backendRef:
              name: oauth2-proxy
              namespace: oauth2-proxy
              port: 80
            http:
              allowedHeaders:
                - cookie
                - x-forwarded-proto
                - x-forwarded-host
              allowedResponseHeaders:
                - set-cookie
                - x-auth-request-user
                - x-auth-request-email
                - x-auth-request-groups'

manifest="$(
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: ${probe_grant}
  namespace: ${oauth2_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: ${probe_namespace}
  to:
    - group: ""
      kind: Service
      name: ${oauth2_service}
YAML
  route_manifest "${authz_route}" "${authz_host}" "${authz_filters}"
)"

# Prints how many of the named routes are Accepted with resolved references.
ready_routes() {
  local json
  json="$(kc -n "${probe_namespace}" get httproutes -l "${probe_label}=${run_id}" -o json)" || {
    printf '0'
    return 0
  }
  jq -r --arg names "$*" '
    def cond($t): [.status.parents[]?.conditions[]? | select(.type == $t) | .status];
    ($names | split(" ")) as $want
    | [.items[]? | select((.metadata.name // "") as $n | $want | index($n))
        | select((cond("Accepted") | length) > 0 and all(cond("Accepted")[]; . == "True")
                 and (cond("ResolvedRefs") | length) > 0 and all(cond("ResolvedRefs")[]; . == "True"))]
    | length' <<<"${json}" 2>/dev/null || printf '0'
}

# Prints, for each named route that is not ready, which condition held it back — its condition type,
# status and reason, e.g. `ResolvedRefs=False (RefNotPermitted)` — so an INCONCLUSIVE run says why.
# Reasons are Gateway API's fixed CamelCase words; anything outside [A-Za-z] is dropped so a message
# field, address or name can never reach the public log. The route is named by its role, not its name.
route_blockers() {
  local json
  json="$(kc -n "${probe_namespace}" get httproutes -l "${probe_label}=${run_id}" -o json)" || {
    printf 'route status unreadable'
    return 0
  }
  jq -r --arg authz "${authz_route}" --arg control "${control_route}" --arg names "$*" '
    def clean: tostring | gsub("[^A-Za-z]"; "");
    ($names | split(" ")) as $want
    | [ $want[] as $n
        | ([.items[]? | select(.metadata.name == $n)] | first) as $r
        | (if $n == $authz then "ExternalAuth route" elif $n == $control then "control route" else "route" end) as $role
        | if $r == null then "\($role): not found"
          else ([$r.status.parents[]?.conditions[]?
                  | select((.type == "Accepted" or .type == "ResolvedRefs") and .status != "True")
                  | "\(.type | clean)=\(.status | clean) (\(.reason // "" | clean))"] | unique) as $bad
            | if ($bad | length) > 0 then "\($role): \($bad | join(", "))"
              elif ([$r.status.parents[]?.conditions[]? | select(.type == "Accepted" or .type == "ResolvedRefs")] | length) == 0
              then "\($role): no Accepted or ResolvedRefs condition yet"
              else empty end
          end ]
    | if length > 0 then join("; ") else "no blocking condition seen" end' <<<"${json}" 2>/dev/null ||
    printf 'route status unreadable'
}

# Waits, under the shared wall-clock deadline, until every named route is ready. It always checks at
# least once before honouring the deadline: the two phases share one deadline, so a slow first phase
# must not leave the second phase refusing without ever looking.
wait_routes() {
  while :; do
    if [[ "$(ready_routes "$@")" == "$#" ]]; then
      return 0
    fi
    ((SECONDS < route_deadline)) || return 1
    sleep "${poll_seconds}"
  done
}

readonly route_deadline=$((SECONDS + route_wait_seconds))

# ORDER MATTERS, because it is what makes every control answer a proof of programming. Both routes are
# compiled into the gateway's single CiliumEnvoyConfig, and each Envoy applies its versions in order.
# The ExternalAuth route is created FIRST and the control route only once the ExternalAuth route is
# accepted, so any Envoy that already serves the control route (200) has also applied the ExternalAuth
# route. A failure on the ExternalAuth route of a pair whose control answered is therefore a
# measurement, never an Envoy whose configuration is still converging.
if ! kc apply -f - <<<"${manifest}" >/dev/null; then
  inconclusive 'could not create the ExternalAuth probe route and grant'
fi
wait_routes "${authz_route}" ||
  inconclusive "the ExternalAuth probe route was not accepted with resolved references in time ($(route_blockers "${authz_route}"))"
if ! kc apply -f - <<<"$(route_manifest "${control_route}" "${control_host}" '')" >/dev/null; then
  inconclusive 'could not create the control probe route'
fi
wait_routes "${authz_route}" "${control_route}" ||
  inconclusive "the control probe route was not accepted with resolved references in time ($(route_blockers "${authz_route}" "${control_route}"))"

# ---------------------------------------------------------------------------
# 4. Send the pairs through the load balancer.
# ---------------------------------------------------------------------------
# One pair: the control route, then the ExternalAuth route, in ONE curl invocation so the second
# request reuses the first one's connection and reaches the same Envoy. curl's stderr is discarded:
# its error text can name an address, and the log is public. Prints
# "PAIR <control code> <echoed source or -> <authz code> <authz size> <authz connects> [<location>]".
send_pair() {
  local out control_code ip authz
  out="$(curl -sk --max-time "${timeout}" -H "Host: ${control_host}" \
    -w '\nPROBE-CONTROL %{http_code}\n' "${gateway_url}" \
    --next -sk -o /dev/null --max-time "${timeout}" -H "Host: ${authz_host}" \
    -w 'PROBE-AUTHZ %{http_code} %{size_download} %{num_connects} %{redirect_url}\n' "${gateway_url}" 2>/dev/null || true)"
  control_code="$(sed -n 's/^PROBE-CONTROL //p' <<<"${out}" | tail -n 1)"
  # whoami echoes the connection's source as "RemoteAddr: <ip>:<port>".
  ip="$(sed -n 's/^RemoteAddr: *\[\{0,1\}\([0-9A-Fa-f.:]*\)\]\{0,1\}:[0-9]*$/\1/p' <<<"${out}" | head -n 1)"
  authz="$(sed -n 's/^PROBE-AUTHZ //p' <<<"${out}" | tail -n 1)"
  printf 'PAIR %s %s %s\n' "${control_code:-000}" "${ip:--}" "${authz:-000 0 -}"
}

# Warm-up: some Envoy must serve the control route (200) while the ExternalAuth route does not answer
# like an unprogrammed hostname (404, or a 301 from a stale redirect). A timeout there is NOT
# unprogrammed — it is what a black-holed check looks like — so it ends the warm-up and is measured.
warm=0
attempt=0
while ((attempt < warmup_attempts)); do
  attempt=$((attempt + 1))
  read -r _ warm_control _ warm_authz _ <<<"$(send_pair)"
  if [[ "${warm_control}" == '200' && -n "${warm_authz}" && "${warm_authz}" != '404' && "${warm_authz}" != '301' ]]; then
    warm=1
    break
  fi
  sleep "${warmup_pause_seconds}"
done
if [[ "${warm}" -ne 1 ]]; then
  # One more control request, with curl's error text, says how the connection failed (reset, refused,
  # timeout, TLS). Only a word from this fixed list is printed: the raw text can name an address.
  err="$(curl -sS -k -o /dev/null --max-time "${timeout}" -H "Host: ${control_host}" "${gateway_url}" 2>&1 >/dev/null || true)"
  case "${err}" in
    '') why=none ;;
    *'reset by peer'*) why=connection-reset ;;
    *'Connection refused'*) why=connection-refused ;;
    *'timed out'* | *'Timeout was reached'*) why=timeout ;;
    *SSL_ERROR_SYSCALL*) why=tls-syscall ;;
    *'nexpected eof'* | *'nexpected EOF'*) why=tls-unexpected-eof ;;
    *alert*) why=tls-alert ;;
    *'resolve host'*) why=dns ;;
    *) why=other ;;
  esac
  [[ "${warm_control:-}" =~ ^[0-9]{3}$ ]] || warm_control=none
  [[ "${warm_authz:-}" =~ ^[0-9]{3}$ ]] || warm_authz=none
  inconclusive "the load balancer path could not reach both routes during warm-up (last answers control=${warm_control} authz=${warm_authz}, then one more request failed with curl=${why}), so the path or policy is broken"
fi

pairs=''
i=0
while ((i < requests)); do
  i=$((i + 1))
  pairs="${pairs}$(send_pair)"$'\n'
done
guard_code="$(curl -sk -o /dev/null --max-time "${timeout}" -H "Host: ${guard_host}" -w '%{http_code}' "${gateway_url}" 2>/dev/null || true)"
[[ "${guard_code}" =~ ^[0-9]{3}$ ]] || guard_code='none'

# ---------------------------------------------------------------------------
# 5. Classify the pairs.
# ---------------------------------------------------------------------------
# Prints: control_ok control_bad unattributed
#         cross_n cross_delivered cross_lost cross_other same_n same_delivered same_lost same_other
#         same_remote other_codes
# A pair is attributed to a node only when its echoed source is exactly that node's ingress IP and the
# ExternalAuth request reused the connection (0 new connects). same_remote sums, over the same-node
# pairs, the number of oauth2-proxy endpoints NOT on the pair's node (the separation check below).
classify() {
  awk -v ingress="$(awk '$1 == "ING" { printf "%s=%s;", $3, $2 }' <<<"${ingress_lines}")" \
    -v endpoints="$(tr '\n' ';' <<<"${endpoint_node_list}")" '
    BEGIN {
      n = split(ingress, pairs_, ";")
      for (k = 1; k <= n; k++) if (pairs_[k] != "") { split(pairs_[k], kv, "="); node_of[kv[1]] = kv[2] }
      n = split(endpoints, eps, ";")
      for (k = 1; k <= n; k++) if (eps[k] != "") { local_eps[eps[k]]++; total++ }
    }
    $1 == "PAIR" {
      if ($2 != "200") { bad++; next }
      ok++
      ip = $3; code = $4; size = $5; conns = $6; loc = $7
      if (!(ip in node_of) || conns != "0") { unattributed++; next }
      node = node_of[ip]
      g = (node in local_eps) ? "same" : "cross"
      n_[g]++
      if (g == "same") remote += total - local_eps[node]
      if ((code == "302" || code == "303") && index(loc, "https://dex.") == 1) delivered[g]++
      else if (code == "401") delivered[g]++
      else if (code == "000" || code == "" || code == "502" || code == "503" || code == "504" || (code == "403" && size == "0")) lost[g]++
      else {
        other[g]++
        c = (code == "" ? "none" : code)
        if (!(c in seen)) { seen[c] = 1; codes = (codes == "" ? c : codes "," c) }
      }
    }
    END {
      printf "%d %d %d %d %d %d %d %d %d %d %d %d %s\n", ok, bad, unattributed,
        n_["cross"], delivered["cross"], lost["cross"], other["cross"],
        n_["same"], delivered["same"], lost["same"], other["same"], remote, (codes == "" ? "-" : codes)
    }
  ' <<<"${pairs}"
}

[[ "$(grep -c '^PAIR ' <<<"${pairs}" || true)" -eq "${requests}" ]] || inconclusive 'not every pair produced an answer'
counts="$(classify)" || inconclusive 'the pairs could not be classified'
read -r control_ok control_bad unattributed cross_n cross_delivered cross_lost cross_other \
  same_n same_delivered same_lost same_other same_remote other_codes <<<"${counts}"
[[ -n "${other_codes:-}" ]] || inconclusive 'the pairs could not be classified'

# ---------------------------------------------------------------------------
# 6. Topology after: the placement the verdict relies on must not have changed.
# ---------------------------------------------------------------------------
endpoint_lines_after="$(read_endpoints "${oauth2_namespace}" "${oauth2_service}")" || inconclusive 'could not re-read the oauth2-proxy endpoints'
[[ "$(sed -n 's/^FP //p' <<<"${endpoint_lines_after}")" == "${endpoint_fp_before}" ]] ||
  inconclusive 'the oauth2-proxy endpoints changed during the run'
backend_lines_after="$(read_endpoints "${probe_namespace}" "${backend_service}")" || inconclusive 'could not re-read the control backend endpoints'
[[ "$(sed -n 's/^FP //p' <<<"${backend_lines_after}")" == "${backend_fp_before}" ]] ||
  inconclusive 'the control backend endpoints changed during the run'
node_lines_after="$(read_nodes)" || inconclusive 'could not re-read the nodes'
[[ "$(sed -n 's/^FP //p' <<<"${node_lines_after}")" == "${node_fp_before}" ]] ||
  inconclusive 'the node set changed during the run'
datapath_lines_after="$(read_datapath)" || inconclusive 'could not re-read the Cilium and cilium-envoy DaemonSets'
[[ "$(sed -n 's/^FP //p' <<<"${datapath_lines_after}")" == "${datapath_fp_before}" ]] ||
  inconclusive 'the Cilium or cilium-envoy rollout changed during the run'
ingress_lines_after="$(read_ingress_ips)" || inconclusive 'could not re-read the per-node Cilium ingress IPs'
[[ "$(sed -n 's/^FP //p' <<<"${ingress_lines_after}")" == "${ingress_fp_before}" ]] ||
  inconclusive 'the per-node Cilium ingress IPs changed during the run'

# ---------------------------------------------------------------------------
# 7. Verdict.
# ---------------------------------------------------------------------------
printf 'pairs: %s sent, control %s ok / %s failed, %s unattributed\n' "${requests}" "${control_ok}" "${control_bad}" "${unattributed}"
printf 'cross-node pairs: %s — ExternalAuth %s delivered / %s lost / %s other\n' \
  "${cross_n}" "${cross_delivered}" "${cross_lost}" "${cross_other}"
printf 'same-node pairs:  %s — ExternalAuth %s delivered / %s lost / %s other\n' \
  "${same_n}" "${same_delivered}" "${same_lost}" "${same_other}"
summary "| group | pairs | delivered | lost | other |"
summary "| --- | --- | --- | --- | --- |"
summary "| cross-node | ${cross_n} | ${cross_delivered} | ${cross_lost} | ${cross_other} |"
summary "| same-node | ${same_n} | ${same_delivered} | ${same_lost} | ${same_other} |"
summary ""
summary "Control route: ${control_ok} ok, ${control_bad} failed; ${unattributed} pair(s) unattributed."
summary ""

if [[ "${guard_code}" != '404' ]]; then
  inconclusive "the https listener answered the unrouted hostname with ${guard_code} instead of 404 during the run, so the probe route affected other traffic"
fi
if ((control_bad > 0)); then
  inconclusive 'the plain control route did not answer every time, so failures cannot be attributed to ExternalAuth'
fi
if ((cross_other + same_other > 0)); then
  inconclusive "the ExternalAuth route returned answers that are neither delivered nor lost (status codes: ${other_codes})"
fi
if ((requests < min_conclusive_requests)); then
  inconclusive "a run of fewer than ${min_conclusive_requests} pairs is a smoke run and never conclusive"
fi
if ((cross_n < min_conclusive_requests || same_n < min_conclusive_requests)); then
  inconclusive "too few pairs were attributed to each group (cross ${cross_n}, same ${same_n}, unattributed ${unattributed}; at least ${min_conclusive_requests} each), so send more pairs"
fi
if ((same_delivered == 0)); then
  inconclusive 'no ExternalAuth request was delivered even on a node with a local endpoint, so the route or backend is broken'
fi
if ((cross_lost == 0)); then
  if ((same_lost > 0)); then
    inconclusive 'cross-node pairs were all delivered but same-node pairs were lost, which is not the #2284 pattern'
  fi
  conclude 'FIXED' "every cross-node ExternalAuth request (${cross_delivered}) was delivered"
fi
# Separation. A same-node pair's Envoy shares its node with some endpoints, so under the #2284 fault
# only its calls to REMOTE endpoints are lost: the expected same-node loss rate is the average remote
# share of the endpoints over those pairs, same_remote / (same_n * total_endpoints). Node-independent
# loss hits both groups alike, so FAULT-PERSISTS also needs the same-node loss rate to stay within 15
# points of that expectation ON EITHER SIDE (far below it means remote calls are being delivered,
# which contradicts a cross-node black-hole) and at least 15 points below the cross-node loss rate.
total_endpoints="$(grep -c . <<<"${endpoint_node_list}" || true)"
separated=0
deviation=$(((same_lost * total_endpoints - same_remote) * 100))
((deviation < 0)) && deviation=$((-deviation))
if ((total_endpoints > 0)) &&
  ((deviation <= 15 * same_n * total_endpoints)) &&
  (((cross_lost * same_n - same_lost * cross_n) * 100 >= 15 * cross_n * same_n)); then
  separated=1
fi
if ((cross_lost * 10 >= cross_n * 9 && separated == 1)); then
  conclude 'FAULT-PERSISTS' "${cross_lost} of ${cross_n} cross-node ExternalAuth requests were lost (same-node: ${same_lost} of ${same_n}) while the control route answered every time"
fi
inconclusive "${cross_lost} of ${cross_n} cross-node and ${same_lost} of ${same_n} same-node requests were lost, which does not match the #2284 pattern (near-total cross-node loss, with same-node loss close to the remote-endpoint share)"
