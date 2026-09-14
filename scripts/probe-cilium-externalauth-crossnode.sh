#!/usr/bin/env bash
# Measure whether the Gateway API ExternalAuth subrequest crosses nodes on the deployed Cilium.
#
# THE QUESTION (devantler-tech/platform#2284, #3784). On Cilium 1.20.0-pre.3 the gateway Envoy's
# ext_authz subrequest to oauth2-proxy was black-holed whenever it crossed nodes. The ipcache read
# (diagnose-cilium-ext-authz-ipcache.sh) cannot settle it while node encryption is off, so this
# script sends real requests through a test-only route and observes whether they arrive.
#
# WHY THE PATH IS KNOWN, NOT GUESSED. Cilium runs one Envoy per node, and pod socket load balancing
# is off, so a pod's connection to the gateway Service is handled by the Envoy on the POD'S OWN node.
# That Envoy makes the ExternalAuth call to an oauth2-proxy endpoint. So:
#   * a client pinned to a node hosting NO oauth2-proxy endpoint forces every call to cross nodes;
#   * a client pinned to a node hosting one is the same-node control (Envoy does not prefer local
#     endpoints, so it is a mix — enough to prove ext_authz works at all).
# Placement is read from the oauth2-proxy Service's EndpointSlices — what Envoy actually targets,
# addresses included — before and after the run, together with the node set; any change is
# INCONCLUSIVE. The Cilium and cilium-envoy DaemonSets must also be fully rolled out and unchanged
# across the run: after a failed deploy releases the lock, old and new datapath pods can run side by
# side, and a verdict taken across them would describe no single deployed revision.
#
# WHICH ENVOY ANSWERED IS OBSERVED, NOT ASSUMED. The control route's backend (whoami) echoes the
# connection's source address, and Cilium's Envoy connects to backends from its own node's ingress
# IP (CiliumNode spec.ingress.ipv4). Every control answer must carry the ingress IP of the client's
# own node, or the run is INCONCLUSIVE — so if that path assumption ever stops holding, the probe
# reports nothing rather than a wrong verdict. The ExternalAuth requests alternate with the control
# requests over the same Service from the same pod.
#
# WHY A FAILURE IS ATTRIBUTABLE. Requests stay inside the cluster, over plain HTTP on the gateway's
# `http` listener, to hostnames under `externalauth-probe.invalid`. Nothing in the path is
# Cloudflare, public DNS or TLS, and external-dns publishes nothing outside the zone. Each client
# alternates two routes with the SAME backend: a plain control route and the ExternalAuth route. If
# the control route does not answer every time, the path or policy is broken and the run is
# INCONCLUSIVE rather than blamed on ext_authz.
#
#   delivered  a 302/303 whose Location is the Dex login (only oauth2-proxy produces it), or a 401
#   lost       no response within the timeout, a 502/503/504, or an EMPTY 403 (Envoy's ext_authz
#              error) — the shapes a subrequest that never arrived takes
#   other      anything else (a 200 means the filter is not applied; a 404 or 301 means the route is
#              not programmed; another 5xx came from a backend that DID answer) — any occurrence
#              makes the run INCONCLUSIVE, and the distinct status codes are printed
#
# Warm-up waits until the control route answers 200 and the ExternalAuth route answers anything but
# the unprogrammed-route shapes (404, 301). A timeout counts as programmed there, so a COMPLETE
# black-hole still reaches the measurement instead of reading as a broken path.
#
# THE VERDICT (at least 10 requests per client; fewer is a smoke run and always INCONCLUSIVE)
#   FIXED           control routes answered every time, the same-node client was delivered at least
#                   once, and EVERY cross-node request was delivered.
#   FAULT-PERSISTS  control routes answered every time, the same-node client was delivered at least
#                   once, at least 90% of cross-node requests were lost, and more cross-node than
#                   same-node requests were lost — the #2284 pattern, not an intermittent blip.
#   INCONCLUSIVE    anything else, including intermittent loss below that bar.
#
# GUARD. The ExternalAuth route shares the `http` listener with the platform's HTTP→HTTPS redirect.
# Each client finishes with one request to an unrouted hostname, which must still get that redirect
# (301); anything else means the probe changed another route's behaviour, and the run is
# INCONCLUSIVE. This detects the exposure; it cannot prevent it for the run's duration.
#
# WHAT IT WRITES — stated plainly, because unlike the ipcache read this changes production while it
# runs. Everything carries the label `platform.devantler.tech/externalauth-probe=<run-id>`:
#   * in `whoami`: two HTTPRoutes (control, ExternalAuth), one CiliumNetworkPolicy letting the two
#     probe pods reach the gateway Service, and two pods;
#   * in `oauth2-proxy`: one ReferenceGrant letting the ExternalAuth route name the oauth2-proxy
#     Service.
# No existing object is modified. EXIT, INT and TERM all delete everything carrying the run's label.
# If a previous run left labelled objects behind, this run refuses before creating anything.
#
# WHAT IT PRINTS. The workflow log of a public repository is public, so no address, node name, UID
# or redirect URL is printed — only counts, status codes and the verdict. kubectl's stderr is
# discarded for the same reason.
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
readonly probe_label='platform.devantler.tech/externalauth-probe'
readonly control_host='control.externalauth-probe.invalid'
readonly authz_host='authz.externalauth-probe.invalid'
readonly guard_host='unrouted.externalauth-probe.invalid'
readonly gateway_url='http://cilium-gateway-platform.kube-system.svc.cluster.local/'
# The same pinned image the coroot heartbeat CronJob runs; it is not first-party, so no Talos image
# verification rule matches it.
readonly client_image='docker.io/curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777'
# Each pod sends 2 requests per round (control + ExternalAuth), each bounded by the timeout, so its
# request phase is at most 2 * requests * timeout. The cap keeps the whole run, including warm-up
# and scheduling, inside the workflow's job timeout (pinned by the test).
readonly max_request_seconds=250
readonly min_conclusive_requests=10
readonly warmup_attempts=12
# Wall-clock bound on waiting for route acceptance, so slow API calls cannot stretch it (the test
# shortens it; production keeps the default).
readonly route_wait_seconds="${PROBE_ROUTE_WAIT_SECONDS:-120}"
readonly poll_seconds="${PROBE_POLL_SECONDS:-5}"

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

# Worst case per pod: every warm-up attempt makes two timed-out requests and sleeps a second, then
# every round times out twice, then the guard request; plus scheduling and image pull.
readonly pod_deadline_seconds=$(((2 * requests + 2 * warmup_attempts + 1) * timeout + warmup_attempts + 60))
# One deadline for waiting on BOTH pods, measured in wall-clock seconds.
readonly pod_wait_seconds=$((pod_deadline_seconds + 180))
readonly cross_pod="externalauth-probe-cross-${run_id}"
readonly same_pod="externalauth-probe-same-${run_id}"
readonly control_route="externalauth-probe-control-${run_id}"
readonly authz_route="externalauth-probe-authz-${run_id}"
readonly probe_policy="externalauth-probe-client-${run_id}"
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

is_k8s_name() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]
}

# Placement from the Service's EndpointSlices: every endpoint must be ready, not terminating, and
# carry a node and a target UID. Emits "FP <fingerprint>" then one "NODE <name>" per endpoint.
read_endpoints() {
  local json
  json="$(kc -n "${oauth2_namespace}" get endpointslices -l "kubernetes.io/service-name=${oauth2_service}" -o json)" || return 1
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

# A node can run a probe pod when it is Ready and carries no NoSchedule/NoExecute taint. Emits
# "FP <fingerprint>" then "SCHED <name> <hostname-label>" per schedulable node, sorted by name.
read_nodes() {
  local json
  json="$(kc get nodes -o json)" || return 1
  jq -r '
    def ready: ([.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]);
    def schedulable: (.spec.unschedulable != true)
      and ([.spec.taints[]? | select(.effect == "NoSchedule" or .effect == "NoExecute")] | length == 0);
    if ([.items[]?] | length) == 0 then "ERR none"
    elif any(.items[]?; (.metadata.name // "") == "" or (.metadata.uid // "") == "") then "ERR identity"
    else "FP " + ([.items[] | .metadata.name + "|" + .metadata.uid] | sort | join(";")),
      ([.items[] | select(ready and schedulable)
        | {name: .metadata.name, host: (.metadata.labels["kubernetes.io/hostname"] // "")}]
       | sort_by(.name) | .[] | "SCHED " + .name + " " + .host)
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

# ---------------------------------------------------------------------------
# 1. Topology before: settled oauth2-proxy endpoints, and the nodes a probe pod may run on.
# ---------------------------------------------------------------------------
endpoint_lines="$(read_endpoints)" || inconclusive 'could not read the oauth2-proxy endpoints'
case "${endpoint_lines}" in
  'ERR none') inconclusive 'the oauth2-proxy Service has no endpoints' ;;
  'ERR unsettled') inconclusive 'an oauth2-proxy endpoint is not settled (rollout in flight)' ;;
esac
endpoint_fp_before="$(sed -n 's/^FP //p' <<<"${endpoint_lines}")"
endpoint_nodes="$(sed -n 's/^NODE //p' <<<"${endpoint_lines}" | sort -u)"

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
if [[ -z "${endpoint_fp_before}" || -z "${node_fp_before}" || -z "${datapath_fp_before}" ]]; then
  inconclusive 'the topology reads did not parse'
fi

cross_node=''
cross_host=''
same_node=''
same_host=''
while read -r node host; do
  [[ -z "${node}" ]] && continue
  is_k8s_name "${node}" || inconclusive 'a node reported a malformed name'
  # The pod is pinned by this label, so it must be present and well-formed, or the pod would sit
  # Pending until the deadline.
  if [[ -z "${host:-}" ]] || ! is_k8s_name "${host}"; then
    continue
  fi
  if grep -Fxq -- "${node}" <<<"${endpoint_nodes}"; then
    if [[ -z "${same_node}" ]]; then
      same_node="${node}"
      same_host="${host}"
    fi
  elif [[ -z "${cross_node}" ]]; then
    cross_node="${node}"
    cross_host="${host}"
  fi
done < <(sed -n 's/^SCHED //p' <<<"${node_lines}")

[[ -n "${cross_node}" ]] || inconclusive 'no schedulable node without an oauth2-proxy endpoint exists, so no cross-node path can be forced'
[[ -n "${same_node}" ]] || inconclusive 'no schedulable node hosts an oauth2-proxy endpoint, so there is no same-node control'

ingress_lines="$(read_ingress_ips)" || inconclusive 'could not read the per-node Cilium ingress IPs'
case "${ingress_lines}" in
  'ERR none' | 'ERR incomplete') inconclusive 'a node has no Cilium ingress IP, so the Envoy that answers a request cannot be attributed' ;;
  'ERR duplicate') inconclusive 'two nodes report the same Cilium ingress IP' ;;
esac
ingress_fp_before="$(sed -n 's/^FP //p' <<<"${ingress_lines}")"
cross_ingress="$(awk -v n="${cross_node}" '$1 == "ING" && $2 == n { print $3 }' <<<"${ingress_lines}")"
same_ingress="$(awk -v n="${same_node}" '$1 == "ING" && $2 == n { print $3 }' <<<"${ingress_lines}")"
readonly ipv4_re='^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
if [[ -z "${ingress_fp_before}" || ! "${cross_ingress}" =~ ${ipv4_re} || ! "${same_ingress}" =~ ${ipv4_re} ]]; then
  inconclusive 'the chosen nodes have no well-formed Cilium ingress IP'
fi

printf 'oauth2-proxy endpoints on %s node(s); schedulable nodes: %s\n' \
  "$(grep -c . <<<"${endpoint_nodes}" || true)" "$(grep -c '^SCHED ' <<<"${node_lines}" || true)"

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
  kc -n "${probe_namespace}" delete pods,httproutes,ciliumnetworkpolicies -l "${probe_label}=${run_id}" --ignore-not-found --wait=true --timeout=25s >/dev/null || failed=1
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
      sectionName: http
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

pod_manifest() {
  local name="$1" host="$2" role="$3"
  cat <<YAML
---
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
    platform.devantler.tech/externalauth-probe-role: ${role}
spec:
  restartPolicy: Never
  activeDeadlineSeconds: ${pod_deadline_seconds}
  # The client only runs curl, so a short grace period keeps cleanup's bounded delete wait sufficient
  # even when a pod is still running at exit.
  terminationGracePeriodSeconds: 5
  automountServiceAccountToken: false
  enableServiceLinks: false
  hostUsers: false
  nodeSelector:
    kubernetes.io/hostname: ${host}
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: ${client_image}
      env:
        - name: REQUESTS
          value: "${requests}"
        - name: TIMEOUT
          value: "${timeout}"
        - name: WARMUP_ATTEMPTS
          value: "${warmup_attempts}"
        - name: CONTROL_HOST
          value: ${control_host}
        - name: AUTHZ_HOST
          value: ${authz_host}
        - name: GUARD_HOST
          value: ${guard_host}
        - name: GATEWAY_URL
          value: ${gateway_url}
      command:
        - /bin/sh
        - -c
        - |
          set -u
          ask() {
            curl -s -o /dev/null --max-time "\$TIMEOUT" -H "Host: \$1" -w '%{http_code} %{size_download} %{redirect_url}' "\$GATEWAY_URL" 2>/dev/null
          }
          # The control route's whoami backend echoes the connection's source address, which is the
          # ingress IP of the node whose Envoy handled the request. Prints "<code> <size> <ip-or->".
          ask_control() {
            out=\$(curl -s --max-time "\$TIMEOUT" -H "Host: \$1" -w '\\nPROBE-STATUS %{http_code} %{size_download}' "\$GATEWAY_URL" 2>/dev/null)
            status=\$(printf '%s\\n' "\$out" | sed -n 's/^PROBE-STATUS //p' | tail -n 1)
            ip=\$(printf '%s\\n' "\$out" | sed -n 's/^RemoteAddr: *\\[\\{0,1\\}\\([0-9A-Fa-f.:]*\\)\\]\\{0,1\\}:[0-9]*\$/\\1/p' | head -n 1)
            printf '%s %s' "\${status:-000 0}" "\${ip:--}"
          }
          # Warm-up: this node's Envoy must serve the control route (200). The control route was
          # created only after the ExternalAuth route was accepted, and Envoy applies the gateway's
          # configuration in order, so that 200 proves the ExternalAuth route is programmed here too.
          # As a second check the ExternalAuth route must not answer like an unprogrammed hostname
          # (404, or the listener's 301 redirect). A timeout there is NOT unprogrammed — it is what
          # a black-holed check looks like — so it ends the warm-up and is measured below.
          warm=0
          n=0
          while [ "\$n" -lt "\$WARMUP_ATTEMPTS" ]; do
            n=\$((n + 1))
            c=\$(ask_control "\$CONTROL_HOST" | cut -d' ' -f1)
            a=\$(ask "\$AUTHZ_HOST" | cut -d' ' -f1)
            if [ "\$c" = 200 ] && [ -n "\$a" ] && [ "\$a" != 404 ] && [ "\$a" != 301 ]; then
              warm=1
              break
            fi
            sleep 1
          done
          if [ "\$warm" -ne 1 ]; then
            printf 'PROBE-WARMUP failed\\n'
            exit 0
          fi
          printf 'PROBE-WARMUP ok\\n'
          i=0
          while [ "\$i" -lt "\$REQUESTS" ]; do
            i=\$((i + 1))
            printf 'PROBE control %s\\n' "\$(ask_control "\$CONTROL_HOST" || true)"
            printf 'PROBE authz %s\\n' "\$(ask "\$AUTHZ_HOST" || true)"
          done
          printf 'PROBE guard %s\\n' "\$(ask "\$GUARD_HOST" || true)"
          printf 'PROBE-DONE %s\\n' "\$i"
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        capabilities:
          drop:
            - ALL
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 100m
          memory: 64Mi
YAML
}

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
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: ${probe_policy}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  endpointSelector:
    matchLabels:
      ${probe_label}: "${run_id}"
  egress:
    - toServices:
        - k8sService:
            serviceName: cilium-gateway-platform
            namespace: kube-system
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
    - toEntities:
        - ingress
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
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

# ORDER MATTERS, because it is what makes the pods' warm-up sound. Both routes are compiled into the
# gateway's single CiliumEnvoyConfig, and each Envoy applies its versions in order. The ExternalAuth
# route is created FIRST and the control route only once the ExternalAuth route is accepted, so any
# Envoy that already serves the control route (200) has also applied the ExternalAuth route. A
# failure on the ExternalAuth route after warm-up is therefore a measurement, never an Envoy whose
# configuration is still converging.
if ! kc apply -f - <<<"${manifest}" >/dev/null; then
  inconclusive 'could not create the ExternalAuth probe route, grant and policy'
fi
wait_routes "${authz_route}" ||
  inconclusive 'the ExternalAuth probe route was not accepted with resolved references in time'
if ! kc apply -f - <<<"$(route_manifest "${control_route}" "${control_host}" '')" >/dev/null; then
  inconclusive 'could not create the control probe route'
fi
wait_routes "${authz_route}" "${control_route}" ||
  inconclusive 'the control probe route was not accepted with resolved references in time'

pods_manifest="$(
  pod_manifest "${cross_pod}" "${cross_host}" cross
  pod_manifest "${same_pod}" "${same_host}" same
)"
if ! kc apply -f - <<<"${pods_manifest}" >/dev/null; then
  inconclusive 'could not create the probe pods'
fi

# ---------------------------------------------------------------------------
# 4. Wait for BOTH clients under one wall-clock deadline, and check each ran where it was pinned.
# ---------------------------------------------------------------------------
# Prints "done", "pending", "elsewhere" or "failed" for one pod.
pod_state() {
  local name="$1" node="$2" json phase actual
  json="$(kc -n "${probe_namespace}" get pod "${name}" -o json)" || {
    printf 'pending'
    return 0
  }
  phase="$(jq -r '.status.phase // ""' <<<"${json}" 2>/dev/null || true)"
  actual="$(jq -r '.spec.nodeName // ""' <<<"${json}" 2>/dev/null || true)"
  case "${phase}" in
    Succeeded)
      if [[ "${actual}" == "${node}" ]]; then printf 'done'; else printf 'elsewhere'; fi
      ;;
    Failed) printf 'failed' ;;
    *) printf 'pending' ;;
  esac
}

readonly wait_deadline=$((SECONDS + pod_wait_seconds))
while :; do
  cross_state="$(pod_state "${cross_pod}" "${cross_node}")"
  same_state="$(pod_state "${same_pod}" "${same_node}")"
  case "${cross_state} ${same_state}" in
    *elsewhere*) inconclusive 'a probe pod ran on a node other than the one it was pinned to' ;;
    *failed*) inconclusive 'a probe pod did not complete (refused, or past its deadline)' ;;
    'done done') break ;;
  esac
  if ((SECONDS >= wait_deadline)); then
    inconclusive 'a probe pod did not complete in time (not scheduled, or still running)'
  fi
  sleep "${poll_seconds}"
done

# ---------------------------------------------------------------------------
# 5. Classify each client's answers.
# ---------------------------------------------------------------------------
# Prints: control_ok control_bad delivered lost other guard_code other_codes control_on_node
# ($2 is the ingress IP of the node the pod was pinned to; a control answer counts as on-node only
# when whoami saw exactly that source address.)
classify() {
  local log="$1" want_ip="$2"
  awk -v want="${requests}" -v want_ip="${want_ip}" '
    $1 == "PROBE-WARMUP" { warm = $2 }
    $1 == "PROBE-DONE" { done = $2 }
    $1 == "PROBE" && $2 == "control" {
      nc++
      if ($3 == "200") ok++; else bad++
      if (want_ip != "" && $5 == want_ip) onnode++
    }
    $1 == "PROBE" && $2 == "guard" { guard = ($3 == "" ? "none" : $3); ng++ }
    $1 == "PROBE" && $2 == "authz" {
      na++
      code = $3; size = $4; loc = $5
      if ((code == "302" || code == "303") && index(loc, "https://dex.") == 1) delivered++
      else if (code == "401") delivered++
      else if (code == "000" || code == "" || code == "502" || code == "503" || code == "504" || (code == "403" && size == "0")) lost++
      else {
        other++
        c = (code == "" ? "none" : code)
        if (!(c in seen)) { seen[c] = 1; codes = (codes == "" ? c : codes "," c) }
      }
    }
    END {
      if (warm != "ok") { print "ERR warmup"; exit }
      if (done != want || nc != want || na != want || ng != 1) { print "ERR incomplete"; exit }
      printf "%d %d %d %d %d %s %s %d\n", ok, bad, delivered, lost, other, guard, (codes == "" ? "-" : codes), onnode
    }
  ' <<<"${log}"
}

results=''
for role in cross same; do
  if [[ "${role}" == 'cross' ]]; then
    name="${cross_pod}"
    role_ingress="${cross_ingress}"
  else
    name="${same_pod}"
    role_ingress="${same_ingress}"
  fi
  log="$(kc -n "${probe_namespace}" logs "${name}")" || inconclusive 'could not read a probe pod log'
  counts="$(classify "${log}" "${role_ingress}")" || inconclusive 'a probe pod log could not be classified'
  case "${counts}" in
    'ERR warmup') inconclusive 'a probe pod could not reach both routes during warm-up, so the path or policy is broken' ;;
    'ERR incomplete') inconclusive 'a probe pod log did not contain every expected answer' ;;
    '') inconclusive 'a probe pod log could not be classified' ;;
  esac
  results="${results}${role} ${counts}"$'\n'
done

# ---------------------------------------------------------------------------
# 6. Topology after: the placement the verdict relies on must not have changed.
# ---------------------------------------------------------------------------
endpoint_lines_after="$(read_endpoints)" || inconclusive 'could not re-read the oauth2-proxy endpoints'
[[ "$(sed -n 's/^FP //p' <<<"${endpoint_lines_after}")" == "${endpoint_fp_before}" ]] ||
  inconclusive 'the oauth2-proxy endpoints changed during the run'
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
read -r _ cross_ok cross_bad cross_delivered cross_lost cross_other cross_guard cross_codes cross_onnode <<<"$(grep '^cross ' <<<"${results}")"
read -r _ same_ok same_bad same_delivered same_lost same_other same_guard same_codes same_onnode <<<"$(grep '^same ' <<<"${results}")"

printf 'cross-node client: control %s ok / %s failed; ExternalAuth %s delivered / %s lost / %s other\n' \
  "${cross_ok}" "${cross_bad}" "${cross_delivered}" "${cross_lost}" "${cross_other}"
printf 'same-node client:  control %s ok / %s failed; ExternalAuth %s delivered / %s lost / %s other\n' \
  "${same_ok}" "${same_bad}" "${same_delivered}" "${same_lost}" "${same_other}"
summary "| client | control ok | control failed | delivered | lost | other |"
summary "| --- | --- | --- | --- | --- | --- |"
summary "| cross-node | ${cross_ok} | ${cross_bad} | ${cross_delivered} | ${cross_lost} | ${cross_other} |"
summary "| same-node | ${same_ok} | ${same_bad} | ${same_delivered} | ${same_lost} | ${same_other} |"
summary ""

if [[ "${cross_guard}" != '301' || "${same_guard}" != '301' ]]; then
  inconclusive "the http listener's redirect answered ${cross_guard}/${same_guard} instead of 301 during the run, so the probe route affected other traffic"
fi
if ((cross_bad + same_bad > 0)); then
  inconclusive 'the plain control route did not answer every time, so failures cannot be attributed to ExternalAuth'
fi
if ((cross_onnode != requests || same_onnode != requests)); then
  inconclusive "not every control request was answered by the Envoy on the client's own node (cross ${cross_onnode}/${requests}, same ${same_onnode}/${requests}), so the path each client exercised is unproven"
fi
if ((cross_other + same_other > 0)); then
  inconclusive "the ExternalAuth route returned answers that are neither delivered nor lost (status codes: cross ${cross_codes}, same ${same_codes})"
fi
if ((requests < min_conclusive_requests)); then
  inconclusive "a run of fewer than ${min_conclusive_requests} requests per client is a smoke run and never conclusive"
fi
if ((same_delivered == 0)); then
  inconclusive 'no ExternalAuth request was delivered even from a node with a local endpoint, so the route or backend is broken'
fi
if ((cross_lost == 0)); then
  if ((same_lost > 0)); then
    inconclusive 'cross-node requests were all delivered but same-node requests were lost, which is not the #2284 pattern'
  fi
  conclude 'FIXED' "every cross-node ExternalAuth request (${cross_delivered}) was delivered"
fi
# Separation. The same-node client shares its node with some endpoints, so under the #2284 fault only
# its calls to REMOTE endpoints are lost: its expected loss is the remote share of the endpoints.
# Node-independent loss hits both clients alike, so FAULT-PERSISTS also needs the same-node loss to
# stay within 15 points of that expectation ON EITHER SIDE (far below it means this node's calls to
# remote endpoints are being delivered, which contradicts a cross-node black-hole) and at least 15
# points below the cross-node loss.
endpoint_node_list="$(sed -n 's/^NODE //p' <<<"${endpoint_lines}")"
total_endpoints="$(grep -c . <<<"${endpoint_node_list}" || true)"
local_endpoints="$(grep -Fxc -- "${same_node}" <<<"${endpoint_node_list}" || true)"
remote_endpoints=$((total_endpoints - local_endpoints))
separated=0
if ((total_endpoints > 0)) &&
  ((same_lost * 100 * total_endpoints <= requests * (100 * remote_endpoints + 15 * total_endpoints))) &&
  ((same_lost * 100 * total_endpoints >= requests * (100 * remote_endpoints - 15 * total_endpoints))) &&
  (((cross_lost - same_lost) * 100 >= requests * 15)); then
  separated=1
fi
if ((cross_lost * 10 >= requests * 9 && separated == 1)); then
  conclude 'FAULT-PERSISTS' "${cross_lost} of ${requests} cross-node ExternalAuth requests were lost (same-node: ${same_lost}) while the control route answered every time"
fi
inconclusive "${cross_lost} of ${requests} cross-node and ${same_lost} same-node requests were lost, which does not match the #2284 pattern (near-total cross-node loss, with same-node loss close to the remote-endpoint share)"
