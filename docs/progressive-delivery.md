# Progressive delivery (Flagger + Gateway API)

Flagger is the platform's standard **progressive-delivery** controller. Instead of
a plain `RollingUpdate` — where a bad release reaches 100% of traffic before
anyone notices — an onboarded app is rolled out as a **canary**: Flagger shifts a
small, increasing slice of traffic to the new version, checks request
success-rate and latency at each step, and **automatically rolls back** if the new
version misbehaves.

This follows the upstream
[Flagger Gateway API tutorial](https://docs.flagger.app/tutorials/gatewayapi-progressive-delivery),
adapted to this platform's Cilium Gateway API + Coroot stack.

> **Status: foundation only.** The controller, load-tester and metric templates
> are deployed, but **no app is onboarded to a `Canary` yet**. This document is
> both the design note and the onboarding recipe.

## How it works

```
                 ┌─────────────────────────── Flagger ───────────────────────────┐
   git push ▶ Flux ▶ Deployment <app>      (1) detect change, clone to <app>-primary
                          │                 (2) create <app>-primary / <app>-canary Services
                          ▼                 (3) own the HTTPRoute, split weights
              Cilium Gateway (platform)  ◀──(4) shift 10% → 20% … → 50% to canary
                          │                 (5) query SLOs each step; promote or roll back
                          ▼
            ┌─────────────┴─────────────┐
       <app>-primary               <app>-canary
            │                            │
            └──────── Coroot eBPF node-agent (server-side HTTP metrics) ──────────┐
                                                                                  ▼
                                                            coroot-prometheus:9090
                                                                                  ▲
                          Flagger MetricTemplate queries ────────────────────────┘
```

**Traffic shifting.** Flagger owns each onboarded app's `HTTPRoute` and rewrites
the `backendRefs` weights between `<app>-primary` and `<app>-canary`. Cilium's
Gateway API implementation honours `backendRef.weight`, so no service mesh is
required.

**Metrics (SLO gating).** There is no Istio/Envoy telemetry and no app
instrumentation here, so Flagger reuses **Coroot's bundled Prometheus** — the same
endpoint OpenCost queries (`coroot-prometheus.coroot.svc.cluster.local:9090`).
Coroot's eBPF node-agent already exports **server-side** HTTP metrics for every
container:

| Metric | Type | Used for |
| --- | --- | --- |
| `container_http_inbound_requests_total{status}` | counter | request success-rate (non-5xx ÷ total) |
| `container_http_inbound_requests_duration_seconds_total{le}` | histogram | p99 latency |

Workload identity is encoded in the `container_id` label
(`/k8s/<namespace>/<pod>/<container>`), so the templates target the canary's
primary pods with `container_id=~"/k8s/{{ namespace }}/{{ target }}-primary-.*"`.

**Load & smoke tests.** `flagger-loadtester` is the webhook target Flagger calls
during analysis to run an acceptance (smoke) test before shifting traffic and to
generate load so Coroot has requests to measure.

## What's deployed (foundation)

| Component | Layer | Path |
| --- | --- | --- |
| `flagger` controller (`meshProvider: gatewayapi:v1`) | infrastructure-controllers | [`k8s/bases/infrastructure/controllers/flagger/`](../k8s/bases/infrastructure/controllers/flagger) |
| `flagger-loadtester` | infrastructure-controllers | same dir |
| `coroot-request-success-rate` / `coroot-request-duration` `MetricTemplate`s | infrastructure | [`k8s/bases/infrastructure/flagger/`](../k8s/bases/infrastructure/flagger) |

> **Why two layers?** The Flagger HelmRelease ships the `Canary` / `MetricTemplate`
> CRDs. A CR of those kinds in the *same* Flux Kustomization as the HelmRelease
> fails the kustomize-controller's server-side dry-run (`no matches for kind`) and
> deadlocks the whole set. The `MetricTemplate` CRs therefore live in the
> `infrastructure` layer, which `dependsOn` (and `wait:true`s)
> `infrastructure-controllers` — the same split as
> [`infrastructure/coroot/coroot.yaml`](../k8s/bases/infrastructure/coroot/coroot.yaml).

### Before onboarding the first app — validate the metric queries

The `MetricTemplate` PromQL is written against Coroot's documented schema but has
**not** been verified against live data. Once the stack is reconciled, port-forward
or curl `coroot-prometheus` and confirm:

1. `container_http_inbound_requests_total` exists and its `status` label holds the
   numeric HTTP code (so `status=~"5.."` selects 5xx). If Coroot uses a status
   *class*, adjust the selector.
2. The `container_id` label format is `/k8s/<namespace>/<pod>/<container>`.
3. The latency histogram bucket series name and `le` label match
   `metric-template-request-duration.yaml`.

Tune the two templates if anything differs — the query is just a string.

## Onboarding an app to a Canary

> Pick stateless, always-on, **directly-routed** apps first. Read the
> [constraints](#per-app-constraints) below — most current apps need a routing
> change before they can be canaried.

1. **Free the HTTPRoute.** Flagger generates and owns the app's `HTTPRoute`, so
   remove the app's hand-written `httproute.yaml`. If the app is fronted by the
   KEDA HTTP add-on, also remove its `http-scaled-object.yaml` (scale-to-zero and
   Flagger traffic-splitting are mutually exclusive — the app becomes always-on,
   `min 1`).

2. **Add the `Canary`** to the app's base dir (apps layer):

   ```yaml
   apiVersion: flagger.app/v1beta1
   kind: Canary
   metadata:
     name: <app>
     namespace: <app>
   spec:
     provider: gatewayapi:v1
     targetRef:
       apiVersion: apps/v1
       kind: Deployment
       name: <app>
     # No autoscalerRef — the platform uses VPA/KEDA, not HPA.
     progressDeadlineSeconds: 600
     service:
       port: <service-port>
       targetPort: <container-port>
       hosts:
         - <app>.${domain}
       gatewayRefs:
         - name: platform
           namespace: kube-system
     analysis:
       interval: 1m
       threshold: 5        # consecutive failed checks before rollback
       maxWeight: 50
       stepWeight: 10
       metrics:
         - name: success-rate
           templateRef:
             name: coroot-request-success-rate
             namespace: flagger-system
           thresholdRange:
             min: 99       # %
           interval: 1m
         - name: latency-p99
           templateRef:
             name: coroot-request-duration
             namespace: flagger-system
           thresholdRange:
             max: 500      # ms
           interval: 1m
       webhooks:
         - name: acceptance-test
           type: pre-rollout
           url: http://flagger-loadtester.flagger-system/
           timeout: 30s
           metadata:
             type: bash
             cmd: "curl -sf http://<app>-canary.<app>:<service-port>/"
         - name: load-test
           type: rollout
           url: http://flagger-loadtester.flagger-system/
           timeout: 30s
           metadata:
             cmd: "hey -z 1m -q 10 -c 2 -host <app>.${domain} http://cilium-gateway-platform.kube-system/"
   ```

3. **Open the network policies:**
   - **App namespace:** allow ingress from the Cilium Gateway (entity `ingress`)
     to the app's `targetPort`; drop the old `from keda` rule.
   - **`flagger-system`:** extend the load-tester's egress in
     [`controllers/flagger/networkpolicy.yaml`](../k8s/bases/infrastructure/controllers/flagger/networkpolicy.yaml)
     to reach the platform Gateway in `kube-system` (`:80`/`:443`) and the app's
     `<app>-canary` Service.
   - **KEDA namespace:** if the app left the KEDA HTTP add-on, drop its
     `keda → <app>` egress rule.

4. **Homepage discovery.** The `gethomepage.dev/*` annotations lived on the old
   `HTTPRoute`. Flagger's generated route does not carry them, so re-add the app
   to the homepage dashboard another way (e.g. a static `homepage` config entry).

5. **Validate & ship:** `kubectl kustomize k8s/clusters/local/` (and `prod/`)
   must build; open a draft PR. CI's Talos+Docker system test exercises the
   reconcile.

### Per-app constraints

| App(s) | Routing today | To canary |
| --- | --- | --- |
| `whoami`, `headlamp`, `actual-budget` | KEDA HTTP add-on (scale-to-zero) | Drop the `HTTPScaledObject`; app becomes always-on. Mutually exclusive with KEDA HTTP. |
| `homepage` | oauth2-proxy fronts the route | The public route targets oauth2-proxy, not the app — needs a custom split behind the proxy; not a clean Gateway API fit. |
| `umami` | direct → `umami-umami` Service | Cleanest route fit, **but** stateful (CloudNativePG) and prod-only — two app versions share one DB; avoid schema-changing migrations mid-canary. |
| `ascoachingogvaner`, `wedding-app` | tenant OCI-sync static sites | Owned by the tenant repos; onboard from there, not here. |

## References

- [Flagger Gateway API tutorial](https://docs.flagger.app/tutorials/gatewayapi-progressive-delivery)
- [Flagger Canary spec](https://docs.flagger.app/usage/how-it-works)
- [Coroot node-agent metrics](https://docs.coroot.com/metrics/node-agent/)
