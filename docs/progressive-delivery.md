# Progressive delivery (Flagger + Gateway API)

Flagger is the platform's standard **progressive-delivery** controller. Instead of
a plain `RollingUpdate` — where a bad release reaches 100% of traffic before
anyone notices — an onboarded app is rolled out as a **canary**: Flagger shifts a
small, increasing slice of traffic to the new version (or, for blue/green,
validates it out-of-band), checks request success-rate and latency at each step,
and **automatically rolls back** if the new version misbehaves.

This follows the upstream
[Flagger Gateway API tutorial](https://docs.flagger.app/tutorials/gatewayapi-progressive-delivery)
and [KEDA ScaledObject tutorial](https://docs.flagger.app/tutorials/keda-scaledobject),
adapted to this platform's Cilium Gateway API + Coroot stack.

## How it works

Flagger watches a `Canary`, clones the target `Deployment` to `<name>-primary`,
creates `<name>-primary` / `<name>-canary` Services, and drives an analysis loop.
Promotion vs rollback is gated on:

- **SLO metrics** — there is no Istio/Envoy telemetry and no app instrumentation
  here, so the `MetricTemplate`s query **Coroot's bundled Prometheus** (the same
  endpoint OpenCost uses, `coroot-prometheus.observability.svc:9090`). Coroot's eBPF
  node-agent exports server-side `container_http_inbound_requests_total{status}`
  (a counter) plus the `container_http_inbound_requests_duration_seconds_total`
  histogram — a standard Prometheus histogram, so its queryable bucket series is
  `..._total_bucket{le}` — per container. The templates measure the
  **canary** pods — see [the canary-vs-primary note](#measuring-the-canary).
- **Webhooks** — `flagger-loadtester` runs an acceptance (smoke) test before
  traffic shifts and generates load during analysis (so Coroot has requests to
  measure). The webhooks hit the `<name>-canary` Service directly.

### Two delivery modes

| Mode | Flagger `provider` | When | Traffic |
| --- | --- | --- | --- |
| **Weighted canary** | `gatewayapi:v1` | App is routed **directly** by the Gateway | Flagger owns the `HTTPRoute` and shifts `backendRef` weights 10% → 50% |
| **Blue/green** | `kubernetes` | App is **behind oauth2-proxy** (no gateway-level split possible) | No live split; canary validated via the load-tester, then the apex Service is repointed |

## What's deployed

| Component | Layer | Path |
| --- | --- | --- |
| `flagger` controller + `flagger-loadtester` | infra-controllers | [`controllers/flagger/`](../k8s/bases/infrastructure/controllers/flagger) |
| `coroot-request-success-rate` / `coroot-request-duration` `MetricTemplate`s | infrastructure | [`infrastructure/flagger/`](../k8s/bases/infrastructure/flagger) |
| **umami** Canary (weighted) | apps | [`apps/umami/canary.yaml`](../k8s/bases/apps/umami/canary.yaml) |
| **homepage** Canary (blue/green) | apps | [`apps/homepage/canary.yaml`](../k8s/bases/apps/homepage/canary.yaml) |
| **opencost** Canary (blue/green) | infrastructure | [`infrastructure/flagger/canary-opencost.yaml`](../k8s/bases/infrastructure/flagger/canary-opencost.yaml) |

> **CRD-vs-CR layering.** The flagger HelmRelease ships the `Canary` /
> `MetricTemplate` CRDs (infra-controllers). A CR of those kinds in the *same*
> Flux Kustomization fails the server-side dry-run (`no matches for kind`) and
> deadlocks the set. So **app** Canaries live in the apps layer and the
> **opencost** Canary + the MetricTemplates live in the `infrastructure` layer
> (both depend on, and wait for, infra-controllers) — the same split as
> [`infrastructure/coroot/coroot.yaml`](../k8s/bases/infrastructure/coroot/coroot.yaml).

### Onboarded apps & status

- **umami** — weighted Gateway API canary. ⚠️ **Prod-only** (excluded from the
  docker/CI overlay, so it is **not** exercised by the system test) and stateful
  (one shared CloudNativePG DB; do not land a schema-changing upgrade as a
  canary). Its old route's HSTS header is re-added via `service.headers`; the
  `gethomepage.dev/*` tile annotations are not reproducible on a Flagger route.
- **homepage** — blue/green (it's the root dashboard behind oauth2-proxy). High
  blast radius; the primary runs 1 replica (was 2) since Flagger owns replicas.
- **opencost** — blue/green infra workload. ⚠️ Headlamp's cost plugin uses the
  `opencost:http-ui` **named** port via the apiserver proxy; `portDiscovery` may
  not preserve that name — watch the Headlamp cost panel after rollout.

### Excluded (and why)

| Workload | Reason |
| --- | --- |
| whoami, headlamp, actual-budget, fleetdm, hubble-ui | KEDA **HTTP add-on** (scale-to-zero). whoami keeps scale-to-zero by choice; headlamp = single-pod in-memory OIDC; actual-budget = single-writer file DB — none can run concurrent canary pods. |
| openbao | StatefulSet — Flagger only manages Deployments / DaemonSets. |
| coroot UI, hubble-ui | Operator-reconciled Deployments — the operator fights Flagger for ownership. |
| dex, oauth2-proxy, flux-operator | Critical SSO / GitOps — too risky to canary. |

## Onboarding a new app

1. **Pick the mode** (table above). Stateless, directly-routed apps → weighted;
   oauth2-proxy-fronted apps → blue/green.
2. **Free the route** (weighted only) — delete the app's `httproute.yaml`;
   Flagger generates the route from the Canary's `gatewayRefs` + `hosts`. Re-add
   any response-header filters via `spec.service.headers`. Blue/green keeps the
   existing route untouched.
3. **Let Flagger own replicas** — add a HelmRelease postRenderer that strips
   `/spec/replicas` from the Deployment, otherwise Flux re-applies the chart's
   replica count and fights Flagger's scale-to-zero of the canary (flapping). See
   any onboarded app's `helm-release.yaml`.
4. **Add the `Canary`** — copy umami's (weighted) or homepage's (blue/green),
   referencing the two `MetricTemplate`s and a loadtester acceptance + load-test
   webhook on `<app>-canary`.
5. **Open the netpols** — app namespace: ingress from `flagger-system` on the app
   port; `flagger-system`: load-tester egress to the app namespace+port (in
   [`controllers/flagger/networkpolicy.yaml`](../k8s/bases/infrastructure/controllers/flagger/networkpolicy.yaml)).
6. **Place the Canary** in the apps layer (app) or the `infrastructure` layer
   (infra component) — never in `infrastructure/controllers`.

### KEDA apps (documented pattern — not currently used)

Flagger's KEDA integration is for **core `keda.sh ScaledObject`**, NOT the
`http.keda.sh HTTPScaledObject` (HTTP add-on) this platform uses for scale-to-zero.
To canary a plain-ScaledObject app, reference it from the Canary and let Flagger
manage the `-primary` scaler:

```yaml
spec:
  autoscalerRef:
    apiVersion: keda.sh/v1alpha1
    kind: ScaledObject
    name: myapp-so
    primaryScalerQueries:      # rewrite each trigger query for the primary
      requests: sum(rate(container_http_inbound_requests_total{...primary...}[1m]))
```

Onboarding a HTTP-add-on app (whoami/headlamp/…) this way means **replacing its
`HTTPScaledObject` with a `ScaledObject`**, giving up HTTP cold-start
scale-to-zero — a deliberate trade not taken here.

## Measuring the canary

Flagger gates on the **canary** (`{{ target }}` = `targetRef.name`, the original
deployment), not the primary. Coroot labels metrics by pod-name `container_id`
and RE2 lacks negative lookahead, so the `MetricTemplate`s select canary pods
while excluding `<target>-primary-*` by exploiting that Kubernetes pod-template
hashes are **vowel-free** (`bcdfghjklmnpqrstvwxz2456789`): `[bcdfghjklmnpqrstvwxz2-9]+`
matches a hash but never "primary" (it has i/a).

⚠️ The PromQL is written against Coroot's documented schema but **not validated
against live data** — before trusting auto-promotion, confirm in
`coroot-prometheus` the `status` label format, the `container_id` format, and the
latency bucket series, and tune
[`infrastructure/flagger/metric-template-*.yaml`](../k8s/bases/infrastructure/flagger).

## References
- [Flagger Gateway API tutorial](https://docs.flagger.app/tutorials/gatewayapi-progressive-delivery)
- [Flagger KEDA ScaledObject tutorial](https://docs.flagger.app/tutorials/keda-scaledobject)
- [Coroot node-agent metrics](https://docs.coroot.com/metrics/node-agent/)
